package bot

import (
	"context"
	"errors"
	"time"

	"good-review-master/conversation"
	"good-review-master/logutil"
	"good-review-master/onebot"
	"good-review-master/pipeline"
	"good-review-master/workspace"
)

const (
	// enqueueTimeout HTTP 处理器最多等这么久。超过就回 503，
	// 让实现端把“下游处理不过来”显式暴露出来，而不是悄悄丢消息。
	enqueueTimeout = 2 * time.Second

	// drainPollInterval 关停排空时的轮询间隔。
	drainPollInterval = 20 * time.Millisecond
)

// Run 启动各域消费 worker，并阻塞到生命周期结束。
//
// 关闭时先停止接收新事件，给存量队列 DrainTimeout 排空；到期后关闭缓冲，
// 各 worker 退出。消息本体在补全域已同步入库，排空失败不会丢正文。
func (b *Bot) Run(ctx context.Context) {
	logutil.Info("🤖 机器人已就绪，等待 IM 事件推送")

	// worker 使用独立 ctx：应用 ctx 取消后仍允许把存量处理完（排空预算由
	// DrainTimeout 控制），而不是让在途消息半途而废。
	workerCtx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	b.orch.run(workerCtx)

	<-ctx.Done()
	logutil.Info("消息处理开始排空", "超时", b.cfg().Pipeline.DrainTimeout)
	b.drain(b.cfg().Pipeline.DrainTimeout)
	b.closeBuffers()
	b.orch.closeStages()
	b.orch.wait()
	logutil.Info("消息分发循环已退出")
}

func (b *Bot) drain(timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b.orch.idle() {
			return
		}
		time.Sleep(drainPollInterval)
	}
	logutil.Warn("关停排空超时，仍有消息未处理完",
		"pending", b.orch.pending(), "active", b.orch.active())
}

// closeBuffers 关闭各域缓冲；阻塞中的 Take 会被唤醒。
//
// 只关一次：closeOnce 同时也把 closed 抬起，入口据此开始拒收新事件。
func (b *Bot) closeBuffers() {
	b.closeOnce.Do(func() {
		b.closed.Store(true)
		b.orch.closeBuffers()
	})
}

// AcceptEvent 把一条事件交给编排层放进入前置，然后立即返回。
//
// 前置会补名、入库，再决定进哪条工作流——那一段在入站应答之后发生，
// 所以 HTTP 这边不会被补名拖慢。
func (b *Bot) AcceptEvent(ctx context.Context, account string, event onebot.Event) (bool, error) {
	if b.closed.Load() {
		return false, pipeline.ErrClosed
	}
	ws, err := b.workspaces.ResolveOrCreate(event.Platform(), event.Kind(), account)
	if err != nil {
		if errors.Is(err, workspace.ErrAccountKindRequired) {
			logutil.Warn("事件缺少 account_kind，且找不到已有工作空间，已拒绝接管",
				"platform", event.Platform(), "account", account,
				"group", event.GroupID, "user", event.UserID)
			return false, nil
		}
		logutil.Warn("无法解析事件所属工作空间，已拒绝接管",
			"platform", event.Self.Platform, "account", account,
			"group", event.GroupID, "user", event.UserID, "err", err)
		return false, nil
	}
	if ws == nil {
		logutil.Debug("工作空间未开通且不在允许名单，忽略事件",
			"platform", event.Platform(), "account", account, "group", event.GroupID)
		return false, nil
	}

	key := conversation.ChatKey(ws.ID, event.ChatID())
	dedupKey := key + "\x00" + event.MessageID
	if !b.dedup.claim(dedupKey) {
		logutil.Debug("入站去重：同一条消息已在处理中", "workspace", ws.ID, "msgID", event.MessageID)
		return false, nil
	}
	putCtx, cancel := context.WithTimeout(ctx, enqueueTimeout)
	defer cancel()
	if err := b.orch.admit(putCtx, key, event.MessageID,
		conversation.Inbound{WorkspaceID: ws.ID, Event: event}); err != nil {
		b.dedup.forget(dedupKey)
		logutil.Warn("入站缓冲已满或已关闭", "workspace", ws.ID, "msgID", event.MessageID, "err", err)
		return false, err
	}
	return true, nil
}
