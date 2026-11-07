package speech

import (
	"strings"

	"good-review-master/config"
)

// 判官的 system prompt：人格片 + 四段规则。
//
// 判官要回答的是"该不该开口"，而这件事被两层东西支配：一层是"轮不轮得到你"
// （对象、安全、时序、能力），一层是"这个热闹值不值得凑"。前一层与角色性格无关，
// 后一层恰恰由角色决定——雌小鬼乐意接梗，教师只回应问题。所以规则按这两层拆开，
// 人格只喂给后一层。
//
// 人格片放在最前面。缓存上"放最后"能跨群共享前缀，但那样判官读到的顺序会变成
// "规则 → 角色"：规则先入为主、角色被推到末尾，模型对末尾内容更敏感，结果是
// 人格基本不起作用。放最前放弃的是跨群共享，留下的是每群一条稳定前缀——
// 前缀缓存内容寻址，群人格不常切换，这条前缀就一直命中。

// renderJudgePersona 渲染判官视角的人格片：只取身份、性格、与群友的关系。
//
// 为什么不复用 RenderPersona：那一份是给生成用的，末尾还挂着人格级系统指令
// （"回复控制在 50 字"这类输出要求）和关键字共享规则。把输出要求喂给一个只输出
// 四个字段的判官，它会以为自己要写回复；共享规则还会和机械层打架。
//
// emotion 也不进来：它描述的是情绪反应的强度，对"该不该开口"这种二值判断没有
// 稳定影响。真观察到判定偏差再把某个维度补进来，别一开始就把几百 token 押在
// 一个没量过的字段上。
func renderJudgePersona(p config.Persona) string {
	var b strings.Builder
	if identity := strings.TrimSpace(p.Identity); identity != "" {
		b.WriteString("身份：" + identity + "\n")
	}
	writeJSONLine(&b, "性格特质", p.Personality)
	writeJSONLine(&b, "与群友的关系", p.Relationship)
	return strings.TrimRight(b.String(), "\n")
}

// composeJudgeSystem 拼判官的 system prompt（纯函数，便于单测穷举有无人格两种情形）。
//
// 段落之间空一行：四段规则各自以【标题】开头，空行把它们和人格片的正文隔开，
// 否则人格片最后一行会和机械层的标题粘在一起。
func composeJudgeSystem(pc *config.PromptConfig, persona *config.Persona) string {
	blocks := make([]string, 0, 5)
	if persona != nil {
		intro := strings.TrimRight(pc.JudgePersonaIntro(), "\n")
		blocks = append(blocks, intro+"\n"+renderJudgePersona(*persona))
	}
	blocks = append(blocks,
		pc.ProactiveJudgeRole(),
		pc.ProactiveJudgeMechanical(),
		pc.ProactiveJudgeVibe(),
		pc.ProactiveJudgeOutput(),
	)
	return strings.Join(blocks, "\n\n")
}
