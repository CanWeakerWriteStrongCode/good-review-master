package speech

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"good-review-master/config"
	"good-review-master/conversation"
	"good-review-master/logutil"
	"good-review-master/onebot"
)

// 交付：把 Reply 送出去。生成方不需要知道结果去了哪——
// 现在只有聊天渠道，以后可能是管理面板或另一个 Agent。

// Sink 一条回复的去处。先用常量，不做接口：只有一个实现时接口只是转手，
// 等第二个实现（面板 / 另一个 Agent）真出现，接口形状从两个实现里长出来才准。
type Sink int

const (
	// SinkChat 发到聊天渠道（QQ 群 / 私聊）。现在唯一的实现。
	SinkChat Sink = iota
	// SinkPanel 交到管理面板给人看。
	SinkPanel
	// SinkAgent 交给另一个 Agent 转述。
	SinkAgent
)

// String 给日志用：日志里写 "chat" 比写 "0" 好认。
func (s Sink) String() string {
	switch s {
	case SinkChat:
		return "chat"
	case SinkPanel:
		return "panel"
	case SinkAgent:
		return "agent"
	}
	return fmt.Sprintf("sink(%d)", int(s))
}

// Reply 一次生成的产物：要发什么、发给谁、以及这次用掉了哪段窗口。
//
// 只装值，不装 *Router 之类的东西：生成方交出去之后就不管了，
// 换成另一个进程去交付（以后的多 Agent）时，这个结构能直接编码走。
type Reply struct {
	// Sentences 排版后的句子；交付时按平台上限切成若干条消息。
	Sentences []string
	// LongForm 是否是知识型长篇（生成时跳过了排版）。
	LongForm bool

	// Event 触发这次回复的入站事件，交付时用它决定引用谁、@ 谁。
	Event onebot.Event
	// ChatID 发到哪个会话。
	ChatID string
	// Mention 第一条是否 @ 提问者。主动接话为 false：不该把人架出来回答。
	Mention bool

	// Anchor 这次发出去的窗口首尾。nil 表示不推进锚点——
	// 兜底话术没有对应的窗口，推进了会让下一轮以为那段前缀已经发过。
	Anchor *conversation.LLMAnchor
}

// Text 把句子拼回一段文本（日志与调试用）。
func (r *Reply) Text() string {
	if r == nil {
		return ""
	}
	return joinLines(r.Sentences)
}

// Deliver 把一条回复交到指定去处。reply 为空表示这次没有可发的东西，按成功处理。
func (r *Router) Deliver(ctx context.Context, reply *Reply, sink Sink) error {
	if reply == nil || len(reply.Sentences) == 0 {
		return nil
	}
	switch sink {
	case SinkChat:
		return r.deliverChat(ctx, reply)
	default:
		// 面板与 Agent 交接还没有消费方，明确报错而不是静默丢弃。
		return fmt.Errorf("交付渠道 %s 尚未实现", sink)
	}
}

// deliverChat 发到聊天渠道：切动作 → 查台账 → 逐条发 → 全部发完才推进锚点。
func (r *Router) deliverChat(ctx context.Context, reply *Reply) error {
	if reply == nil || len(reply.Sentences) == 0 {
		return nil
	}
	if err := r.sendReply(ctx, reply.Event, reply.ChatID, reply.Mention, reply.Sentences, r.appCfg()); err != nil {
		return err
	}
	// 保存锚点（窗口首条 + 末条 message_id）。只有整次发送完成才推进，
	// 未发出的句子不会因为锚点前移而被跳过。
	if reply.Anchor == nil {
		return nil
	}
	r.cacheStore().SetLLMAnchor(reply.ChatID, *reply.Anchor)
	if r.saveAnchor != nil {
		r.saveAnchor(reply.ChatID, *reply.Anchor)
	}
	return nil
}

// RecordOnlyReply 备用号（只记录不回复的账号）被点名时的固定回答。
// 放在这里而不是编排层：话术是发言域的东西，谁触发它不该再抄一份字符串。
const RecordOnlyReply = "我是备用号，不负责回答"

// SayFixed 直接发一句固定文本，不经过大模型。
//
// 用于「只记录不回复」的账号被点名时表明身份：群聊带上引用和 @ 提问者，
// 私聊直接回到会话。不走发送台账——这类提示重复一次无害，少了反而像掉线。
func (r *Router) SayFixed(event onebot.Event, chatID, text string) error {
	if text == "" {
		return nil
	}
	if event.IsPrivate() {
		return r.obClient.SendPrivateReplyE(event.UserID, text)
	}
	return r.obClient.SendGroupReplyE(chatID, event.MessageID, event.UserID, text)
}

