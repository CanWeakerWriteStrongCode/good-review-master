package bot

import (
	"good-review-master/config"
	"good-review-master/conversation"
	"good-review-master/policy"
	"good-review-master/workspace"
)

// router 是补全之后的那一步：把一条消息分到一条工作流。
//
// 它不是一个节点——没有自己的 Buffer 和 Consumer。原因不是「它简单」，而是三条：
//
//   - 它没有自己的资源特征。补全要写库（慢、要背压、要按会话串行），
//     打标签要调视觉模型，生成要调大模型——这些才值得一个池子。
//     择路只是一次配置读取加一次前缀树匹配。
//   - 它和补全共享同一个内存值。判断依据是 PrepareBatch 刚造出来的
//     synced.Message.Content 和 ws.Router；拆成两个节点就要把整个 Synced
//     编码成 JSON 再解出来，只为读一个字段。
//   - 它没有失败模式。补全会失败（库写不进）、打标签会失败（模型超时）、
//     生成会超时；择路不会——查不到就是聊天。没有失败就没有重试、没有死信、
//     没有队列，也就不需要 Consumer。
//
// 什么时候它会变成独立节点：择路本身变贵（查库按租户取规则、走外部规则引擎、
// 调模型判断意图）。那时它有了自己的失败与延迟，才需要队列和背压。
//
// 所以它在 handleSync 的同一轮循环里被调用，只是代码上分开放：
// 「哪一段会失败、哪一段不会」读代码时就看得见。

// route 交回前置的选择：这条消息进哪条工作流、什么分区键、带什么载荷。
func (b *Bot) route(ws *workspace.Workspace, cfg *config.Config, synced conversation.Synced) Pick {
	return Pick{
		Workflow: b.workflowOf(ws, cfg, synced),
		Key:      conversation.ChatKey(synced.WorkspaceID, synced.ChatID),
		MsgID:    synced.Message.MessageID,
		Payload:  synced,
	}
}

// workflowOf 回答「这条消息进哪条工作流」，只读、无副作用，可以单独测。
//
// 管理指令走独立工作流：控制平面不占打标签与生成的并发槽，也不进群聊缓存
// （模型看不到「刚才有人切了人格」，已确认可接受）。
// record-only 的指令不转出：备用号只记录，交回聊天工作流由生成域回固定话术。
func (b *Bot) workflowOf(ws *workspace.Workspace, cfg *config.Config, synced conversation.Synced) string {
	if b.isInternalCommand(ws, cfg, synced) && !policy.IsRecordOnly(cfg, ws.ID) {
		return WorkflowCommand
	}
	return WorkflowChat
}

// isInternalCommand 只做只读匹配：命中就说明这条消息该走指令工作流。
// 执行前的第二次匹配在 speech.RunInternalCommand 里，两处不能合并。
func (b *Bot) isInternalCommand(ws *workspace.Workspace, cfg *config.Config, synced conversation.Synced) bool {
	if ws == nil || ws.Router == nil {
		return false
	}
	_, ok := ws.Router.MatchInternalCommand(synced.Event, synced.Message.Content,
		b.allowNicknameFallback(ws, cfg.BotNickname))
	return ok
}
