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

// defaultAddPersonaPrompt 是 #添加人格 生成提示词+人格时用的系统提示词兜底：
// 正常从 prompt_system.yaml 的 internal.add_persona 读取；老配置文件没有这一段时用这份，
// 避免升级后生成功能直接失效。
const defaultAddPersonaPrompt = `你是一个提示词工程师兼人格设计师。根据用户的要求，输出一个 JSON 对象，包含：
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
要求：persona 全部字段必须补全，不得缺省；只输出 JSON，不要多余解释。
如果用户没有指定回复长度，在生成的 prompt 里限制回复在 50 字左右；查询资料或新闻时可以扩展到 300 字左右。`

// defaultProactiveJudgePrompt 是主动插话裁判（判断"要不要回"）的系统提示词兜底：
// 正常从 prompt_system.yaml 的 internal.proactive_judge 读取；老配置文件没有这一段时用这份。
// 内容必须逐字节稳定：裁判调用很频繁，固定的 system 前缀才能持续命中缓存。
const defaultProactiveJudgePrompt = `你是群聊机器人的发言决策器，只判断"要不要回"，不写回复内容。
【默认克制】没有明确理由就不回；但不必每条都追求"有价值"——群里气氛合适时，偶尔凑个趣、接个梗让聊天更热闹也可以。宁可少说，不要连续刷存在感。
【先看对象】@ 的不是机器人自己（@ 别人、@ 其他机器人、@所有人）就不回；@昵称(ID) 与 <@ID> 以括号里的 ID 判断是不是自己。

可以回（满足任一）：
1. 在跟机器人说话：叫昵称、接着机器人上一句追问；
2. 向全群求助或提问，且机器人凭知识或工具能给出实质答案（在问某个具体群友、或明显在等别人回答的不算）；
3. 与机器人职责相关：查新闻、锐评、解释规则、回顾群历史；
4. 机器人正在参与且话题未结束，补充有明显价值；
5. 图片是新闻截图、带文字截图或图表，且与上下文相关；
6. 大家正轻松玩梗、开玩笑，能自然接一句让气氛更好（偶尔为之，别连续跟）；
7. 群友话里有"求回应"的潜台词：分享趣事、自嘲、吐槽、抛梗，明显希望大家捧个场、
   接一句或吐槽回去——这种就回一下，别让对方冷场。

不要回（命中任一）：
1. 两人私聊式对话：互相称呼、连续来回、只涉及彼此；
2. 纯自言自语、公告、没有互动期待的陈述（不指望任何人接话）；
3. 已经有人回答或正在回答；
4. 刷屏、接龙、斗图、表情包刷屏、争吵起哄——连续玩梗不要跟；
5. 机器人刚说完，没人继续追问；
6. 回了也只能复读或说"不知道"；
7. 个人隐私、敏感话题。

还要给出 focus（不回时填"整体上下文"即可）：
- "最后一条消息"：最后一条是独立提问、追问或点名，或与前文无关，只需回应它；
- "整体上下文"：话题在延续、需要承接前文，或最后一条只有结合上文才看得懂，按整段语境回应。

还要给出 long_form（是否需要查资料 / 正式知识型长篇解答）：
- true：需要查证事实、新闻、数据、规则，或在解释概念、教程、对比等必须完整展开的问题；
- false：日常闲聊、接梗、简短问答。

只输出 JSON，不要代码块、不要解释：{"reply": true 或 false, "long_form": true 或 false, "focus": "整体上下文 或 最后一条消息", "reason": "不超过 20 字的理由"}`

// defaultFocusContext / defaultFocusLast / defaultFocusMention 是生成回复时追加的
// "回复焦点"提示词兜底（正常从 internal.focus_context / focus_last / focus_mention 读取）：
// 群里被点名用 focus_mention（只回最后一条找机器人的消息）；私聊与直接 @ 以外的
// 直答用 focus_last；主动接话按裁判给出的 focus 二选一。
const (
	defaultFocusContext = "【回复焦点】结合整体上下文：先理解整段聊天的话题和来龙去脉，再回应最后一条消息，不要只盯着最后一句孤立作答。"
	defaultFocusLast    = "【回复焦点】只回复最后一条消息，不要回答多余的内容；前文仅用于理解语境。"
	defaultFocusMention = "【回复焦点】只回复最后一条找你（@ 你或叫你昵称）的消息，不要回答更早的消息；前文仅用于理解语境。"
)