// reply 回复触发消息：引用这条触发消息本身，并 @ 发送者。
// 引用的是 event.MessageID（别人 @ 我的那条），不是 event.QuotedMessageID()
// （那条是发送者自己引用的消息）。QQ 官方用 message_reference + at 段；
// 拿不到该消息的 REFIDX 时插件会退化成只 @ 不引用。
//
// 私聊没有引用/@ 语义，直接按 user_id 发文本。
// 被点名回复同时清空主动插话的"连续发言"计数：对话对象已经切回机器人。
func (r *Router) reply(event onebot.Event, chatID, text string) {
	if event.IsPrivate() {
		r.obClient.SendPrivateReply(event.UserID, text)
	} else {
		r.obClient.SendGroupReply(chatID, event.MessageID, event.UserID, text)
	}
	r.proactive.noteDirect(chatID, time.Now())
	r.recordSelfReply(event, chatID, text)
}

// selfReplySeq 进程内单调序号：出站消息没有平台 message_id（send_message 不回传），
// 用 时间戳+序号 补一个稳定唯一值，只用于缓存去重、选窗锚点和数据库主键。
var selfReplySeq atomic.Uint64

// recordSelfReply 把机器人发出的回复写回内存缓存与 state.db。
// 不记录的话，下一轮请求的聊天记录里只有用户说的话，模型不知道自己刚回过什么，
// 容易重复回答、或把已经答过的问题再答一遍。
func (r *Router) recordSelfReply(event onebot.Event, chatID, text string) {
	if strings.TrimSpace(text) == "" || r.appCfg == nil {
		return
	}
	cfg := r.appCfg()
	// 实现端会把机器人自己的消息回传成事件时，入站链路已经把它写进缓存/DB；
	// 这里再记一次就是重复的两条（消息 ID 还不一样）。
	if cfg.EchoSelfMessages {
		return
	}
	now := time.Now()
	msg := conversation.Message{
		MessageID: fmt.Sprintf("bot-%d-%d", now.UnixNano(), selfReplySeq.Add(1)),
		GroupID:   chatID,
		SelfID:    event.Self.UserID,
		UserID:    event.BotID(),
		Nickname:  cfg.BotNickname,
		Content:   text,
		Time:      now.Unix(),
	}
	// 先落库拿到行号再进内存缓存：聊天记录里的 message_id 就是这一行。
	if r.saveMessage != nil {
		msg.RowID = r.saveMessage(msg)
	}
	r.cacheStore().GetGroupCache(chatID, cfg.MaxCacheMsg).Add(msg)
}

// maxReplyMessages QQ 官方对同一条入站消息的被动回复上限：超过 5 条会被平台拒绝。
// 桥接插件会给每次 send_message 带同一个 msg_id + 递增 msg_seq，所以这里最多拆 5 条。
const maxReplyMessages = 5

// splitReplyMessages 把句子切成最多 max 条消息；超出的句子合并进最后一条，
// 保证一条回复不会被拆成平台发不出去的第 6 条。每句仍然独占一行。
func splitReplyMessages(sentences []string, max int) [][]string {
	if len(sentences) == 0 {
		return nil
	}
	if max < 1 {
		max = 1
	}
	if len(sentences) <= max {
		out := make([][]string, len(sentences))
		for i, sentence := range sentences {
			out[i] = []string{sentence}
		}
		return out
	}
	out := make([][]string, 0, max)
	for i := 0; i < max-1; i++ {
		out = append(out, sentences[i:i+1])
	}
	return append(out, sentences[max-1:])
}

// sendReply 发送排版后的句子，返回未发送成功的错误。
//
//   - cfg.ReplySendInterval<=0：不拆分，所有句子合成一条消息（旧行为）；
//   - >0：拆成最多 5 条逐条发送，每条间隔该时长。第一条沿用原来的引用/@ 语义，
//     后续为纯文本（一条回复里反复 @ 人像刷屏）。
//
// 每个动作先查发送台账：已发（含结果不确定）直接跳过，重试只补未发的。
func (r *Router) sendReply(ctx context.Context, event onebot.Event, chatID string, mention bool, sentences []string, cfg *config.Config) error {
	if len(sentences) == 0 {
		return nil
	}
	if r.ledger == nil {
		r.ledger = NewMemoryLedger()
	}
	actions := buildReplyActions(r.workspace, event, chatID, mention, sentences, cfg)
	for i, action := range actions {
		if entry, ok, err := r.ledger.Load(action.ID); err == nil && ok && entry.Sent {
			continue
		}
		if i > 0 {
			timer := time.NewTimer(cfg.ReplySendInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				logutil.Info("分条发送被取消", "group", chatID, "已发", i)
				return ctx.Err()
			case <-timer.C:
			}
		}
		entry, err := r.sendAction(ctx, action)
		r.ledger.Mark(entry)
		if entry.Sent {
			r.recordSelfReply(event, chatID, action.Text)
		}
		if err != nil {
			return err
		}
	}
	if len(actions) > 0 {
		r.proactive.noteDirect(chatID, time.Now())
	}
	logutil.Debug("分条发送完成", "group", chatID, "动作数", len(actions), "间隔", cfg.ReplySendInterval)
	return nil
}

