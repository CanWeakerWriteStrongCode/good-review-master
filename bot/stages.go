package bot

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"good-review-master/assets"
	"good-review-master/config"
	"good-review-master/conversation"
	"good-review-master/logutil"
	"good-review-master/pipeline"
	"good-review-master/policy"
	"good-review-master/speech"
	"good-review-master/state"
	"good-review-master/telemetry"
	"good-review-master/workspace"
)

// handleIngest 是前置：所有消息都从它过。同一会话的一批统一补名、组装并同步入库，
// 然后交回编排层——这里的回执里 Topic 是工作流名，不是去向。
//
// 它不属于任何一条工作流：加一条工作流不用动它，它也只管“这条消息走哪条”，
// 不管那条工作流里有什么。
func (b *Bot) handleIngest(ctx context.Context, _ string, items []pipeline.Item) Outcome {
	var out Outcome
	byWorkspace := make(map[string][]conversation.Inbound)
	for _, item := range items {
		in, err := pipeline.DecodeItem[conversation.Inbound](item)
		if err != nil {
			out.Failed = append(out.Failed, pipeline.FailedItem{Item: item, Err: err})
			continue
		}
		byWorkspace[in.WorkspaceID] = append(byWorkspace[in.WorkspaceID], in)
	}

	for workspaceID, inbounds := range byWorkspace {
		ws, ok := b.workspaces.ByID(workspaceID)
		if !ok {
			logutil.Debug("工作空间已不存在，跳过这一批", "workspace", workspaceID)
			continue
		}
		cfg := ws.Snapshot()()
		for _, synced := range ws.PrepareBatch(ctx, inbounds) {
			out.Routes = append(out.Routes, Route{
				Topic:   b.workflowOf(ws, cfg, synced),
				Key:     conversation.ChatKey(synced.WorkspaceID, synced.ChatID),
				MsgID:   synced.Message.MessageID,
				Payload: synced,
			})
		}
	}
	return out
}

// workflowOf 回答“这条消息进哪条工作流”，只读、无副作用，可以单独测。
//
// 管理指令走独立工作流：控制平面不占打标签与生成的并发槽，也不进群聊缓存
// （模型看不到“刚才有人切了人格”，已确认可接受）。
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

// handleCommand 是管理指令域：执行指令，不进入打标签与生成。
//
// 交回执但不重试：指令副作用不幂等，重试等于执行两次（#添加人格 会生成两个人格）。
// 失败只能在 handler 内部回一句错误，这条消息不再进重试队列。
func (b *Bot) handleCommand(ctx context.Context, _ string, items []pipeline.Item) Outcome {
	var out Outcome
	for _, item := range items {
		synced, err := pipeline.DecodeItem[conversation.Synced](item)
		if err != nil {
			logutil.Warn("指令阶段载荷解码失败，已丢弃", "item", item.ID, "err", err)
			continue
		}
		// 如实汇报"这一条经手完了"，不管下面走了哪个分支。之后还有没有下一站
		// 不在这里决定：现在 command_done 被声明为终点，要接审计域时改链即可。
		out.Routes = append(out.Routes, Route{
			Topic:   TopicCommandDone,
			Key:     conversation.ChatKey(synced.WorkspaceID, synced.ChatID),
			MsgID:   synced.Message.MessageID,
			Payload: synced,
		})
		ws, ok := b.workspaces.ByID(synced.WorkspaceID)
		if !ok || ws.Router == nil {
			continue
		}
		cfg := ws.Snapshot()()
		if !ws.Router.RunInternalCommand(ctx, synced.Event, synced.ChatID,
			synced.Message.Content, b.allowNicknameFallback(ws, cfg.BotNickname)) {
			// 分流后路由表被别的指令改掉（关键字被删了）：丢弃即可，
			// 消息正文在 Stage 1 已经入库。
			logutil.Warn("指令在分流后已匹配不上，已丢弃",
				"workspace", ws.ID, "chat", synced.ChatID,
				"message", synced.Message.MessageID, "content", synced.Message.Content)
		}
	}
	return out
}

