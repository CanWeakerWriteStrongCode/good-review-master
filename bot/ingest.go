package bot

import (
	"context"
	"errors"
	"time"

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

// Run 启动三段消费 worker，并阻塞到生命周期结束。
//
// 关闭时先停止接收新事件，给存量队列 DrainTimeout 排空；到期后关闭缓冲，
// 各 worker 退出。消息本体在 Stage 1 已同步入库，排空失败不会丢正文。
func (b *Bot) Run(ctx context.Context) {
	logutil.Info("🤖 机器人已就绪，等待 IM 事件推送")

	// worker 使用独立 ctx：应用 ctx 取消后仍允许把存量处理完（排空预算由
	// DrainTimeout 控制），而不是让在途消息半途而废。
	workerCtx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	b.stage1.Run(workerCtx)
	b.stage2.Run(workerCtx)
	b.stage3.Run(workerCtx)

	<-ctx.Done()
	logutil.Info("消息处理开始排空", "超时", b.cfg().Pipeline.DrainTimeout)
	b.drain(b.cfg().Pipeline.DrainTimeout)
	b.closeBuffers()
	b.stage1.Close()
	b.stage2.Close()
	b.stage3.Close()
	b.stage1.Wait()
	b.stage2.Wait()
	b.stage3.Wait()
	logutil.Info("消息分发循环已退出")
}

func (b *Bot) drain(timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b.pending() == 0 && b.stage1.Active() == 0 && b.stage2.Active() == 0 && b.stage3.Active() == 0 {
			return
		}
		time.Sleep(drainPollInterval)
	}
	logutil.Warn("关停排空超时，仍有消息未处理完", "pending", b.pending(),
		"active", b.stage1.Active()+b.stage2.Active()+b.stage3.Active())
}

func (b *Bot) pending() int {
	return b.b1.Total() + b.b2.Total() + b.b3.Total()
}

// AcceptEvent 把一条事件放进 B1 后立即返回。
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

	key := pipeline.ChatKey(ws.ID, event.ChatID())
	dedupKey := key + "\x00" + event.MessageID
	if !b.dedup.claim(dedupKey) {
		logutil.Debug("入站去重：同一条消息已在处理中", "workspace", ws.ID, "msgID", event.MessageID)
		return false, nil
	}
	item, err := pipeline.NewItem(pipeline.StageIngest, key, event.MessageID,
		pipeline.InboundEvent{WorkspaceID: ws.ID, Event: event})
	if err != nil {
		logutil.Error("编码入站事件失败", "workspace", ws.ID, "msgID", event.MessageID, "err", err)
		return false, nil
	}

	putCtx, cancel := context.WithTimeout(ctx, enqueueTimeout)
	defer cancel()
	if err := b.b1.Put(putCtx, item); err != nil {
		b.dedup.forget(dedupKey)
		logutil.Warn("入站缓冲已满或已关闭", "workspace", ws.ID, "msgID", event.MessageID, "err", err)
		return false, err
	}
	b.stage1.Submit(key)
	return true, nil
}
