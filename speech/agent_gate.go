package speech

import (
	"context"
	"regexp"
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
	helpAgentStart = "格式：" + fmtAgentStart + "，或 #人格名，启动！" +
		"\n开启本会话的打标签与自动回复；带人格名时顺带加载该人格（如：#启动！ 鲸鱼娘，或 #鲸鱼娘，启动！）。" +
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
		// 不带人格名：沿用当前人格，发它的字符画。
		if binding, ok := r.getGroupPersona(groupID); ok {
			r.personaBanner(event, groupID, binding.keyword, binding.persona.Art)
			return
		}
		// 一个人格都没设过：主角就是机器人自己，报自己的昵称。
		// 这里不退回「已启动：打标签与自动回复已开启」——那句话是系统俯视群聊，
		// 群友看到的应该是一个“人”上线了，哪怕这个人还没有人格设定。
		who := r.selfName()
		if who == "" {
			who = "我" // 昵称没配时的兵底，至少是一句人话
		}
		r.personaBanner(event, groupID, who, "")
		return
	}
	binding := r.personaBinding(name)
	if binding == nil {
		// 人格不存在不是错误：跳过加载，只把「这个名字没人」说清楚。
		msg := "❌ 未找到人格「" + name + "」"
		if cur, ok := r.getGroupPersona(groupID); ok {
			msg += "，仍由「" + cur.keyword + "」回应。"
		} else {
			msg += "，本群还没设定人格。"
		}
		r.reply(event, groupID, msg)
		return
	}
	r.setGroupPersona(groupID, binding)
	r.personaBanner(event, groupID, name, binding.persona.Art)
}

// bannerRule 是上线回执的第一行：把「@某人」挡在图形上面。
//
// 回执是「引用 + @ + 正文」，客户端把 @ 和正文首行拼成「@某人 <首行>」；
// 不先垫一行，@ 就挤进字符画的首行，整张图当场散架。
// 顺带当名字牌的上沿——三行符号上下有框，比几行散字像一块牌子。
const bannerRule = "﹏﹏﹏﹏﹏﹏﹏﹏"

// personaBanner 人格刚上线时发的那一条：一条引线 → 字符画 → 一行状态。
//
// #启动 与 #切换人格 共用这一个出口：两边都说「这个人格上线了」，分成两套文案迟早漂。
// 没有字符画的人格只发引线和那一行。
//
// 字符画**不靠行首空格对齐**：QQ 会把每行开头的空格吃掉（群里的实际渲染就是
// “    ⌇ ⌇”变成“⌇ ⌇”），所以图只能左对齐画，写进 config.Persona.Art 之前先用
// trimArtIndent 抹平——配置里怎么写、群里就怎么显示，不留“我本机看着是对的”这种坑。
//
// 会话关着时第二行改成「已切换（本群关闭中…）」：那条路上去 agent 会被开关静默丢掉，
// 什么都不说的话，群里只看到一张脸和一句“正在启动中”，只会以为切失败了。
// #启动 进来时开关已经先打开，所以它永远拿不到这个分支。
func (r *Router) personaBanner(event onebot.Event, groupID, name, art string) {
	line := name + "正在启动中..."
	if !r.AgentEnabled(groupID) {
		line = name + "已切换（本群关闭中，暂时不会回应，发 " + keywordAgentStart + " 可开启）"
	}
	var buf strings.Builder
	buf.WriteString(bannerRule)
	buf.WriteString("\n")
	if art = trimArtIndent(art); art != "" {
		buf.WriteString(art)
		buf.WriteString("\n")
	}
	buf.WriteString(line)
	r.reply(event, groupID, buf.String())
}

// selfName 无人格可用时“上线”的名字：机器人自己的昵称。
// 一个人格都没设过的群里，主角就是它本人——总不能又退回一句系统话术。
func (r *Router) selfName() string {
	if r.appCfg == nil {
		return ""
	}
	return strings.TrimSpace(r.appCfg().BotNickname)
}

// trimArtIndent 抹掉字符画每行行首的空格与制表符，顺带去掉首尾空行。
//
// QQ 会吃掉消息里每行开头的空白（群里实测「    ⌇ ⌇」渲染成「⌇ ⌇」），
// 靠缩进摆位的图在群里必定散架。与其让配置里写的样子和群里看到的两回事，
// 不如发之前就抹平：图一律左对齐画，给 add_persona 的提示词也是这么要求的。
// 行内空格不动——「⌇ ⌇」「( ˶•ᴗ•˶ )>~」靠的就是这些。
func trimArtIndent(art string) string {
	lines := strings.Split(strings.TrimRight(art, "\n"), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimLeft(line, " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
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

// personaStartRe 匹配「人格名在前」的启动写法：#鲸鱼娘，启动！ / #HRBP,启动!
//
// 为什么不能注册成关键字：`#` 后面是任意人格名，前缀树只能存固定串。
// 所以它当作一条专用规则跑在前缀树之前（matchPersonaStart），命中后复用 `#启动` 那条路由，
// 人格名直接作为 extra 交给 handler。
//
// 只认一种形状：# 紧接名字、名字紧接逗号、逗号紧接「启动」、启动紧接感叹号。
// 名字里不能有空白与逗号，四处也不许插空格——一旦允许容错，就要逐处解释"哪里能空格、能塞几个"，
// 而漏写一个空格的表现是「没反应」，比直接不命中更难查。
//
// 仍然认的例外：逗号与感叹号的全角半角四种组合；触发词后面跟别的内容
// （「#鲸鱼娘，启动！ 今天怎么样」也命中，后面的字不参与解析）。
var personaStartRe = regexp.MustCompile(`^#([^，,\s\x{3000}]+)[，,]启动[！!]`)

// matchPersonaStart 认领「#人格名，启动！」。
//
// 必须跑在前缀树之前：否则「#帮助，启动！」会被 trie 当成 #帮助 处理——
// 前缀匹配不看尾巴，人格名恰好叫「帮助」时就这样悄悄失败。
func (r *Router) matchPersonaStart(text string) (*Command, string, bool) {
	m := personaStartRe.FindStringSubmatch(text)
	if m == nil {
		return nil, "", false
	}
	// 复用已注册的 #启动：ContinueAfterRun、权限、handler 都跟着那一份走。
	route := trieMatch(r.table.Load().trie, keywordAgentStart)
	if route == nil || route.Keyword != keywordAgentStart {
		return nil, "", false
	}
	return route, m[1], true
}

// personaNameFromExtra 从「#启动！ 鲸鱼娘」里取出人格名。
//
// 关键字只注册到 `#启动`，所以感叹号（全角/半角）、冒号、逗号、空白都会落进 extra。
// 只剥前缀、不剥中间：人格名本身可能带空格以外的字符，内部一律原样保留。
func personaNameFromExtra(extra string) string {
	return strings.TrimSpace(strings.TrimLeft(extra, "！!：:，,、~～ \t"))
}