// ReplyAction 一次回复里的一个发送动作。
// ID 由“入站消息 + 第几句”确定，重试时用它做幂等。
type ReplyAction struct {
	ID               string
	ReplyToMessageID string
	RefIdx           string
	Text             string
	ChatID           string
	UserID           string
	Private          bool
	Mention          bool
	Quote            bool // 第一条动作引用触发消息
}

// LedgerEntry 发送台账的一行。Sent=true 含“结果不确定”；
// SendOK=false 表示接口没有明确返回成功，后台需要排查。
type LedgerEntry struct {
	ActionID  string
	MessageID string
	Text      string
	Sent      bool
	SendOK    bool
}

// SendLedger 发送台账。生产实现落 state.db；未注入时用进程内 map。
type SendLedger interface {
	Load(actionID string) (LedgerEntry, bool, error)
	Mark(entry LedgerEntry)
}

// memoryLedger 进程内台账：跨重启会丢，但同一进程内的重试不会重复发送。
type memoryLedger struct {
	mu      sync.Mutex
	entries map[string]LedgerEntry
}

// NewMemoryLedger 创建进程内发送台账。
func NewMemoryLedger() SendLedger {
	return &memoryLedger{entries: make(map[string]LedgerEntry)}
}

func (l *memoryLedger) Load(actionID string) (LedgerEntry, bool, error) {
	if l == nil {
		return LedgerEntry{}, false, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[actionID]
	return entry, ok, nil
}

func (l *memoryLedger) Mark(entry LedgerEntry) {
	if l == nil || entry.ActionID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[entry.ActionID] = entry
}

// buildReplyActions 把排版后的句子转成动作列表。
// ReplySendInterval<=0 或只有一句时合成一个动作（旧行为）。
func buildReplyActions(workspace string, event onebot.Event, chatID string, mention bool, sentences []string, cfg *config.Config) []ReplyAction {
	if len(sentences) == 0 {
		return nil
	}
	chunks := splitReplyMessages(sentences, maxReplyMessages)
	if cfg.ReplySendInterval <= 0 || len(sentences) == 1 {
		chunks = [][]string{sentences}
	}
	actions := make([]ReplyAction, 0, len(chunks))
	for i, chunk := range chunks {
		actions = append(actions, ReplyAction{
			ID:               fmt.Sprintf("%s|%s|%d", workspace, event.MessageID, i),
			ReplyToMessageID: event.MessageID,
			RefIdx:           event.QQMsgIdx,
			Text:             joinLines(chunk),
			ChatID:           chatID,
			UserID:           event.UserID,
			Private:          event.IsPrivate(),
			Mention:          i == 0 && mention,
			Quote:            i == 0,
		})
	}
	return actions
}

func joinLines(lines []string) string {
	out := ""
	for i, line := range lines {
		if i > 0 {
			out += "\n"
		}
		out += line
	}
	return out
}

// sendAction 发送一个动作，返回台账条目。
// 传输错误（超时/连接中断）按“已发但未确认”处理，不重发；平台明确拒绝才算未发。
func (r *Router) sendAction(ctx context.Context, action ReplyAction) (LedgerEntry, error) {
	entry := LedgerEntry{ActionID: action.ID, MessageID: action.ReplyToMessageID, Text: action.Text}
	var err error
	switch {
	case action.Private:
		err = r.obClient.SendPrivateReplyE(action.UserID, action.Text)
	case action.Mention:
		err = r.obClient.SendGroupReplyE(action.ChatID, action.ReplyToMessageID, action.UserID, action.Text)
	case action.Quote:
		err = r.obClient.SendGroupReplyE(action.ChatID, action.ReplyToMessageID, "", action.Text)
	default:
		err = r.obClient.SendGroupMessageE(action.ChatID, action.Text)
	}
	if err == nil {
		entry.Sent = true
		entry.SendOK = true
		return entry, nil
	}
	var sendErr *onebot.SendError
	if errors.As(err, &sendErr) && sendErr.Uncertain {
		entry.Sent = true
		entry.SendOK = false
		logutil.Warn("发送结果不确定：按已发记录，不重复发送", "action", action.ID, "err", err)
		return entry, nil
	}
	entry.Sent = false
	entry.SendOK = false
	return entry, err
}
