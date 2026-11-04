package speech

import (
	"context"
	"strings"

	"good-review-master/onebot"
)

// agent_gate.go：#启动 / #关闭 —— 会话级的 Agent 开关。
//
// 它们是唯一两条「执行完不停工作流」的指令（Command.ContinueAfterRun）：
// 打标签与生成本身也要看这个开关，所以指令必须跑在它们前面，跑完再让这条消息
// 继续往下走——这样 #启动 的那一句话才能被刚打开（或刚绑好人格）的 Agent 回复。
//
// 为什么关键字注册成 `#启动` 而不是 `#启动！`：分发是前缀树最长匹配，
// 注册到 `#启动` 之后 `#启动` / `#启动！` / `#启动!` / `#启动！鲸鱼娘` 全都是同一路由，
// 后面那串（含感叹号）落进 extra，由 personaNameFromExtra 洗掉。
const (
	keywordAgentStart = "#启动"
	keywordAgentStop  = "#关闭"
)

const (
	fmtAgentStart  = keywordAgentStart + " [人格名]"
	helpAgentStart = "格式：" + fmtAgentStart +
		"\n开启本会话的打标签与自动回复；带人格名时顺带加载该人格（如：#启动！ 鲸鱼娘）。" +
		"\n不带人格名则沿用本会话当前人格。"

	fmtAgentStop  = keywordAgentStop
	helpAgentStop = "格式：" + fmtAgentStop +
		"\n关闭本会话的打标签与自动回复：消息照常入库，但不再调用大模型。"
)

// handleAgentStart #启动 [人格名]：先把开关打开，再尝试加载人格。
//
// 顺序不能反：人格加载失败（名字写错、关键字已被删）时开关仍然要开——
// 用户的意思是"让这个群恢复运行"，人格只是顺带的修饰。
func (r *Router) handleAgentStart(_ context.Context, event onebot.Event, groupID string,
	systemPrompt string, keywordPrompt string, mentionerNick string, extra string) {
	r.SetAgentEnabled(groupID, true)

	name := personaNameFromExtra(extra)
	if name == "" {
		r.reply(event, groupID, "✅ 已启动：打标签与自动回复已开启。")
		return
	}
	binding := r.personaBinding(name)
	if binding == nil {
		// 人格不存在不是错误：跳过加载，把这件事说清楚就够了。
		r.reply(event, groupID,
			"✅ 已启动：打标签与自动回复已开启（未找到人格「"+name+"」，沿用当前人格）。")
		return
	}
	r.setGroupPersona(groupID, binding)
	r.reply(event, groupID, "✅ 已启动：打标签与自动回复已开启（人格："+name+"）。")
}

// handleAgentStop #关闭：只关开关，不动人格。
//
// 不清人格是有意的：下次 #启动 不写人格名，就等于"接着用上次那个人格"。
func (r *Router) handleAgentStop(_ context.Context, event onebot.Event, groupID string,
	systemPrompt string, keywordPrompt string, mentionerNick string, extra string) {
	r.SetAgentEnabled(groupID, false)
	r.reply(event, groupID,
		"✅ 已关闭：本群暂停打标签与自动回复，发 "+keywordAgentStart+" 可重新开启。")
}

// personaNameFromExtra 从「#启动！ 鲸鱼娘」里取出人格名。
//
// 关键字只注册到 `#启动`，所以感叹号（全角/半角）、冒号、逗号、空白都会落进 extra。
// 只剥前缀、不剥中间：人格名本身可能带空格以外的字符，内部一律原样保留。
func personaNameFromExtra(extra string) string {
	return strings.TrimSpace(strings.TrimLeft(extra, "！!：:，,、~～ \t"))
}
