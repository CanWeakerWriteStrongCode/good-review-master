package bot

import (
	"good-review-master/config"
	"good-review-master/conversation"
	"good-review-master/workspace"
)

// router 是补全之后的那一步：把一条消息分到一条工作流。
//
// 它不是一个节点——没有自己的 Buffer 和 Consumer。原因不是「它简单」，而是三条：
//
//   - 它没有自己的资源特征。补全要写库（慢、要背压、要按会话串行），
//     指令要改会话状态，打标签要调视觉模型，生成要调大模型——这些才值得一个池子。
//     择路只是一次配置读取加一次判断。
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
// 现在只有一条：指令是聊天工作流的第一个域（见 NewBot 的连线），不再在这里分流。
// "是不是指令"这个判断下移到 command 域——它本来就要再匹配一次才能执行，
// 在这里再判一遍等于同一件事留两份实现。
//
// 参数形状故意留着（下划线命名），它是**加工作流的唯一入口**：
// 以后要多一条链，这里加一个分支 + NewBot 里 workflow/mount/connect/exit，
// 别处（前置、域、编排层）一行都不用动。
func (b *Bot) workflowOf(_ *workspace.Workspace, _ *config.Config, _ conversation.Synced) string {
	return WorkflowChat
}
