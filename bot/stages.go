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

// handleSync 是 Stage 1：同一会话的一批消息统一补名、组装并同步入库。
func (b *Bot) handleSync(ctx context.Context, key string, items []pipeline.Item) []pipeline.FailedItem {
	byWorkspace := make(map[string][]conversation.Inbound)
	failed := make([]pipeline.FailedItem, 0)
	byMessage := make(map[string]pipeline.Item, len(items))
	for _, item := range items {
		in, err := pipeline.DecodeItem[conversation.Inbound](item)
		if err != nil {
			failed = append(failed, pipeline.FailedItem{Item: item, Err: err})
			continue
		}
		byWorkspace[in.WorkspaceID] = append(byWorkspace[in.WorkspaceID], in)
		byMessage[in.Event.MessageID] = item
	}

	for workspaceID, inbounds := range byWorkspace {
		ws, ok := b.workspaces.ByID(workspaceID)
		if !ok {
			logutil.Debug("工作空间已不存在，跳过这一批", "workspace", workspaceID)
			continue
		}
		cfg := ws.Snapshot()()
		for _, synced := range ws.PrepareBatch(ctx, inbounds) {
			nextKey := conversation.ChatKey(synced.WorkspaceID, synced.ChatID)
			// 管理指令走独立阶段：控制平面不占打标签与 Agent 的并发槽，也不进群聊缓存
			//（模型看不到“刚才有人切了人格”，已确认可接受）。
			// record-only 的指令不转出：备用号只记录，交回原路径由 Stage 3 回固定话术。
			if b.isInternalCommand(ws, cfg, synced) && !policy.IsRecordOnly(cfg, ws.ID) {
				if err := b.forward(ctx, b.bCmd, pipeline.StageCommand, nextKey, synced.Message.MessageID, synced); err != nil {
					if item, ok := byMessage[synced.Message.MessageID]; ok {
						b.deadLetter(item, err)
					}
				} else {
					b.stageCommand.Submit(nextKey)
				}
				continue
			}
			if err := b.forward(ctx, b.b2, pipeline.StageAssets, nextKey, synced.Message.MessageID, synced); err != nil {
				if item, ok := byMessage[synced.Message.MessageID]; ok {
					b.deadLetter(item, err)
				}
			} else {
				b.stage2.Submit(nextKey)
			}
		}
	}
	return failed
}

// isInternalCommand 只做只读匹配：命中就说明这条消息该进 Stage Command。
// 执行前的第二次匹配在 speech.RunInternalCommand 里，两处不能合并。
func (b *Bot) isInternalCommand(ws *workspace.Workspace, cfg *config.Config, synced conversation.Synced) bool {
	if ws == nil || ws.Router == nil {
		return false
	}
	_, ok := ws.Router.MatchInternalCommand(synced.Event, synced.Message.Content,
		b.allowNicknameFallback(ws, cfg.BotNickname))
	return ok
}

// handleCommand 是 Stage Command：执行管理指令，不进入 Stage 2/3。
//
// 永远返回空：指令副作用不幂等，重试等于执行两次（#添加人格 会生成两个人格）。
// 失败只能在 handler 内部回一句错误，这条消息就此结束。
func (b *Bot) handleCommand(ctx context.Context, _ string, items []pipeline.Item) []pipeline.FailedItem {
	for _, item := range items {
		synced, err := pipeline.DecodeItem[conversation.Synced](item)
		if err != nil {
			logutil.Warn("指令阶段载荷解码失败，已丢弃", "item", item.ID, "err", err)
			continue
		}
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
	return nil
}

// handleAssets 是 Stage 2：给一批消息打资产标签，欠条落库后继续进入 Stage 3。
func (b *Bot) handleAssets(ctx context.Context, key string, items []pipeline.Item) []pipeline.FailedItem {
	failed := make([]pipeline.FailedItem, 0)
	tagger := b.workspaces.Tagger()
	for _, item := range items {
		synced, err := pipeline.DecodeItem[conversation.Synced](item)
		if err != nil {
			failed = append(failed, pipeline.FailedItem{Item: item, Err: err})
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
		nextKey := conversation.ChatKey(synced.WorkspaceID, synced.ChatID)
		if err := b.forward(ctx, b.b3, pipeline.StageAgent, nextKey, synced.Message.MessageID, synced); err != nil {
			failed = append(failed, pipeline.FailedItem{Item: item, Err: err})
			continue
		}
		b.stage3.Submit(nextKey)
	}
	return failed
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

// handleAgent 是 Stage 3：提交缓存、冻结快照、同步执行一次路由/主动接话。
func (b *Bot) handleAgent(ctx context.Context, key string, items []pipeline.Item) []pipeline.FailedItem {
	failed := make([]pipeline.FailedItem, 0)
	type proactiveTask struct {
		ws       *workspace.Workspace
		synced   conversation.Synced
		snapshot []conversation.Message
	}
	var latestProactive *proactiveTask

	for _, item := range items {
		synced, err := pipeline.DecodeItem[conversation.Synced](item)
		if err != nil {
			failed = append(failed, pipeline.FailedItem{Item: item, Err: err})
			continue
		}
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
	if latestProactive != nil && b.b3.Len(key) == 0 {
		latestProactive.ws.Router.TryProactiveNow(ctx, latestProactive.synced.Event,
			latestProactive.synced.ChatID, latestProactive.synced.Message.Content, latestProactive.snapshot)
	}
	return failed
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

// forward 把一个阶段载荷编码后放进下一段缓冲。
func (b *Bot) forward(ctx context.Context, buf pipeline.Buffer, stage, key, messageID string, payload any) error {
	item, err := pipeline.NewItem(stage, key, messageID, payload)
	if err != nil {
		logutil.Error("编码阶段载荷失败", "stage", stage, "err", err)
		return err
	}
	if err := buf.Put(ctx, item); err != nil {
		logutil.Warn("写入阶段缓冲失败", "stage", stage, "err", err)
		return err
	}
	return nil
}

// closeBuffers 关闭各阶段缓冲；阻塞中的 Take 会被唤醒。
func (b *Bot) closeBuffers() {
	b.closeOnce.Do(func() {
		b.closed.Store(true)
		for _, buf := range []pipeline.Buffer{b.b1, b.b2, b.b3, b.bCmd} {
			if err := buf.Close(); err != nil {
				logutil.Warn("关闭阶段缓冲失败", "err", err)
			}
		}
	})
}
