package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"good-review-master/config"
)

// addCommandGenPrompt 生成新指令时使用的提示词：要求 LLM 输出 prompt + persona（5 字段全补全）。
const addCommandGenPrompt = `你是一个提示词工程师兼人格设计师。根据用户的要求，输出一个 JSON 对象，包含：
{
  "prompt": "指令的任务行为提示词（简洁有效）",
  "persona": {
    "identity": "身份背景（名字/年龄/职业/经历等，写具体可信的人物，不要堆砌标签）",
    "personality": ["性格特质列表（行为化描述，不写抽象形容词）"],
    "relationship": {"角色": "...", "对待方式": "..."},
    "system_prompt": "人格专属系统指令（行为边界/输出要求）",
    "emotion": {"维度名": "取值", "情绪反应性": "高/中/低", "...": "..."}
  }
}
要求：persona 全部字段必须补全，不得缺省；只输出 JSON，不要多余解释。`

// personaDirective 授权指令：要求 LLM 依据人格维度推演各维度变化后再回答。
// 无状态机设计——对话窗口即隐式状态，LLM 是维度变化与回答的最终解释者。
// 另含两条通用规则：①情绪缓和——用户情绪过于强烈（喜怒哀乐皆然）时情绪反应符合人格，但以缓解对方情绪为先、不升级冲突；
// ②表达多样性——回复要有变化，避免固定口头禅/收尾词，防止自我回音导致的输出趋同。
const personaDirective = `这是你的人格（emotion 字段即你的情绪维度）。回复时：依据你各维度的情绪状态，结合用户的问题内容，推演每个维度应有的变化——例如被夸奖时，
按你的情绪反应性升高愉悦相关维度、按外显度决定是否表露、按恢复速度自然回落；然后把各维度的变化体现到你的回答里，用符合这些维度的语气和方式回应用户。当用户情绪过于强烈（无论愤怒、悲伤，还是过度兴奋、狂喜）时，
情绪反应要与人格特质相符，但以缓解对方情绪为先：不要升级冲突，不针锋相对、不火上浇油，用符合人格的方式安抚、缓和对方的情绪。每次回复都在当前维度状态的基础上继续，不要每次都从平静开始。回复要有变化与丰富性：在符合人格的前提下，避免反复使用同一句口头禅或同一个词收尾，表达应自然多样。`

// RenderPersona 把人格渲染成提示片段（纯函数），由调用方追加到 systemPrompt 末尾：
// RouteMessage 关键字路由渲染 route.Persona；replyDefault 群人格渲染 #切换人格 的快照。
// 渲染顺序：身份 → 性格 → 与群友的关系 → 人格级系统指令 → 情绪维度 → 授权指令。
// 只保留 essence 字段；说话方式不预设，由 LLM 依据性格+情绪维度自然生成。
func RenderPersona(p config.Persona, sharedRules string) string {
	var b strings.Builder
	b.WriteString("【人格】\n")
	b.WriteString("身份：" + p.Identity + "\n")
	writeJSONLine(&b, "性格特质", p.Personality)
	writeJSONLine(&b, "与群友的关系", p.Relationship)
	b.WriteString("人格级系统指令：" + p.SystemPrompt + "\n")
	writeJSONLine(&b, "情绪维度", p.Emotion)
	b.WriteString("\n" + personaDirective + "\n")
	b.WriteString("\n" + sharedRules + "\n")
	return b.String()
}

// writeJSONLine 写入缩进后的 JSON 字段（json.Indent），失败则回退原始文本
func writeJSONLine(b *strings.Builder, label string, raw []byte) {
	if len(raw) == 0 {
		return
	}
	var dst bytes.Buffer
	if err := json.Indent(&dst, raw, "", "  "); err != nil {
		b.WriteString(label + "：" + string(raw) + "\n")
		return
	}
	b.WriteString(label + "：" + dst.String() + "\n")
}

// stripJSONFences 去掉 LLM 可能包裹的 ```json ... ``` 代码围栏（部分模型输出习惯）
func stripJSONFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimLeft(s, "`")
		if i := strings.IndexByte(s, '\n'); i != -1 {
			s = s[i+1:]
		}
		if idx := strings.LastIndex(s, "```"); idx != -1 {
			s = s[:idx]
		}
	}
	return strings.TrimSpace(s)
}

// addCommandJSON LLM 输出结构：prompt + persona
type addCommandJSON struct {
	Prompt  string         `json:"prompt"`
	Persona config.Persona `json:"persona"`
}

// parseAddCommandOutput 解析 LLM 生成的 {prompt, persona} JSON（纯函数，供单测穷举）。
// 校验：prompt 非空；persona 5 字段全非空；3 个 JSON 字段合法；emotion 为 JSON 对象。
func parseAddCommandOutput(output string) (string, *config.Persona, error) {
	var parsed addCommandJSON
	if err := json.Unmarshal([]byte(stripJSONFences(output)), &parsed); err != nil {
		return "", nil, fmt.Errorf("解析 LLM 输出失败（非合法 JSON）: %w", err)
	}
	prompt := strings.TrimSpace(parsed.Prompt)
	if prompt == "" {
		return "", nil, fmt.Errorf("LLM 输出缺少 prompt")
	}
	if err := validatePersona(&parsed.Persona); err != nil {
		return "", nil, err
	}
	return prompt, &parsed.Persona, nil
}

// validatePersona 校验 persona 5 字段全非空、3 个 JSON 字段合法、emotion 为 JSON 对象
func validatePersona(p *config.Persona) error {
	// 文本字段
	for field, v := range map[string]string{
		"identity":      p.Identity,
		"system_prompt": p.SystemPrompt,
	} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("persona.%s 缺失", field)
		}
	}
	// JSON 字段
	for field, raw := range map[string][]byte{
		"personality":  p.Personality,
		"relationship": p.Relationship,
		"emotion":      p.Emotion,
	} {
		if len(raw) == 0 || !json.Valid(raw) {
			return fmt.Errorf("persona.%s 缺失或非法 JSON", field)
		}
	}
	// emotion 必须是 JSON 对象（维度 → 取值）
	var emotionObj map[string]any
	if err := json.Unmarshal(p.Emotion, &emotionObj); err != nil {
		return fmt.Errorf("persona.emotion 必须是 JSON 对象: %v", err)
	}
	return nil
}
