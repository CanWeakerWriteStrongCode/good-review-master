package speech

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"good-review-master/config"
)

// judgePromptConfig 拿一份全走内置兜底值的提示词配置：
// 空文件解析出零值，每段 internalPrompt 都会退回 Go 里的常量。
// 本文件只关心"段落怎么拼、拼了哪些字段"，不关心配置读文件的那条链路。
func judgePromptConfig(t *testing.T) *config.PromptConfig {
	t.Helper()
	dir := t.TempDir()
	systemPath := filepath.Join(dir, "prompt_system.yaml")
	if err := os.WriteFile(systemPath, nil, 0o600); err != nil {
		t.Fatalf("写入 prompt_system.yaml 失败：%v", err)
	}
	pc, err := config.LoadPromptConfig(systemPath, filepath.Join(dir, "prompt_custom.yaml"))
	if err != nil {
		t.Fatalf("加载提示词配置失败：%v", err)
	}
	return pc
}

// 判官要的是"这个角色在乎什么"，不是"这个角色怎么输出"。
// 人格级系统指令（输出要求）与 emotion 一旦混进来，判官会以为自己要写回复，
// 或者按情绪档位改判定——这两件都不该发生在只输出四个字段的调用里。
func Test裁判人格片只带身份性格与关系(t *testing.T) {
	persona := config.Persona{
		Identity:     "林小满，24 岁，中学语文老师",
		Personality:  []byte(`["说话慢，喜欢追问细节","看不惯糊弄"]`),
		Relationship: []byte(`{"角色":"老师","对待方式":"耐心但严格"}`),
		SystemPrompt: "【输出要求】回答控制在 50 字以内，不许用表情符号",
		Emotion:      []byte(`{"情绪反应性":"低"}`),
		Art:          "⌇ ⌇",
	}

	block := renderJudgePersona(persona)
	for _, want := range []string{"林小满", "说话慢", "耐心但严格"} {
		if !strings.Contains(block, want) {
			t.Fatalf("人格片应包含 %q，实际：%s", want, block)
		}
	}
	for _, unwanted := range []string{"【输出要求】", "情绪反应性", "⌇ ⌇"} {
		if strings.Contains(block, unwanted) {
			t.Fatalf("人格片不该包含 %q，实际：%s", unwanted, block)
		}
	}
}

// 人格片在最前，四段规则按 外壳 → 机械层 → 氛围层 → 输出契约 排。
// 没有人格时只少第一段：机械层与氛围层必须都还在，否则"该沉默时不乱接"
// 与"该开口时别漏"这两头会同时失效。
func Test裁判提示词把人格片放最前(t *testing.T) {
	pc := judgePromptConfig(t)
	persona := &config.Persona{
		Identity:     "鲸鱼娘",
		Personality:  []byte(`["爱逗人，看到大话就想拆台"]`),
		Relationship: []byte(`{"角色":"群宠","对待方式":"黏人"}`),
	}

	with := composeJudgeSystem(pc, persona)
	order := []string{
		pc.JudgePersonaIntro(),
		pc.ProactiveJudgeRole(),
		pc.ProactiveJudgeMechanical(),
		pc.ProactiveJudgeVibe(),
		pc.ProactiveJudgeOutput(),
	}
	prev := -1
	for _, part := range order {
		at := strings.Index(with, part)
		if at < 0 {
			t.Fatalf("拼出来的 system 缺少段落：%s", part)
		}
		if at <= prev {
			t.Fatalf("段落顺序不对：%q 的位置是 %d，前一段在 %d", part, at, prev)
		}
		prev = at
	}
	if !strings.Contains(with, "鲸鱼娘") {
		t.Fatalf("群有人格时 system 里应出现人格名：%s", with)
	}

	without := composeJudgeSystem(pc, nil)
	if strings.Contains(without, pc.JudgePersonaIntro()) {
		t.Fatalf("群没设人格时不该写人格片的外壳：%s", without)
	}
	for _, part := range order[1:] {
		if !strings.Contains(without, part) {
			t.Fatalf("群没设人格时四段规则仍应完整，缺：%s", part)
		}
	}
	if strings.Contains(without, "\n\n\n") || strings.HasPrefix(without, "\n") {
		t.Fatalf("段落之间不该出现连续空行或前导空行：%q", without)
	}
}
