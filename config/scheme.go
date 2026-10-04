package config

// 全应用唯一的域注册表：一眼看全有哪些配置域。
//
// 新增一个配置域 = 加一个 section_*.go + 在下面结构体里加一个字段 + 在 registry() 里加一行。
// 改造前加一个配置项要在 configFile 里加字段、在 LoadConfig 中段加默认值、在返回字面量里搬一次，
// 三处都要记得改，漏一处就是静默失效。
//
// newSections 是**构造函数**而不是包级共享实例，这一点是被阶段 3 的 informer 倒逼的：
// informer 会反复调 Load（文件一变就调，还有 resync 兜底），域实例每次都必须是新的，
// 否则 Unmarshal/SetDefaults 会在同一批对象上反复叠加。
type sections struct {
	NapCat  *napcatSection
	Bot     *botSection
	Runtime *runtimeSection
	LLM     *llmSection
	MCP     *mcpSection

	// Secret 刻意不在上面那张表里，也不由 registry() 返回：
	// 它的来源不是 config.yaml，而是 secret.yaml + 环境变量（见 section_secret.go）。
	// 硬塞进同一个循环会把"读哪份文档"这件事藏起来，反而更难懂。
	Secret *secretSection
}

func newSections() *sections {
	return &sections{
		NapCat:  &napcatSection{},
		Bot:     &botSection{},
		Runtime: &runtimeSection{},
		LLM:     &llmSection{},
		MCP:     &mcpSection{},
		Secret:  &secretSection{},
	}
}

// registry 返回全部读 config.yaml 的域，顺序即四步曲的执行顺序。
func (s *sections) registry() []Section {
	return []Section{s.NapCat, s.Bot, s.Runtime, s.LLM, s.MCP}
}
