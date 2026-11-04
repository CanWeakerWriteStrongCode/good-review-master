package speech

import (
	"context"
	"strings"

	"good-review-master/conversation"
	"good-review-master/logutil"
	"good-review-master/onebot"
	"good-review-master/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// 分发：一条消息进来，剥掉 @ 前缀、查指令表，命中就交给处理函数，
// 没命中就走直答。这里只做"交给谁"，不做生成与发送本身。

// RouteMessage 前缀树匹配并分发。
//
// ctx 由调用方传入（轮询循环的应用 ctx，或 debug trigger 的 HTTP 请求 ctx），
// 一路传到处理器里的大模型与 MCP 调用——这条链是链路追踪的主干。
func (r *Router) RouteMessage(ctx context.Context, content string, event onebot.Event, groupID string) {
	r.routeMessage(ctx, content, event, groupID, nil, false)
}

// RouteMessageNow 是 Stage 3 的同步入口：在当前 goroutine 里完成选窗/生成/发送，
// 并只使用调用方冻结的窗口。内部指令仍走原 handler（少数内部指令自带异步）。
func (r *Router) RouteMessageNow(ctx context.Context, content string, event onebot.Event, groupID string, snapshot []conversation.Message) {
	r.routeMessage(ctx, content, event, groupID, snapshot, true)
}

func (r *Router) routeMessage(ctx context.Context, content string, event onebot.Event, groupID string, snapshot []conversation.Message, sync bool) {
	// 路由本身也记一个 span：它是"一条群消息"与"一次大模型调用"之间的那一跳，
	// 少了它，链路图上就看不出是哪条消息触发的、命中了哪个关键字。
	ctx, span := telemetry.StartSpan(ctx, "speech.route",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attribute.String("messaging.group_id", groupID)))
	defer span.End()

	// 开头取一次快照、全程共用：拼 systemPrompt 要用到模型名/昵称/看图开关，
	// 它们必须自洽。若每处都调 r.appCfg()，热更新正好插在中间就会拼出
	// "新模型名 + 旧昵称"这种混合提示词。
	cfg := r.appCfg()

	text := stripCQPrefix(content, cfg)
	// 剥掉 @/昵称前缀后没正文（例如只 @ 机器人、一个字没打）：
	// 只有当这条消息确实点在机器人身上才继续，@ 的是别人就不该回。
	// 聊天记录里保留了完整的「@昵称(ID)」，模型能看到这是一次点名并自行回应。
	if text == "" {
		if strings.TrimSpace(content) == "" || !IsAddressed(event, content, cfg, true) {
			return
		}
	}

	// 没有正文时跳过关键字匹配：空串不该命中任何指令。
	var route *Command
	if text != "" {
		route = trieMatch(r.table.Load().trie, text)
	}
	if route == nil {
		// 未命中：走普通问答（replyDefault 会视群人格再补一段）
		// 焦点提示由 replyDefault 在人格块之后追加，保证它是最后一条指令。
		systemPrompt := ComposeSystemPrompt(cfg, event.BotID(), "", r.promptCfg.ReplyRequirement(), "")
		if sync {
			r.replyDefaultNow(ctx, text, event, groupID, systemPrompt, snapshot)
		} else {
			r.replyDefault(ctx, text, event, groupID, systemPrompt)
		}
		return
	}

	// 管理指令（Category == internal）统一交给 RunInternalCommand：
	// 权限判定、参数解析、handler 调用只有那一个入口，#帮助 与私聊的例外也在里面。
	// 阶段二把指令前移到独立阶段后，正常消息链路已经到不了这里；
	// 面板与调试触发仍从这条路径进来，两边行为必须一致，所以不再各写一份。
	if route.Category == "internal" {
		r.RunInternalCommand(ctx, event, groupID, content, true)
		return
	}

	extra := strings.TrimSpace(text[len(route.Keyword):])
	// 关键字路由只把"路由自带人格"渲染进 systemPrompt 末尾；
	// 群人格（#切换人格）只在未命中关键字的纯 @ 聊天里生效，见 replyDefault。
	persona := ""
	if route.Persona != nil {
		persona = RenderPersona(*route.Persona, route.SharedRules)
	}
	// 有人直接 @ 机器人说话：只回最后一条点名消息，避免把回复铺到更早的 @ 或别的话题上。
	systemPrompt := ComposeSystemPrompt(cfg, event.BotID(), persona, r.promptCfg.ReplyRequirement(), r.directFocusPrompt(event))
	span.SetAttributes(attribute.String("speech.keyword", route.Keyword),
		attribute.String("speech.category", route.Category))
	if sync && route.Category == "chat_review" {
		logutil.InfoCtx(ctx, "触发锐评", "group", groupID, "user", r.nameOf(groupID, event.UserID), "user_id", event.UserID)
		reply, err := r.Generate(ctx, replyParams{
			event: event, groupID: groupID, systemPrompt: systemPrompt,
			keywordPrompt: route.Prompt, mention: true, snapshot: snapshot,
		})
		if err == nil {
			r.deliverChat(ctx, reply)
		}
		return
	}
	route.Handler(ctx, event, groupID, systemPrompt, route.Prompt, r.nameOf(groupID, event.UserID), extra)
}

// replyDefault @bot 但未匹配任何指令：直接发给大模型。
// systemPrompt 只含机器人身份（模型名+昵称），不带指令共享规则；若本群已 #切换人格，
// 则在末尾追加渲染好的人格块（仅纯 @ 聊天生效）；否则显式声明当前是普通问答、
// 不代入任何人格，避免模型曾在被切换过人格的群里继续沿用旧人设。
// 复用 chatReview 的缓存窗口/回复/锚点逻辑。
func (r *Router) replyDefault(ctx context.Context, text string, event onebot.Event, groupID string, systemPrompt string) {
	if b, ok := r.getGroupPersona(groupID); ok {
		systemPrompt += "\n" + RenderPersona(b.persona, b.sharedRules)
	} else {
		systemPrompt += "\n当前无指定人格，现在是普通大模型问答模式：请如实正常回答，不要代入任何虚构人格或角色。"
	}
	systemPrompt += "\n" + r.directFocusPrompt(event)
	r.chatReview(ctx, event, groupID, systemPrompt, "", r.nameOf(groupID, event.UserID), text)
}

// replyDefaultNow 是 replyDefault 的同步版本，供 Stage 3 使用。
func (r *Router) replyDefaultNow(ctx context.Context, text string, event onebot.Event, groupID, systemPrompt string, snapshot []conversation.Message) {
	if b, ok := r.getGroupPersona(groupID); ok {
		systemPrompt += "\n" + RenderPersona(b.persona, b.sharedRules)
	} else {
		systemPrompt += "\n当前无指定人格，现在是普通大模型问答模式：请如实正常回答，不要代入任何虚构人格或角色。"
	}
	systemPrompt += "\n" + r.directFocusPrompt(event)
	logutil.InfoCtx(ctx, "触发锐评", "group", groupID, "user", r.nameOf(groupID, event.UserID), "user_id", event.UserID)
	reply, err := r.Generate(ctx, replyParams{
		event: event, groupID: groupID, systemPrompt: systemPrompt,
		mention: true, snapshot: snapshot,
	})
	if err == nil {
		r.deliverChat(ctx, reply)
	}
}
