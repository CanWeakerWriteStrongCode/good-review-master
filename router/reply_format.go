package router

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"good-review-master/config"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/onebot"
)

// 最终回复排版：模型先正常生成回复，再把它作为输入发回去，用 JSON schema
// 强制拆成"最多 N 句短句"。设计上的三个点：
//
//  1. 独立调用、不带工具：带工具的轮次里锁 JSON 会破坏 tool_calls（Qwen 的工具
//     调用走模板文本），所以排版一定发生在工具循环结束、拿到最终稿之后。
//  2. 失败一律退回原文：排版只是让回复更像真人，绝不能因为它把有效回复丢掉。
//  3. 句数上限随配置（llm.reply_max_sentences）动态生成 schema；system prompt
//     保持稳定，数字放在 user 消息里，保住前缀缓存。
//  4. 排版走 ChatStructured（= ChatJSON 同一实现），因此**关闭思考模式**
//     （enable_thinking=false）：这一步只是格式转换，不需要推理，也不该为它多花 token。
//  5. 短回复（不足 llm.reply_rewrite_min_runes 字，默认 60）不排版：本来就没多少可拆的，省一次调用。
//  6. 群聊记录随 user 消息一并附上：只给一段回复，排版器不知道"他/这个"指谁、也不清楚
//     该保留哪些信息，改写容易跑偏；记录之后才是要改写的那条回复。

// sentenceReplySchema 排版输出的 JSON Schema。strict 模式要求所有属性都进
// required、additionalProperties 必须为 false。
func sentenceReplySchema(max int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "sentences": {
      "type": "array",
      "minItems": 1,
      "maxItems": %d,
      "items": {"type": "string"}
    }
  },
  "required": ["sentences"],
  "additionalProperties": false
}`, max))
}

// rewriteReply 把最终回复改写成最多 cfg.LLMConfig.ReplyMaxSentences 句短句，返回句子数组。
// chatLog 是生成这条回复时用的同一份群聊记录（与正文同样的 JSON 行格式，可为空），
// 排版器靠它理解上下文；botName/botID 让它在记录里认出哪条是机器人自己。
// ReplyMaxSentences<=0（关闭排版）、输入为空，或不足 ReplyRewriteMinRunes 字（0=不设下限）时，
// 返回只含原文的单元素切片。
func (r *Router) rewriteReply(ctx context.Context, cfg *config.Config, draft, chatLog, botName, botID string) []string {
	maxSentences := cfg.LLMConfig.ReplyMaxSentences
	if maxSentences <= 0 || strings.TrimSpace(draft) == "" {
		return []string{draft}
	}
	if minRunes := cfg.LLMConfig.ReplyRewriteMinRunes; minRunes > 0 && len([]rune(draft)) < minRunes {
		logutil.Debug("回复较短，跳过排版", "字符数", len([]rune(draft)), "下限", minRunes)
		return []string{draft}
	}

	var b strings.Builder
	if line := identityLine(botName, botID); line != "" {
		b.WriteString(line + "\n")
	}
	b.WriteString(fmt.Sprintf(
		"【上限】最多 %d 句。句数按内容长短定：短就 1~2 句，长才多拆几句；不要为了凑句数硬拆，能短则短。\n", maxSentences))
	if chatLog = strings.TrimSpace(chatLog); chatLog != "" {
		b.WriteString("【群聊记录】\n" + chatLog + "\n")
	}
	b.WriteString("【生成的回复】这是你（机器人）刚生成的回复，结合上面的群聊记录改写它：\n" + draft)
	userText := b.String()
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: r.promptCfg.ReplyRewritePrompt()},
		{Role: llm.RoleUser, Content: userText},
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.LLMTimeout)
	defer cancel()
	resp, err := r.llmClient.ChatStructured(ctx, messages, sentenceReplySchema(maxSentences))
	if err != nil {
		logutil.Warn("回复排版失败，使用原文", "err", err, "原文字符数", len([]rune(draft)))
		return []string{draft}
	}
	sentences, err := parseSentenceReply(resp.Content)
	if err != nil {
		logutil.Warn("回复排版结果无法解析，使用原文", "err", err, "原始", truncateJudgeLog(resp.Content))
		return []string{draft}
	}
	if len(sentences) > maxSentences {
		// schema 在部分后端只是提示词约束（降级到 json_object/none 时没有 grammar），
		// 多出来的句子不丢：合并进最后一句，保证最终仍是不超过上限的句数。
		tail := strings.Join(sentences[maxSentences-1:], "")
		sentences = append(sentences[:maxSentences-1], tail)
	}
	logutil.Debug("回复已排版", "句数", len(sentences), "原文字符数", len([]rune(draft)))
	return sentences
}

// parseSentenceReply 从排版结果里取 sentences。容忍代码围栏与前后散文，
// 但要求至少有一句非空；否则返回错误，由调用方回退原文。
func parseSentenceReply(raw string) ([]string, error) {
	text := stripJSONFences(raw)
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("排版结果不是 JSON：%s", truncateJudgeLog(raw))
	}
	var payload struct {
		Sentences []string `json:"sentences"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &payload); err != nil {
		return nil, fmt.Errorf("排版结果解析失败：%w（原文 %s）", err, truncateJudgeLog(raw))
	}
	out := make([]string, 0, len(payload.Sentences))
	for _, sentence := range payload.Sentences {
		// 句子内部的换行压成空格：每句必须真的占一行，不能被内嵌换行打乱排版。
		sentence = strings.TrimSpace(strings.ReplaceAll(sentence, "\n", " "))
		if sentence != "" {
			out = append(out, sentence)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("排版结果 sentences 为空：%s", truncateJudgeLog(raw))
	}
	return out, nil
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