// defaultReplyRequirement 是 @ 回复时 system prompt 里"【要求】"句的兜底：
// 正常从 prompt_system.yaml 的 internal.reply_requirement 读取，老配置文件没有这一段时用这份。
const defaultReplyRequirement = "【要求】发给你的内容是聊天记录，继续聊天或者执行指令后回复。"

// defaultReplyRewrite 是"最终回复再排一次版"的系统提示词兜底：
// 模型出稿后，连同群聊记录一起发回来，用 JSON schema 约束成最多 N 句短句（N 在 user 消息里给）。
const defaultReplyRewrite = `你是群聊回复的排版器。用户消息里有群聊记录和机器人刚生成的一条回复，结合上下文理解后把它改写成短句（句数上限以用户消息为准）：
- 句数由内容长短决定：短就 1~2 句，长才多拆几句；不要为了凑数把一句话硬拆成几句；
- 保持原回复的语气、人称和人格色彩：只调整断句和长度，不要改成生硬的书面语、客服腔或新闻播报腔；
- 可以适当删减，以应对多个短句的形式；
- 每句尽量短（一般不超过 50 字），口语化，像真人在群里发消息；
- 只输出 JSON，不要解释。`

// defaultProactiveRequirement 是主动插话（裁判判定要回后）生成回复时，
// system prompt 里"【要求】"那句的兜底：正常从 prompt_system.yaml 的
// internal.proactive_requirement 读取，老配置文件没有这一段时用这份。
const defaultProactiveRequirement = "【要求】发给你的内容是聊天记录。你判断值得参与后才被唤起（主动参与群聊），" +
	"请自然、简短地接话；不要复述别人说过的内容，也不要 @ 任何人。"

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
	// Internal 内置指令自己的提示词（如 add_persona），只从 prompt_system.yaml 读，不合并自定义文件
	Internal map[string]string `yaml:"internal"`
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
	system := &promptFile{Cmd: make(map[string][]CmdConf), Rules: make(map[string]string), Internal: make(map[string]string)}
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
	if systemCfg.Internal == nil {
		systemCfg.Internal = make(map[string]string)
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

// AddPersonaPrompt 返回 #添加人格 调用大模型时使用的系统提示词。
func (pc *PromptConfig) AddPersonaPrompt() string {
	return pc.internalPrompt("add_persona", defaultAddPersonaPrompt)
}

// ProactiveJudgePrompt 返回主动插话裁判（判断"要不要回"）的系统提示词。
func (pc *PromptConfig) ProactiveJudgePrompt() string {
	return pc.internalPrompt("proactive_judge", defaultProactiveJudgePrompt)
}

// ReplyRequirement 返回 @ 回复时 system prompt 里的【要求】句。
func (pc *PromptConfig) ReplyRequirement() string {
	return pc.internalPrompt("reply_requirement", defaultReplyRequirement)
}

// ProactiveRequirement 返回主动插话生成回复时 system prompt 里的【要求】句。
func (pc *PromptConfig) ProactiveRequirement() string {
	return pc.internalPrompt("proactive_requirement", defaultProactiveRequirement)
}

// ReplyRewritePrompt 返回"把最终回复改写成短句"请求的系统提示词。
// 句数上限不写在这里：它随配置变化，放在 user 消息里，system 保持稳定以命中缓存。
func (pc *PromptConfig) ReplyRewritePrompt() string {
	return pc.internalPrompt("reply_rewrite", defaultReplyRewrite)
}

// FocusPrompt 返回生成回复时追加的"回复焦点"提示词：
// focusLast=true 聚焦最后一条（私聊等没有 @ 语义的直答用它），否则按整体上下文。
func (pc *PromptConfig) FocusPrompt(focusLast bool) string {
	if focusLast {
		return pc.internalPrompt("focus_last", defaultFocusLast)
	}
	return pc.internalPrompt("focus_context", defaultFocusContext)
}

// MentionFocusPrompt 返回群里被点名（@ 或叫昵称）直答时的焦点提示词：
// 只回最后一条找机器人的消息，别翻回更早的 @ 作答。
func (pc *PromptConfig) MentionFocusPrompt() string {
	return pc.internalPrompt("focus_mention", defaultFocusMention)
}

// internalPrompt 取 prompt_system.yaml 的 internal.<key>；缺失或为空时退回内置默认值，
// 让升级前创建的 prompt_system.yaml 继续可用。
func (pc *PromptConfig) internalPrompt(key, fallback string) string {
	if cfg := pc.getSystemPrompt(); cfg != nil {
		if p := strings.TrimSpace(cfg.Internal[key]); p != "" {
			return p
		}
	}
	return fallback
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
