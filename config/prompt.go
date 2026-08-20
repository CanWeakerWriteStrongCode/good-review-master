package config

import (
	"fmt"
	"good-review-master/logutil"
	"os"
	"path/filepath"
	"strings"
	"sync"

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

// Persona 人格配置。有子字段的字段用 JSON（RawJSON）保存，LLM 自拟结构；
// 纯叙述字段（Identity/Greeting/SystemPrompt）保持 YAML 文本。
type Persona struct {
	Identity     string  `yaml:"identity" json:"identity"`             // 身份背景（必填，YAML 文本）
	Personality  RawJSON `yaml:"personality" json:"personality"`       // 性格特质（必填，JSON 数组）
	SpeechStyle  RawJSON `yaml:"speech_style" json:"speech_style"`     // 说话风格（必填，JSON 对象：语气/口癖/称呼）
	Relationship RawJSON `yaml:"relationship" json:"relationship"`     // 与群友的关系（必填，JSON 对象：角色/对待）
	Greeting     string  `yaml:"greeting" json:"greeting"`             // 开场白（必填，YAML 文本）
	SystemPrompt string  `yaml:"system_prompt" json:"system_prompt"`   // 人格级系统指令（必填，YAML 文本）
	Emotion      RawJSON `yaml:"emotion" json:"emotion"`               // 情绪维度（必填，JSON 对象：维度→取值，核心）
	Examples     RawJSON `yaml:"examples" json:"examples"`             // 示例对话（必填，JSON 数组：多组 {用户,你}）
}

// PromptConfig 提示词配置（系统 + 自定义合并），支持热重载
type PromptConfig struct {
	CmdConfigs   map[string][]CmdConf
	SharedRules  map[string]string
	mu           sync.Mutex
	systemPath   string
	customPath   string
	systemPrompt *promptFile // 缓存 prompt_system.yaml 解析结果，启动后只读
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
	pc.load()
	return pc, nil
}

// load 读取并合并提示词配置文件（调用方需持有 mu）
func (pc *PromptConfig) load() {
	pc.CmdConfigs = make(map[string][]CmdConf)
	cfgPath := pc.systemPath
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		destPath := apppath.GetWorkPath("prompt_system.yaml")
		if writeErr := writePromptSystem(destPath); writeErr != nil {
			logutil.Warn("无法创建 prompt_system.yaml，以空指令集启动", "err", writeErr)
			return
		}
		logutil.Info("已创建 prompt_system.yaml", "path", destPath)
		raw = promptSystemExampleTemplate // 使用内嵌模板字节，落入下方解析逻辑
	}
	var cfg promptFile
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		logutil.Warn("prompt_system.yaml 格式错误，将以空指令集启动", "err", err)
		return
	}
	if cfg.Cmd == nil {
		cfg.Cmd = make(map[string][]CmdConf)
	}
	pc.systemPrompt = &cfg // 缓存系统提示词，后续只读不重新解析
	pc.CmdConfigs = cfg.Cmd
	pc.SharedRules = cfg.Rules

	// 合并 prompt_custom.yaml
	customRaw, err := os.ReadFile(pc.customPath)
	if err != nil {
		return
	}
	var customCfg promptFile
	if err := yaml.Unmarshal(customRaw, &customCfg); err != nil {
		logutil.Warn("prompt_custom.yaml 格式错误，跳过", "err", err)
		return
	}
	for name, entries := range customCfg.Cmd {
		pc.CmdConfigs[name] = append(pc.CmdConfigs[name], entries...)
	}
	if customCfg.Rules != nil {
		if pc.SharedRules == nil {
			pc.SharedRules = make(map[string]string)
		}
		for cat, rule := range customCfg.Rules {
			pc.SharedRules[cat] = rule
		}
	}

	// persona 必填（无特例）：旧配置可能缺失，告警但不致命（该指令不渲染人格块）
	for name, entries := range pc.CmdConfigs {
		for _, entry := range entries {
			if entry.Persona == nil {
				logutil.Warn("指令缺少 persona，将不渲染人格块", "keyword", entry.Keyword, "category", name)
			}
		}
	}
}

// Reload 热重载提示词配置
func (pc *PromptConfig) Reload() {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.load()
}

// getSystemPrompt 返回缓存 prompt_system.yaml 解析结果（启动后只读，不需要每次读文件）
func (pc *PromptConfig) getSystemPrompt() *promptFile {
	return pc.systemPrompt
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
	pc.mu.Lock()
	defer pc.mu.Unlock()
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
	pc.mu.Lock()
	defer pc.mu.Unlock()
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
	pc.mu.Lock()
	defer pc.mu.Unlock()
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
	pc.mu.Lock()
	defer pc.mu.Unlock()
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
					writePromptJSONField(&buf, "        ", "speech_style", entry.Persona.SpeechStyle)
					writePromptJSONField(&buf, "        ", "relationship", entry.Persona.Relationship)
					writePromptYAMLBlock(&buf, "        ", "greeting", entry.Persona.Greeting)
					writePromptYAMLBlock(&buf, "        ", "system_prompt", entry.Persona.SystemPrompt)
					writePromptJSONField(&buf, "        ", "emotion", entry.Persona.Emotion)
					writePromptJSONField(&buf, "        ", "examples", entry.Persona.Examples)
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

// writePromptJSONField 写单引号 JSON 字符串字段（JSON 内部单引号按 YAML 规则转义为 ''）
func writePromptJSONField(buf *strings.Builder, indent, key string, raw []byte) {
	escaped := strings.ReplaceAll(string(raw), "'", "''")
	buf.WriteString(indent + key + ": '" + escaped + "'\n")
}

// writePromptSystem 写入 prompt_system.yaml（首次启动从内嵌模板自动创建）
func writePromptSystem(path string) error {
	return os.WriteFile(path, promptSystemExampleTemplate, 0644)
}