// handleAssets 是打标签域：给一批消息打资产标签、写欠条，然后回执给编排层。
func (b *Bot) handleAssets(ctx context.Context, _ string, items []pipeline.Item) Outcome {
	var out Outcome
	tagger := b.workspaces.Tagger()
	for _, item := range items {
		synced, err := pipeline.DecodeItem[conversation.Synced](item)
		if err != nil {
			out.Failed = append(out.Failed, pipeline.FailedItem{Item: item, Err: err})
			continue
		}
		if ws, ok := b.workspaces.ByID(synced.WorkspaceID); ok {
			if tagger != nil && len(synced.Message.Images) > 0 {
				result := tagger.TagURLs(ctx, synced.Message.Images)
				synced.Message.ImageIDs = result.ImageIDs
				synced.Message.ImageNotes = result.ImageNotes
				synced.Assets = result.Assets
				synced.AssetStatus = result.Status
				if len(result.ImageIDs) > 0 && ws.State != nil {
					if err := ws.State.UpdateMessageImages(synced.ChatID, synced.Message.MessageID, result.ImageIDs); err != nil {
						logutil.Warn("回写消息图片标识失败", "err", err)
					}
				}
				b.persistDeferred(synced, result)
			}
		}
		// 回执交给编排层：它知道“打完标签”该由谁接手。
		out.Routes = append(out.Routes, Route{
			Topic:   TopicTagged,
			Key:     conversation.ChatKey(synced.WorkspaceID, synced.ChatID),
			MsgID:   synced.Message.MessageID,
			Payload: synced,
		})
	}
	return out
}

// persistDeferred 把没打完标签的资产写成欠条，只记原始地址。
// 原图不另存副本：后台补标签时直接按这个地址再看一眼。
func (b *Bot) persistDeferred(synced conversation.Synced, result assets.TagResult) {
	store := b.workspaces.StateStore()
	if store == nil || len(result.Deferred) == 0 {
		return
	}
	now := time.Now().Unix()
	for _, deferred := range result.Deferred {
		job := state.AssetTagJob{
			WorkspaceID: synced.WorkspaceID, GroupID: synced.ChatID,
			MessageID: synced.Message.MessageID, AssetIndex: deferred.Index,
			Kind: deferred.Kind, URL: deferred.URL, Status: "pending",
			LastError: deferred.Error, CreatedAt: now,
		}
		if deferred.Index < len(result.Assets) && result.Assets[deferred.Index].Status == conversation.StatusFailed {
			job.Status = "failed"
		}
		if deferred.Index < len(result.ImageNotes) {
			job.Attempts = result.ImageNotes[deferred.Index].Attempts
		}
		if err := store.SaveAssetTagJob(job); err != nil {
			logutil.Warn("写入资产欠条失败", "err", err)
		}
	}
}

// handleAgent 是生成域：提交缓存、冻结快照、同步执行一次路由/主动接话。
//
// 每条经手的消息都交一条 TopicReplied 回执，包括"判定不该回"的那些——
// 域只汇报这一条处理完了；之后还有没有事，由链的拓扑决定。
func (b *Bot) handleAgent(ctx context.Context, key string, items []pipeline.Item) Outcome {
	var out Outcome
	type proactiveTask struct {
		ws       *workspace.Workspace
		synced   conversation.Synced
		snapshot []conversation.Message
	}
	var latestProactive *proactiveTask

	for _, item := range items {
		synced, err := pipeline.DecodeItem[conversation.Synced](item)
		if err != nil {
			out.Failed = append(out.Failed, pipeline.FailedItem{Item: item, Err: err})
			continue
		}
		// 后面每个分支都是终结分支（回、不回、放弃），所以先把回执记上。
		// 真正投递发生在 handler 返回之后，不影响执行顺序。
		out.Routes = append(out.Routes, Route{
			Topic:   TopicReplied,
			Key:     key,
			MsgID:   synced.Message.MessageID,
			Payload: synced,
		})
		ws, ok := b.workspaces.ByID(synced.WorkspaceID)
		if !ok {
			continue
		}
		cfg := ws.Snapshot()()
		gc := ws.Cache.GetGroupCache(synced.ChatID, cfg.MaxCacheMsg)
		gc.Add(synced.Message)
		telemetry.CacheMessages.WithLabelValues(ws.ID, synced.ChatID).Set(float64(gc.Len()))
		if !synced.Private {
			// openid 是账号级的，私聊与群聊给的是同一个值；这里跟着群聊学，
			// 学到一次就对所有会话都算数。
			ws.LearnIdentity(synced.Event.Self.UserID)
			ws.SyncSelfName(synced.Event.BotID(), synced.ChatID)
		}

		// 只记录不回复：入库、打标签、进缓存上面都已做完，这里只处理"被点名"。
		// 不走 FindMsgIndex 之后的任何一步，所以裁判和生成都不会跑。
		if policy.IsRecordOnly(cfg, ws.ID) {
			if b.addressed(ws, cfg, synced) {
				b.answerRecordOnly(ws, synced)
			}
			continue
		}

		msgs := gc.GetAll()
		index := conversation.FindMsgIndex(msgs, synced.Message.MessageID)
		if index < 0 {
			b.recordAging(ws.ID, synced, msgs)
			continue
		}
		snapshot := msgs[:index+1]

		if b.addressed(ws, cfg, synced) {
			ws.Router.RouteMessageNow(ctx, synced.Message.Content, synced.Event, synced.ChatID, snapshot)
			continue
		}
		// 主动接话 latest-wins：同一批只看最后一条，且后面还有积压时也不判，
		// 交给下一批的最后一条。避免刷屏时每条都跑一次裁判。
		latestProactive = &proactiveTask{ws: ws, synced: synced, snapshot: snapshot}
	}
	if latestProactive != nil && b.orch.backlog(pipeline.StageAgent, key) == 0 {
		latestProactive.ws.Router.TryProactiveNow(ctx, latestProactive.synced.Event,
			latestProactive.synced.ChatID, latestProactive.synced.Message.Content, latestProactive.snapshot)
	}
	return out
}

