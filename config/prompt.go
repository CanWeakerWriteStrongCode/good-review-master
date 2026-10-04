package config

import (
	"fmt"
	"good-review-master/logutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"good-review-master/apppath"

	"gopkg.in/yaml.v3"
)

// CmdConf 指令配置（keyword + prompt + 可选人格）
type CmdConf struct {
	Keyword string   `yaml:"keyword"`
	Prompt  string   `yaml:"prompt"`
	Persona *Persona `yaml:"persona"` // 人格（必填：新增指令与示例全带；加载缺失仅告警不致命）
}

// RawJSON 承载 JSON 字段：YAML 里是单引号 JSON 字符串，LLM 输出里是原生 JSON。
// yaml.v3 不能直接把字符串解成 []byte，需要自定义解码。
type RawJSON []byte

// UnmarshalYAML 从单引号 JSON 字符串解码（写入端保证单引号包裹）
func (r *RawJSON) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	*r = RawJSON(s)
	return nil
}

// UnmarshalJSON 原生 JSON 解码（LLM 输出 {prompt, persona} 时使用）
func (r *RawJSON) UnmarshalJSON(data []byte) error {
	*r = append((*r)[:0], data...)
	return nil
}

// Persona 人格配置（5 个 essence 字段，全必填，无特例）。有子字段的字段用 JSON（RawJSON）保存，
// LLM 自拟结构；纯叙述字段（Identity/SystemPrompt）保持 YAML 文本。
// 曾设 8 字段（含 greeting/speech_style/examples），因"表演脚本"式的预设（固定开场白、口癖词表、
// few-shot 示例）导致回答过于固定、内容易流于刻板，已砍掉——说话方式完全交给 LLM 依据性格+情绪自然生成。
type Persona struct {
	Identity     string  `yaml:"identity" json:"identity"`           // 身份背景（必填，YAML 文本）
	Personality  RawJSON `yaml:"personality" json:"personality"`     // 性格特质（必填，JSON 数组，行为化描述）
	Relationship RawJSON `yaml:"relationship" json:"relationship"`   // 与群友的关系（必填，JSON 对象：角色/对待）
	SystemPrompt string  `yaml:"system_prompt" json:"system_prompt"` // 人格级系统指令（必填，YAML 文本）
	Emotion      RawJSON `yaml:"emotion" json:"emotion"`             // 情绪维度（必填，JSON 对象：维度→取值，核心）
}

// PromptData 提示词可变部分的一份**完整快照**（发布后不可变）。
//
// 为什么需要这一层：热重载把 Reload 与路由重建带到了 informer 自己的 goroutine 上，
// 而消息分发、内部指令的异步任务也在读同一批数据。Go 的 map 并发读写是
// **fatal error 而不是竞态警告**——`concurrent map read and map write` 会直接带走进程。
// 所以约定变成：写侧构造全新的 map、一次性发布；读侧拿到的 map 永不被再改动。
//
// 与 config.Snapshot 是同一套思路，只是这里的数据是两张 map 而不是一个结构体。
type PromptData struct {
	CmdConfigs  map[string][]CmdConf
	SharedRules map[string]string
}

// PromptConfig 提示词配置（系统 + 自定义合并），支持热重载
type PromptConfig struct {
	systemPath string
	customPath string

	// writeMu 串行化所有写操作（Reload 与增删指令）。
	// 它们都是"读文件 → 改 → 写回"的整段事务，必须互斥，否则两次添加会互相覆盖。
	writeMu sync.Mutex

	// system 是 prompt_system.yaml 的解析结果；data 是它与自定义文件合并后的结果。
	// 两者都随 Reload 整体替换，读侧无锁。
	system atomic.Pointer[promptFile]
	data   atomic.Pointer[PromptData]
}

type promptFile struct {
	Cmd   map[string][]CmdConf `yaml:"cmd"`
	Rules map[string]string    `yaml:"rules"`
}

// LoadPromptConfig 加载提示词配置（系统 + 自定义合并）
func LoadPromptConfig(systemPath, customPath string) (*PromptConfig, error) {
	pc := &PromptConfig{
		systemPath: systemPath,
		customPath: customPath,
	}
	pc.Reload()
	return pc, nil
}

// Snapshot 返回当前提示词数据的一份不可变视图，**无锁**。
//
// 调用方可以一直持有它、反复读其中的 map——因为发布之后没有任何代码会再改动它们。
// 这与 config.Snapshot 的约定一致：用到时取一次，跨一次完整操作用一个。
func (pc *PromptConfig) Snapshot() *PromptData {
	return pc.data.Load()
}

// Reload 重新读取并合并提示词文件，然后一次性发布。
//
// 发布点是全类唯一的：build 只构造、不碰接收者，所以读侧要么看到完整的旧数据、
// 要么看到完整的新数据，不存在"合并到一半"的中间态。
func (pc *PromptConfig) Reload() {
	pc.writeMu.Lock()
	defer pc.writeMu.Unlock()

	system, data := pc.build()
	pc.system.Store(system)
	pc.data.Store(data)
}

