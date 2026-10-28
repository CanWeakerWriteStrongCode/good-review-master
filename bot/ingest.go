package bot

import (
	"context"
	"fmt"
	"time"

	"good-review-master/logutil"
	"good-review-master/onebot"
	"good-review-master/telemetry"
)

const (
	// eventQueueSize 是入站事件队列容量。群聊消息量很低，这个值足够吸收
	// 一次重连风暴；真正的背压由 AcceptEvent 的等待超时给出。
	eventQueueSize = 1024

	// enqueueTimeout HTTP 处理器最多等这么久。超过就回 503，
	// 让实现端把"下游处理不过来"显式暴露出来，而不是悄悄丢掉消息。
	enqueueTimeout = 2 * time.Second
)

// Run 启动消息消费循环。历史由共享 state.db 按 workspace_id 提供，启动时不再回填。
func (b *Bot) Run(ctx context.Context) {
	logutil.Info("🤖 机器人已就绪，等待 IM 事件推送")

	for {
		select {
		case <-ctx.Done():
			logutil.Info("消息分发循环正常退出")
			return
		case item := <-b.queue:
			b.dispatch(ctx, item)
		}
	}
}

// AcceptEvent 把一条事件放进队列后立即返回。
//
// 不在 HTTP 处理器里直接处理：一次 @ 会触发大模型调用，让实现端等上几十秒没有任何好处。
// 工作空间的归属在这里判定：既没有目录、又不在 bot.allow_workspace_accounts 名单里时
// 返回 accepted=false，入站层回 404，实现端把消息交回自己的 Agent。
func (b *Bot) AcceptEvent(ctx context.Context, account string, event onebot.Event) (bool, error) {
	ws, err := b.workspaces.ResolveOrCreate(event.Platform(), account)
	if err != nil {
		// 平台名不合法属于请求内容错误；账号路径已在 im 层校验过。
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

	timer := time.NewTimer(enqueueTimeout)
	defer timer.Stop()

	select {
	case b.queue <- job{event: event, ws: ws}:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return false, fmt.Errorf("消息队列已满（容量 %d）", eventQueueSize)
	}
}

// dispatch 做消费侧的最后一层过滤：白名单与重复消息。
func (b *Bot) dispatch(ctx context.Context, item job) {
	event, ws := item.event, item.ws
	cfg := ws.Snapshot()()
	if !ws.AllowsGroup(event.GroupID) {
		return
	}

	gc := ws.Cache.GetGroupCache(event.GroupID, cfg.MaxCacheMsg)
	if gc.HasSeen(event.Self.UserID, event.MessageID) {
		logutil.Debug("跳过重复消息", "group", event.GroupID, "self", event.Self.UserID, "msgID", event.MessageID)
		return
	}

	if event.Time > 0 {
		telemetry.PollLagSeconds.Observe(time.Since(time.Unix(int64(event.Time), 0)).Seconds())
	}
	b.ProcessMessage(ctx, event, ws)
}
