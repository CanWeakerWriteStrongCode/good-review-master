package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"good-review-master/config"
)

// RenderPersona 把人格渲染成提示片段（纯函数），由调用方追加到 systemPrompt 末尾：
// RouteMessage 关键字路由渲染 route.Persona；replyDefault 群人格渲染 #切换人格 的快照。
// 渲染顺序：身份 → 性格 → 与群友的关系 → 人格级系统指令 → 情绪维度 → 该指令的共享规则。
// 只保留 essence 字段；说话方式不预设，由 LLM 依据性格+情绪维度自然生成。
func RenderPersona(p config.Persona, sharedRules string) string {
	var b strings.Builder
	b.WriteString("【人格】\n")
	b.WriteString("身份：" + p.Identity + "\n")
	writeJSONLine(&b, "性格特质", p.Personality)
	writeJSONLine(&b, "与群友的关系", p.Relationship)
	b.WriteString("人格级系统指令：" + p.SystemPrompt + "\n")
	writeJSONLine(&b, "情绪维度", p.Emotion)
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