// build 读取并合并提示词文件，返回全新的数据（不修改接收者）。
//
// 任何一步失败都返回一份**可用的空数据**而不是 nil：PromptData 的 map 若为 nil，
// 调用方 range 没问题但写入会 panic，而且 nil 会让"取快照"这件事变得需要判空。
func (pc *PromptConfig) build() (*promptFile, *PromptData) {
	system := &promptFile{Cmd: make(map[string][]CmdConf), Rules: make(map[string]string)}
	data := &PromptData{CmdConfigs: make(map[string][]CmdConf), SharedRules: make(map[string]string)}

	raw, err := os.ReadFile(pc.systemPath)
	if err != nil {
		destPath := apppath.GetWorkPath("prompt_system.yaml")
		if writeErr := writePromptSystem(destPath); writeErr != nil {
			logutil.Warn("无法创建 prompt_system.yaml，以空指令集启动", "err", writeErr)
			return system, data
		}
		logutil.Info("已创建 prompt_system.yaml", "path", destPath)
		raw = promptSystemExampleTemplate // 使用内嵌模板字节，落入下方解析逻辑
	}
	var systemCfg promptFile
	if err := yaml.Unmarshal(raw, &systemCfg); err != nil {
		logutil.Warn("prompt_system.yaml 格式错误，将以空指令集启动", "err", err)
		return system, data
	}
	if systemCfg.Cmd == nil {
		systemCfg.Cmd = make(map[string][]CmdConf)
	}
	if systemCfg.Rules == nil {
		systemCfg.Rules = make(map[string]string)
	}
	// system 是"系统文件本身长什么样"，供 KeywordInSystemCmd / CategoryInSystemRule 判定；
	// 它与下面 data 里的合并结果必须分开：守卫要问的是"这个词在系统文件里吗"，
	// 而不是"合并后有没有"——后者会把用户自定义的指令误判成系统的。
	system = &systemCfg

	for name, entries := range systemCfg.Cmd {
		data.CmdConfigs[name] = append(data.CmdConfigs[name], entries...)
	}
	for category, rule := range systemCfg.Rules {
		data.SharedRules[category] = rule
	}

	// 合并 prompt_custom.yaml（缺失或格式错都不致命，用系统的即可）
	if customRaw, err := os.ReadFile(pc.customPath); err == nil {
		var customCfg promptFile
		if err := yaml.Unmarshal(customRaw, &customCfg); err != nil {
			logutil.Warn("prompt_custom.yaml 格式错误，跳过", "err", err)
		} else {
			for name, entries := range customCfg.Cmd {
				data.CmdConfigs[name] = append(data.CmdConfigs[name], entries...)
			}
			for category, rule := range customCfg.Rules {
				data.SharedRules[category] = rule
			}
		}
	}

	// persona 必填（无特例）：旧配置可能缺失，告警但不致命（该指令不渲染人格块）
	for name, entries := range data.CmdConfigs {
		for _, entry := range entries {
			if entry.Persona == nil {
				logutil.Warn("指令缺少 persona，将不渲染人格块", "keyword", entry.Keyword, "category", name)
			}
		}
	}
	return system, data
}

// getSystemPrompt 返回 prompt_system.yaml 解析结果（随 Reload 整体替换，读侧无锁）
func (pc *PromptConfig) getSystemPrompt() *promptFile {
	return pc.system.Load()
}

// KeywordInSystemCmd 检查 keyword 是否在 prompt_system.yaml 任意 category 中存在
func (pc *PromptConfig) KeywordInSystemCmd(keyword string) bool {
	cfg := pc.getSystemPrompt()
	if cfg == nil {
		return false
	}
	for _, entries := range cfg.Cmd {
		for _, entry := range entries {
			if entry.Keyword == keyword {
				return true
			}
		}
	}
	return false
}

// DeleteCommand 从 prompt_custom.yaml 删除指令（按 keyword 全局匹配）
func (pc *PromptConfig) DeleteCommand(keyword string) error {
	pc.writeMu.Lock()
	defer pc.writeMu.Unlock()
	raw, err := os.ReadFile(pc.customPath)
	if err != nil {
		return err
	}
	var cfg promptFile
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	for cat, entries := range cfg.Cmd {
		for i, entry := range entries {
			if entry.Keyword == keyword {
				cfg.Cmd[cat] = append(entries[:i], entries[i+1:]...)
				return writePromptCustom(pc.customPath, &cfg)
			}
		}
	}
	return fmt.Errorf("未找到该指令: %s", keyword)
}