// addressed 判断这条消息是不是冲着机器人来的。
// 判定本身在 speech.IsAddressed：同一个问题（这条是不是冲我来的）只该有一份实现，
// 这里只负责给出"昵称兜底能不能用"这个跟部署形态有关的输入。
func (b *Bot) addressed(ws *workspace.Workspace, cfg *config.Config, synced conversation.Synced) bool {
	return speech.IsAddressed(synced.Event, synced.Message.Content, cfg,
		b.allowNicknameFallback(ws, cfg.BotNickname))
}

// answerRecordOnly 备用号被点名时回一句固定话术，不调大模型、不占被动回复额度之外的东西。
func (b *Bot) answerRecordOnly(ws *workspace.Workspace, synced conversation.Synced) {
	if err := ws.Router.SayFixed(synced.Event, synced.ChatID, speech.RecordOnlyReply); err != nil {
		logutil.Warn("备用号提示发送失败", "workspace", ws.ID, "chat", synced.ChatID, "err", err)
		return
	}
	logutil.Debug("备用号已提示不负责回答", "workspace", ws.ID, "chat", synced.ChatID)
}

// recordAging 记录“触发消息已不在窗口里”：放弃这条回复任务，但保存完整现场。
func (b *Bot) recordAging(workspaceID string, synced conversation.Synced, msgs []conversation.Message) {
	first, last := "", ""
	if len(msgs) > 0 {
		first = msgs[0].MessageID
		last = msgs[len(msgs)-1].MessageID
	}
	detail := map[string]any{
		"reason":         "触发消息已不在窗口缓存里，放弃本次回复",
		"hint":           "窗口小了，要调大 llm_send_count / max_cache_msg",
		"workspace_id":   workspaceID,
		"chat_id":        synced.ChatID,
		"message_id":     synced.Message.MessageID,
		"window_first":   first,
		"window_last":    last,
		"window_count":   len(msgs),
		"llm_send_count": b.cfg().LLMSendCount,
		"max_cache_msg":  b.cfg().MaxCacheMsg,
		"event":          synced.Event,
	}
	payload, _ := json.Marshal(detail)
	logutil.Error("窗口太小：触发消息已被淘汰，放弃回复任务",
		"workspace", workspaceID, "chat", synced.ChatID, "message", synced.Message.MessageID)
	if store := b.workspaces.StateStore(); store != nil {
		store.SaveAlert(state.Alert{WorkspaceID: workspaceID, Kind: "window_too_small", Detail: string(payload)})
	}
}

// deadLetter 把一个重试超限的条目记成报警。
//
// Item 里只有 stage / key / id，没有账号。账号从分区键里拆：
// key 是 conversation.ChatKey 拼的「workspaceID|chatID」，
// 不拆的话报警在面板上只能显示「全局」，看不出是哪个账号出的问题。
func (b *Bot) deadLetter(item pipeline.Item, err error) {
	workspaceID, chatID := item.Key, ""
	if left, right, found := strings.Cut(item.Key, "|"); found {
		workspaceID, chatID = left, right
	}
	detail, _ := json.Marshal(map[string]any{
		"stage": item.Stage, "key": item.Key, "chat": chatID, "item_id": item.ID,
		"attempts": item.Attempts, "error": err.Error(), "body": json.RawMessage(item.Body),
	})
	logutil.Error("阶段处理超过重试上限，已隔离",
		"workspace", workspaceID, "chat", chatID, "stage", item.Stage, "item", item.ID, "err", err)
	if store := b.workspaces.StateStore(); store != nil {
		store.SaveAlert(state.Alert{
			WorkspaceID: workspaceID,
			Kind:        "dead_letter_" + item.Stage,
			Detail:      string(detail),
		})
	}
}

// forward 与 closeBuffers 都不在这个文件里了：域只管处理，投递与起停在
// orchestrator 与 ingest.go（Run/drain/closeBuffers）。
