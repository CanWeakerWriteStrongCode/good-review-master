package bot

import (
	"context"
	"encoding/json"
	"time"

	"good-review-master/assets"
	"good-review-master/cache"
	"good-review-master/logutil"
	"good-review-master/pipeline"
	"good-review-master/telemetry"
	"good-review-master/workspace"
	"good-review-master/workspace/state"
)

// handleSync 是 Stage 1：同一会话的一批消息统一补名、组装并同步入库。
func (b *Bot) handleSync(ctx context.Context, key string, items []pipeline.Item) []pipeline.FailedItem {
	byWorkspace := make(map[string][]pipeline.InboundEvent)
	failed := make([]pipeline.FailedItem, 0)
	byMessage := make(map[string]pipeline.Item, len(items))
	for _, item := range items {
		in, err := pipeline.DecodeItem[pipeline.InboundEvent](item)
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
		for _, synced := range ws.PrepareBatch(ctx, inbounds) {
			nextKey := pipeline.ChatKey(synced.WorkspaceID, synced.ChatID)
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

// handleAssets 是 Stage 2：给一批消息打资产标签，欠条落库后继续进入 Stage 3。
func (b *Bot) handleAssets(ctx context.Context, key string, items []pipeline.Item) []pipeline.FailedItem {
	failed := make([]pipeline.FailedItem, 0)
	tagger := b.workspaces.Tagger()
	for _, item := range items {
		synced, err := pipeline.DecodeItem[pipeline.SyncedMessage](item)
		if err != nil {
			failed = append(failed, pipeline.FailedItem{Item: item, Err: err})
			continue
		}
		enriched := pipeline.EnrichedMessage{SyncedMessage: synced}
		if ws, ok := b.workspaces.ByID(synced.WorkspaceID); ok {
			if tagger != nil && len(synced.Message.Images) > 0 {
				result := tagger.TagURLs(ctx, synced.Message.Images)
				enriched.ImageIDs = result.ImageIDs
				enriched.ImageNotes = result.ImageNotes
				enriched.Assets = result.Assets
				enriched.AssetStatus = result.Status
				if len(result.ImageIDs) > 0 && ws.State != nil {
					if err := ws.State.UpdateMessageImages(synced.ChatID, synced.Message.MessageID, result.ImageIDs); err != nil {
						logutil.Warn("回写消息图片标识失败", "err", err)
					}
				}
				b.persistDeferred(synced, result)
			}
		}
		nextKey := pipeline.ChatKey(enriched.WorkspaceID, enriched.ChatID)
		if err := b.forward(ctx, b.b3, pipeline.StageAgent, nextKey, enriched.Message.MessageID, enriched); err != nil {
			failed = append(failed, pipeline.FailedItem{Item: item, Err: err})
			continue
		}
		b.stage3.Submit(nextKey)
	}
	return failed
}

// persistDeferred 把没打完标签的资产写成欠条，只记原始地址。
// 原图不另存副本：后台补标签时直接按这个地址再看一眼。
func (b *Bot) persistDeferred(synced pipeline.SyncedMessage, result assets.TagResult) {
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
		if deferred.Index < len(result.Assets) && result.Assets[deferred.Index].Status == pipeline.AssetStatusFailed {
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
		enriched pipeline.EnrichedMessage
		snapshot []cache.Message
	}
	var latestProactive *proactiveTask

	for _, item := range items {
		enriched, err := pipeline.DecodeItem[pipeline.EnrichedMessage](item)
		if err != nil {
			failed = append(failed, pipeline.FailedItem{Item: item, Err: err})
			continue
		}
		ws, ok := b.workspaces.ByID(enriched.WorkspaceID)
		if !ok {
			continue
		}
		cfg := ws.Snapshot()()
		gc := ws.Cache.GetGroupCache(enriched.ChatID, cfg.MaxCacheMsg)
		msg := enriched.Message
		msg.ImageIDs = enriched.ImageIDs
		msg.ImageNotes = enriched.ImageNotes
		gc.Add(msg)
		telemetry.CacheMessages.WithLabelValues(ws.ID, enriched.ChatID).Set(float64(gc.Len()))
		if !enriched.Private {
			ws.LearnIdentity(enriched.ChatID, enriched.Event.Self.UserID)
			ws.SyncSelfName(enriched.Event.BotID(), enriched.ChatID)
		}

		msgs := gc.GetAll()
		index := cache.FindMsgIndex(msgs, enriched.Message.MessageID)
		if index < 0 {
			b.recordAging(ws.ID, enriched, msgs)
			continue
		}
		snapshot := msgs[:index+1]

		if enriched.Private || enriched.Event.MentionsBot() || enriched.Event.AdapterAtMe() ||
			isAtBot(enriched.Message.Content, cfg, b.allowNicknameFallback(ws, cfg.BotNickname)) {
			ws.Router.RouteMessageNow(ctx, enriched.Message.Content, enriched.Event, enriched.ChatID, snapshot)
			continue
		}
		// 主动接话 latest-wins：同一批只看最后一条，且后面还有积压时也不判，
		// 交给下一批的最后一条。避免刷屏时每条都跑一次裁判。
		latestProactive = &proactiveTask{ws: ws, enriched: enriched, snapshot: snapshot}
	}
	if latestProactive != nil && b.b3.Len(key) == 0 {
		latestProactive.ws.Router.TryProactiveNow(ctx, latestProactive.enriched.Event,
			latestProactive.enriched.ChatID, latestProactive.enriched.Message.Content, latestProactive.snapshot)
	}
	return failed
}

// recordAging 记录“触发消息已不在窗口里”：放弃这条回复任务，但保存完整现场。
func (b *Bot) recordAging(workspaceID string, enriched pipeline.EnrichedMessage, msgs []cache.Message) {
	first, last := "", ""
	if len(msgs) > 0 {
		first = msgs[0].MessageID
		last = msgs[len(msgs)-1].MessageID
	}
	detail := map[string]any{
		"reason":         "触发消息已不在窗口缓存里，放弃本次回复",
		"hint":           "窗口小了，要调大 llm_send_count / max_cache_msg",
		"workspace_id":   workspaceID,
		"chat_id":        enriched.ChatID,
		"message_id":     enriched.Message.MessageID,
		"window_first":   first,
		"window_last":    last,
		"window_count":   len(msgs),
		"llm_send_count": b.cfg().LLMSendCount,
		"max_cache_msg":  b.cfg().MaxCacheMsg,
		"event":          enriched.Event,
	}
	payload, _ := json.Marshal(detail)
	logutil.Error("窗口太小：触发消息已被淘汰，放弃回复任务",
		"workspace", workspaceID, "chat", enriched.ChatID, "message", enriched.Message.MessageID)
	if store := b.workspaces.StateStore(); store != nil {
		store.SaveAlert(state.Alert{WorkspaceID: workspaceID, Kind: "window_too_small", Detail: string(payload)})
	}
}

func (b *Bot) deadLetter(item pipeline.Item, err error) {
	detail, _ := json.Marshal(map[string]any{
		"stage": item.Stage, "key": item.Key, "item_id": item.ID,
		"attempts": item.Attempts, "error": err.Error(), "body": json.RawMessage(item.Body),
	})
	logutil.Error("阶段处理超过重试上限，已隔离", "stage", item.Stage, "item", item.ID, "err", err)
	if store := b.workspaces.StateStore(); store != nil {
		store.SaveAlert(state.Alert{Kind: "dead_letter_" + item.Stage, Detail: string(detail)})
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

// closeBuffers 关闭三段缓冲；阻塞中的 Take 会被唤醒。
func (b *Bot) closeBuffers() {
	b.closeOnce.Do(func() {
		b.closed.Store(true)
		for _, buf := range []pipeline.Buffer{b.b1, b.b2, b.b3} {
			if err := buf.Close(); err != nil {
				logutil.Warn("关闭阶段缓冲失败", "err", err)
			}
		}
	})
}
