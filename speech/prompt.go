package speech

import (
	"fmt"
	"strings"

	"good-review-master/config"
	"good-review-master/onebot"
)

// system prompt 的拼装。只拼字符串，不读缓存、不调模型，所以单测能直接比对。
// 导出给 tests/eval：离线评测必须拿到与线上逐字相同的提示词，
// 否则"评测全绿、线上照样回归"。

// ComposeSystemPrompt 拼装发给大模型的 system prompt：
// 可选的人格块 + 机器人身份（昵称 / 稳定 ID）+ 聊天记录约定 + 【要求】句 + 末尾的回复焦点提示。
// botID 为空时省略 ID 行（例如离线评测或拿不到 bot 身份时）。
// requirement 是【要求】句，focus 是回复焦点提示（空串表示不追加），直答分别传
// promptCfg.ReplyRequirement() / directFocusPrompt(event)（群 @ 用 mention 版，私聊用 focus_last）。
//
// 人格块放最前面：先立"你是谁"，再讲工具、聊天记录与要求——人格是对整段回答的总前提，
// 而不是对某条规则的补充说明。整段 system 每轮都不变（变的是 user 里的聊天记录），
// 所以段内顺序不影响缓存命中范围，只影响模型怎么理解指令的主次。
func ComposeSystemPrompt(cfg *config.Config, botID, personaBlock, requirement, focus string) string {
	systemPrompt := composeSystemPrompt(cfg, botID, personaBlock, requirement)
	if focus != "" {
		systemPrompt += "\n" + focus
	}
	return systemPrompt
}

// directFocusPrompt 直答的焦点提示：群里被点名（@ 或叫昵称）只回最后一条点名消息；
// 私聊没有 @ 语义，仍按"最后一条消息"处理。
func (r *Router) directFocusPrompt(event onebot.Event) string {
	if event.IsPrivate() {
		return r.promptCfg.FocusPrompt(true)
	}
	return r.promptCfg.MentionFocusPrompt()
}

// ComposeProactiveSystemPrompt 主动插话变体：聊天记录约定、身份、人格块都与上面一致，
// 只把"【要求】"那句换成主动参与的说法（由 prompt_system.yaml 的
// internal.proactive_requirement 提供）——那条消息并没有 @ 机器人，
// 继续沿用"按最后的消息回复/执行指令"会让模型以为自己被点名。
func ComposeProactiveSystemPrompt(cfg *config.Config, botID, personaBlock, requirement string) string {
	return composeSystemPrompt(cfg, botID, personaBlock, requirement)
}

// identityLine 拼「【机器人】昵称：X，ID：Y」一行，给裁判、排版这类需要认人的独立调用用：
// 昵称会变、可能重名，同群还常有别的机器人，只有 ID 才能确认某条消息是不是机器人自己。
// 两个值都拿不到时返回空串（调用方不写这一行）。
func identityLine(botName, botID string) string {
	if botName == "" && botID == "" {
		return ""
	}
	line := "【机器人】"
	if botName != "" {
		line += "昵称：" + botName
		if botID != "" {
			line += "，"
		}
	}
	if botID != "" {
		line += "ID：" + botID
	}
	return line
}

func composeSystemPrompt(cfg *config.Config, botID, personaBlock, requirement string) string {
	identity := ""
	if cfg.BotNickname != "" {
		identity = fmt.Sprintf("你的昵称是【%s】", cfg.BotNickname)
	}
	if botID != "" {
		if identity != "" {
			identity += "，"
		}
		identity += fmt.Sprintf("用户ID是【%s】", botID)
	}
	if identity != "" {
		identity += "。"
	}
	// 正文开头不写「你是一个AI，模型是XXX」：模型自己就是 AI，这句话没有信息量；
	// 写死模型名更糟——换一次模型或改一次别名，整段前缀就从头断开。
	// 身份由下面这行 identity 交代。
	body := fmt.Sprintf("【工具使用】当用户询问真实信息时，应调用对应MCP工具，禁止自行编造答案。"+
		"%s【聊天记录】每行一条群消息，message_id 是这条消息的编号（同一条消息每次都一样，按它引用），user_id 是发言者的稳定ID，nickname 是昵称，message 是正文；"+
		"@昵称(ID) 表示 @ 了该用户，昵称可能变化，以括号里的 ID 为准。"+
		"reply_id 表示该消息引用了哪条 message_id。"+
		requirement,
		identity)
	// 看图说明不放在这里：它只该在有图的时候出现。
	// 窗口里真有图时，user 消息尾部会拼【群内图片候选】段（imageCandidatesBlock），
	// 里面已经写了"候选只是文字 / low 不看 / url 逐字复制 / 最多看 N 张 / 别编造"。
	// 固定写在 system 里等于没图的请求白付两百字符，而且和那段重复。
	if personaBlock == "" {
		return body
	}
	// 人格块自身以换行结尾（RenderPersona 的最后一行），这里统一抹掉再只补一个换行，
	// 不靠调用方恰好传了带尾随换行的字符串。
	return strings.TrimRight(personaBlock, "\n") + "\n" + body
}
