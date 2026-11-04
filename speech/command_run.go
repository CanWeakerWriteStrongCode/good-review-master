package speech

import (
	"context"
	"strings"

	"good-review-master/config"
	"good-review-master/logutil"
	"good-review-master/onebot"
	"good-review-master/policy"
)

// 内部指令（Category == "internal"）的"判"与"做"。
//
// 它们跑在独立的工作流里（bot 包的 command 域），与聊天数据平面分开：
// 指令不占打标签与 Agent 的并发槽，也不进群聊缓存。
// 这里只回答"是不是指令"和"执行一条指令"，不碰流水线本身。

// MatchInternalCommand 只回答"这条消息是不是内部指令"，不执行、不改状态。
// 前置用它决定发哪种事件（终止 / 继续）；必须保持无副作用。
//
// continues 是命中指令的 ContinueAfterRun：它只回答「执行完还要不要让这条消息
// 走下去」，不预设是哪一条指令——拓扑只照抄，不认关键字。
func (r *Router) MatchInternalCommand(event onebot.Event, content string, allowNickname bool) (keyword string, continues bool, ok bool) {
	route, extra, matched := r.matchInternal(event, content, allowNickname)
	if !matched {
		return "", false, false
	}
	continues = route.ContinueAfterRun
	// 有的指令要先看参数才知道该不该往下走（#切换人格：没切成就到此为止）。
	if continues && route.ContinueWhen != nil {
		continues = route.ContinueWhen(extra)
	}
	return route.Keyword, continues, true
}

// RunInternalCommand 执行一条内部指令，返回 false 表示"现在匹配不上了"。
//
// 这是内部指令唯一能产生副作用的入口：点名判定与权限判定都收在这里，
// command 域、debug trigger、以后的面板都必须经过它。
// 返回 false 时调用方直接丢弃即可——消息正文在前置已经入库。
func (r *Router) RunInternalCommand(ctx context.Context, event onebot.Event, groupID, content string, allowNickname bool) bool {
	route, extra, ok := r.matchInternal(event, content, allowNickname)
	if !ok {
		return false
	}
	if !r.canUseInternalCommand(r.appCfg(), route, groupID, event.UserID) {
		logutil.InfoCtx(ctx, "管理指令被拒绝", "group", groupID,
			"user_id", event.UserID, "keyword", route.Keyword)
		r.reply(event, groupID, "⛔ 你没有使用该管理指令的权限")
		return true
	}
	// systemPrompt / keywordPrompt 对内部指令无用：现有 handler 都不读它们，
	// 传空串让调用方不必准备窗口快照。
	route.Handler(ctx, event, groupID, "", route.Prompt, r.nameOf(groupID, event.UserID), extra)
	return true
}

// matchInternal 是同一条匹配链：点名 → 剥 @ 前缀 → 专用规则/前缀树 → 必须命中内部指令。
//
// 返回的 extra 是已切好的指令参数，调用方不必再按关键字长度二次切——
// 「#人格名，启动！」的参数跟关键字不在同一个位置上。
//
// 分流与执行各判一次：两次之间路由表可能被别的指令改过（#删除人格 会把关键字
// 删掉），所以执行前必须重新确认，不能把第一次的结果当令牌。
func (r *Router) matchInternal(event onebot.Event, content string, allowNickname bool) (*Command, string, bool) {
	cfg := r.appCfg()
	if !IsAddressed(event, content, cfg, allowNickname) {
		return nil, "", false
	}
	text := commandText(event, cfg)
	if text == "" {
		return nil, "", false
	}
	// 「人格名在前」的写法必须先判：前缀树不看尾巴，#帮助，启动！ 会被当成 #帮助。
	if route, name, ok := r.matchPersonaStart(text); ok {
		return route, name, true
	}
	route := trieMatch(r.table.Load().trie, text)
	if route == nil || route.Category != "internal" {
		return nil, "", false
	}
	return route, strings.TrimSpace(text[len(route.Keyword):]), true
}

// commandText 返回解析内部指令用的正文。
//
// 只取文本段、不用渲染后的正文：渲染会把 @昵称(ID) 按原位置拼进去，
// 「#切换人格@机器人」这种写法下 mention 会被当成指令参数传给 handler
// （表现为「未找到人格：@昵称(ID)」）。手打的「@昵称」前缀照旧要剥掉。
func commandText(event onebot.Event, cfg *config.Config) string {
	return stripCQPrefix(strings.TrimSpace(event.PlainText()), cfg)
}

// canUseInternalCommand 是权限判定的唯一实现：非管理指令不受限，
// #帮助 对未授权用户开放（查看列表无副作用，便于自助发现）。
func (r *Router) canUseInternalCommand(cfg *config.Config, route *Command, groupID, userID string) bool {
	if route == nil || route.Category != "internal" || route.Keyword == internalKeywordHelp {
		return true
	}
	return policy.CanUseCommand(cfg, groupID, userID)
}