// AddCommand 添加指令到 prompt_custom.yaml（全局 keyword 唯一，最后写入生效）
func (pc *PromptConfig) AddCommand(category, keyword, promptText string, persona *Persona) error {
	pc.writeMu.Lock()
	defer pc.writeMu.Unlock()
	var cfg promptFile
	raw, err := os.ReadFile(pc.customPath)
	if err == nil {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return err
		}
	}
	if cfg.Cmd == nil {
		cfg.Cmd = make(map[string][]CmdConf)
	}

	// 全局去重：keyword 在所有 category 中唯一，已有则移除（move 语义 / 最后写入生效）
	removedFrom := ""
	for cat, entries := range cfg.Cmd {
		filtered := entries[:0]
		for _, entry := range entries {
			if entry.Keyword != keyword {
				filtered = append(filtered, entry)
			} else if cat != category {
				removedFrom = cat
			}
		}
		if len(filtered) == 0 {
			delete(cfg.Cmd, cat)
		} else if len(filtered) != len(entries) {
			cfg.Cmd[cat] = filtered
		}
	}

	cfg.Cmd[category] = append(cfg.Cmd[category], CmdConf{Keyword: keyword, Prompt: promptText, Persona: persona})

	if removedFrom != "" {
		logutil.Info("关键字跨类别移动", "keyword", keyword, "from", removedFrom, "to", category)
	}

	return writePromptCustom(pc.customPath, &cfg)
}

// CategoryInSystemRule 检查规则 category 是否在 prompt_system.yaml 中存在
func (pc *PromptConfig) CategoryInSystemRule(category string) bool {
	cfg := pc.getSystemPrompt()
	if cfg == nil {
		return false
	}
	_, ok := cfg.Rules[category]
	return ok
}

// AddRule 添加/更新规则到 prompt_custom.yaml
func (pc *PromptConfig) AddRule(category, ruleText string) error {
	pc.writeMu.Lock()
	defer pc.writeMu.Unlock()
	var cfg promptFile
	raw, err := os.ReadFile(pc.customPath)
	if err == nil {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return err
		}
	}
	if cfg.Rules == nil {
		cfg.Rules = make(map[string]string)
	}
	cfg.Rules[category] = ruleText
	return writePromptCustom(pc.customPath, &cfg)
}

// DeleteRule 删除 prompt_custom.yaml 中的规则
func (pc *PromptConfig) DeleteRule(category string) error {
	pc.writeMu.Lock()
	defer pc.writeMu.Unlock()
	raw, err := os.ReadFile(pc.customPath)
	if err != nil {
		return err
	}
	var cfg promptFile
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	if _, ok := cfg.Rules[category]; !ok {
		return fmt.Errorf("未找到该类型规则: %s", category)
	}
	delete(cfg.Rules, category)
	return writePromptCustom(pc.customPath, &cfg)
}

// CustomPromptPath 导出 customPromptPath（供 main.go 使用）
func CustomPromptPath(systemPath string) string {
	return filepath.Join(filepath.Dir(systemPath), "prompt_custom.yaml")
}

// writePromptCustom 写入 prompt_custom.yaml，强制 prompt/rule 使用 | 格式
func writePromptCustom(path string, cfg *promptFile) error {
	var buf strings.Builder
	if len(cfg.Cmd) > 0 {
		buf.WriteString("cmd:\n")
		for catName, entries := range cfg.Cmd {
			buf.WriteString("  " + catName + ":\n")
			for _, entry := range entries {
				buf.WriteString("    - keyword: \"" + entry.Keyword + "\"\n")
				buf.WriteString("      prompt: |\n")
				for _, line := range strings.Split(entry.Prompt, "\n") {
					buf.WriteString("        " + line + "\n")
				}
				if entry.Persona != nil {
					buf.WriteString("      persona:\n")
					writePromptYAMLBlock(&buf, "        ", "identity", entry.Persona.Identity)
					writePromptJSONField(&buf, "        ", "personality", entry.Persona.Personality)
					writePromptJSONField(&buf, "        ", "relationship", entry.Persona.Relationship)
					writePromptYAMLBlock(&buf, "        ", "system_prompt", entry.Persona.SystemPrompt)
					writePromptJSONField(&buf, "        ", "emotion", entry.Persona.Emotion)
				}
			}
		}
	}
	if len(cfg.Rules) > 0 {
		buf.WriteString("rules:\n")
		for catName, rule := range cfg.Rules {
			buf.WriteString("  " + catName + ": |\n")
			for _, line := range strings.Split(rule, "\n") {
				buf.WriteString("    " + line + "\n")
			}
		}
	}
	return os.WriteFile(path, []byte(buf.String()), 0644)
}

// writePromptYAMLBlock 写 YAML 文本块（key: |- 去掉尾部换行，保证回读无拖尾 \n）
func writePromptYAMLBlock(buf *strings.Builder, indent, key, text string) {
	buf.WriteString(indent + key + ": |-\n")
	for _, line := range strings.Split(text, "\n") {
		buf.WriteString(indent + "  " + line + "\n")
	}
}

// writePromptJSONField 写单引号 JSON 字符串字段（JSON 内部单引号按 YAML 规则转义为 ”）
func writePromptJSONField(buf *strings.Builder, indent, key string, raw []byte) {
	escaped := strings.ReplaceAll(string(raw), "'", "''")
	buf.WriteString(indent + key + ": '" + escaped + "'\n")
}

// writePromptSystem 写入 prompt_system.yaml（首次启动从内嵌模板自动创建）
func writePromptSystem(path string) error {
	return os.WriteFile(path, promptSystemExampleTemplate, 0644)
}
