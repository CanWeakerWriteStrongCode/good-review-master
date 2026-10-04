// Command eval 是提示词的离线评测集。
//
// 它回答的问题是：**改了 prompt / 换了模型 / 动了选窗逻辑之后，回复质量有没有变差？**
// 单元测试管不了这件事——模型输出没有唯一正确答案，断言不了"必须等于某句话"。
// 所以这里的做法是：
//
//  1. 每条用例按**与线上逐字相同**的提示词调用一次真实大模型
//     （system prompt 走 router.ComposeSystemPrompt，user message 走 router.BuildUserMsg，
//     就是为了不出现"评测自己拼一份、线上回归了评测还是绿的"）；
//  2. 跑几条**规则检查**（长度上限、必须/禁止出现的串）——这些是硬失败；
//  3. 与 baseline.json 逐行比对，把差异打出来给人看——模型输出天然会变，
//     所以差异只报告、不判失败，由人决定是"变差了"还是"变好了"。
//
// 它是测试设施，不参与运行时装配，所以住在 tests/ 下（与 tests/e2e 并列）。
//
// 用法见 tests/eval/README.md。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"good-review-master/config"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/router"

	"gopkg.in/yaml.v3"
)

// checkCase 一条用例。
type checkCase struct {
	ID     string    `yaml:"id"`
	Note   string    `yaml:"note"`
	Prompt promptRef `yaml:"prompt"`
	Input  string    `yaml:"input"`
	Checks []check   `yaml:"checks"`
}

// promptRef 指向 prompt_system.yaml / prompt_custom.yaml 里的一条已配置指令。
// 刻意用"引用真实提示词"而不是把提示词内联进用例：评测要防的就是这两个文件的回归，
// 内联一份副本只会让两边各自漂移。
type promptRef struct {
	Category string `yaml:"category"`
	Keyword  string `yaml:"keyword"`
}

// check 一条规则检查。kind 目前支持：max_runes / min_runes / contains / not_contains。
type check struct {
	Kind  string `yaml:"kind"`
	Value any    `yaml:"value"`
	Note  string `yaml:"note"`
}

// caseFile 一个用例文件（cases/*.yaml）。
type caseFile struct {
	Cases []checkCase `yaml:"cases"`
}

// baselineEntry 基线里一条用例的记录。
type baselineEntry struct {
	Model string `json:"model"`
	// PromptSHA256 是这条用例**实际发出去的提示词**（system + user）的指纹。
	//
	// 这就是"prompt 版本化"：没有它，输出变了只能看到"不一样了"，
	// 分不清是模型漂了还是提示词被改了——而这两种情况的处置完全不同
	// （前者要查上游/换模型，后者要 review 这次改动）。有了它，
	// 提示词变了就直接在报告里写明"差异由提示词变化解释"。
	PromptSHA256     string `json:"prompt_sha256"`
	Output           string `json:"output"`
	Runes            int    `json:"runes"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	RecordedAt       string `json:"recorded_at"`
}

type baseline map[string]baselineEntry

func main() {
	configPath := flag.String("config", "config.yaml", "config.yaml 路径（相对仓库根目录）")
	secretPath := flag.String("secret", "secret.yaml", "secret.yaml 路径")
	promptPath := flag.String("prompt", "prompt_system.yaml", "prompt_system.yaml 路径")
	casesDir := flag.String("cases", filepath.Join("tests", "eval", "cases"), "用例目录（读其中的 *.yaml）")
	baselinePath := flag.String("baseline", filepath.Join("tests", "eval", "baseline.json"), "基线文件路径")
	botNickname := flag.String("nickname", "机器人", "评测时使用的机器人昵称（真实的昵称来自 NapCat，离线拿不到）")
	update := flag.Bool("update", false, "把本次输出写成新基线（人工确认后才用）")
	dryRun := flag.Bool("dry", false, "只解析并打印将要发送的提示词，不调用大模型、不比对基线")
	timeout := flag.Duration("timeout", 120*time.Second, "单条用例的超时")
	flag.Parse()

	logutil.SetupLogger()
	defer logutil.Close()

	cfg, err := config.Load(config.Sources{Config: *configPath, Secret: *secretPath})
	if err != nil {
		fail("加载配置失败：%v", err)
	}
	cfg.BotNickname = *botNickname

	promptCfg, err := config.LoadPromptConfig(*promptPath, config.CustomPromptPath(*promptPath))
	if err != nil {
		fail("加载提示词失败：%v", err)
	}

	cases, err := loadCases(*casesDir)
	if err != nil {
		fail("加载用例失败：%v", err)
	}
	if len(cases) == 0 {
		fail("用例目录 %s 下没有任何用例", *casesDir)
	}

	fmt.Printf("用例 %d 条 | 模型 %s | prompt %s\n\n", len(cases), cfg.LLMConfig.ModelName, *promptPath)

	if *dryRun {
		for _, one := range cases {
			systemPrompt, keywordPrompt, err := resolvePrompt(promptCfg, cfg, one)
			if err != nil {
				fail("用例 %s：%v", one.ID, err)
			}
			fmt.Printf("═══ %s ═══\n", one.ID)
			fmt.Printf("--- system prompt ---\n%s\n", systemPrompt)
			fmt.Printf("--- user message ---\n%s\n\n", router.BuildUserMsg(one.Input, keywordPrompt))
		}
		return
	}

	client := llm.NewOpenAIAdapter(cfg.LLMConfig.APIKey, cfg.LLMConfig.APIBase,
		cfg.LLMConfig.ModelName, cfg.LLMConfig.Temperature, cfg.LLMConfig.TopP)

	previous := loadBaseline(*baselinePath)
	next := baseline{}
	results := make([]caseResult, 0, len(cases))

	for _, one := range cases {
		result := runCase(client, promptCfg, cfg, one, *timeout)
		// 只有真的拿到输出才写基线：调用失败时写进去会把一次网络故障
		// 固化成"这是模型的正常表现"，之后所有比对都失去意义。
		if result.err == nil {
			next[one.ID] = baselineEntry{
				Model:            cfg.LLMConfig.ModelName,
				PromptSHA256:     result.promptSHA256,
				Output:           result.output,
				Runes:            len([]rune(result.output)),
				PromptTokens:     result.promptTokens,
				CompletionTokens: result.completionTokens,
				RecordedAt:       time.Now().Format(time.RFC3339),
			}
			if old, ok := previous[one.ID]; ok {
				result.diff = diffLines(splitLines(old.Output), splitLines(result.output))
				// 提示词指纹不同 = 这次的差异是"我改了提示词"造成的，而不是"模型漂了"。
				// 两种情况的处置完全不同（一个要 review 改动，一个要查上游），
				// 所以报告里必须分开说，不能笼统地报一句"有差异"。
				// 老基线没有这个字段（空串）时不做判断，免得把升级本身报成"提示词变了"。
				result.promptChanged = old.PromptSHA256 != "" &&
					old.PromptSHA256 != result.promptSHA256
			}
		}
		results = append(results, result)
	}

	failed := 0
	for _, one := range results {
		fmt.Printf("═══ %s ═══\n", one.id)
		if one.err != nil {
			fmt.Printf("  [错误] %v\n", one.err)
			failed++
			continue
		}
		if len(one.checkFailures) == 0 {
			fmt.Printf("  规则检查：全部通过\n")
		} else {
			for _, failure := range one.checkFailures {
				fmt.Printf("  [失败] %s\n", failure)
			}
			failed++
		}
		fmt.Printf("  用量：%s\n", one.tokens)
		fmt.Printf("  提示词指纹：%s%s\n", shortHash(one.promptSHA256), promptChangeNote(one.promptChanged))
		if len(one.diff) == 0 {
			fmt.Printf("  与基线：一致\n")
		} else {
			fmt.Printf("  与基线：有差异（模型输出本来就会变，请人工看一眼）\n")
			for _, line := range one.diff {
				fmt.Printf("    %s\n", line)
			}
		}
		fmt.Println()
	}

	if *update {
		if err := writeBaseline(*baselinePath, next); err != nil {
			fail("写基线失败：%v", err)
		}
		fmt.Printf("基线已更新：%s（%d 条）\n", *baselinePath, len(next))
	} else if len(previous) == 0 {
		fmt.Printf("提示：%s 还不存在，用 -update 生成第一份基线并提交入库\n", *baselinePath)
	}

	if failed > 0 {
		fmt.Printf("\n结论：%d/%d 条用例未通过规则检查\n", failed, len(results))
		os.Exit(1)
	}
	fmt.Printf("\n结论：%d 条用例规则检查全部通过\n", len(results))
}

// caseResult runCase 的产物（含内部字段，不直接打印）。
type caseResult struct {
	id               string
	output           string
	promptSHA256     string
	promptTokens     int
	completionTokens int
	checkFailures    []string
	diff             []string
	promptChanged    bool
	tokens           string
	err              error
}

// runCase 跑一条用例：解析提示词 → 调大模型 → 规则检查。
func runCase(client llm.Client, promptCfg *config.PromptConfig, cfg *config.Config, one checkCase, timeout time.Duration) caseResult {
	result := caseResult{id: one.ID, tokens: "-"}

	systemPrompt, keywordPrompt, err := resolvePrompt(promptCfg, cfg, one)
	if err != nil {
		result.err = err
		return result
	}
	userMessage := router.BuildUserMsg(one.Input, keywordPrompt)
	result.promptSHA256 = promptFingerprint(systemPrompt, userMessage)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// 直接调 MutiChatWithTool 而不是 SingleChat：两者的请求体完全一样
	// （SingleChat 就是拿这两条消息去调它），但它会带回 usage，
	// 于是评测能报真实 token 花费，不必用本地估算去猜。
	response, err := client.MutiChatWithTool(ctx, []llm.Message{
		{Role: llm.RoleSystem, Content: systemPrompt},
		{Role: llm.RoleUser, Content: userMessage},
	}, nil)
	if err != nil {
		result.err = fmt.Errorf("调用大模型失败：%w", err)
		return result
	}

	result.output = response.Content
	result.promptTokens = response.PromptTokens
	result.completionTokens = response.CompletionTokens
	if response.PromptTokens > 0 || response.CompletionTokens > 0 {
		result.tokens = fmt.Sprintf("prompt %d / completion %d", response.PromptTokens, response.CompletionTokens)
	} else {
		// 有些中转不回 usage，这时说明"上游没给"，而不是"花费是 0"
		result.tokens = "上游未返回 usage"
	}

	result.checkFailures = evaluate(one.Checks, response.Content)
	return result
}

// resolvePrompt 按 category+keyword 从真实提示词里取出这次要用的
// system prompt 与关键词提示词，与线上路由的组装方式一致。
func resolvePrompt(promptCfg *config.PromptConfig, cfg *config.Config, one checkCase) (systemPrompt, keywordPrompt string, err error) {
	data := promptCfg.Snapshot()
	entries, ok := data.CmdConfigs[one.Prompt.Category]
	if !ok {
		return "", "", fmt.Errorf("提示词里没有类别 %q（现有类别：%s）",
			one.Prompt.Category, strings.Join(sortedKeys(data.CmdConfigs), ", "))
	}
	for _, entry := range entries {
		if entry.Keyword != one.Prompt.Keyword {
			continue
		}
		personaBlock := ""
		if entry.Persona != nil {
			personaBlock = router.RenderPersona(*entry.Persona, data.SharedRules[one.Prompt.Category])
		}
		return router.ComposeSystemPrompt(cfg, personaBlock), entry.Prompt, nil
	}
	keywords := make([]string, 0, len(entries))
	for _, entry := range entries {
		keywords = append(keywords, entry.Keyword)
	}
	return "", "", fmt.Errorf("类别 %q 下没有关键字 %q（现有：%s）",
		one.Prompt.Category, one.Prompt.Keyword, strings.Join(keywords, ", "))
}

// evaluate 跑规则检查，返回全部失败描述（空表示通过）。
func evaluate(checks []check, output string) []string {
	var failures []string
	for _, item := range checks {
		label := item.Kind
		if item.Note != "" {
			label += "（" + item.Note + "）"
		}
		switch item.Kind {
		case "max_runes":
			limit := toInt(item.Value)
			if runes := len([]rune(output)); runes > limit {
				failures = append(failures, fmt.Sprintf("%s：输出 %d 字，超过上限 %d", label, runes, limit))
			}
		case "min_runes":
			limit := toInt(item.Value)
			if runes := len([]rune(output)); runes < limit {
				failures = append(failures, fmt.Sprintf("%s：输出只有 %d 字，低于下限 %d", label, runes, limit))
			}
		case "contains":
			text := toStr(item.Value)
			if !strings.Contains(output, text) {
				failures = append(failures, fmt.Sprintf("%s：输出里没有 %q", label, text))
			}
		case "not_contains":
			text := toStr(item.Value)
			if strings.Contains(output, text) {
				failures = append(failures, fmt.Sprintf("%s：输出里出现了不该有的 %q", label, text))
			}
		default:
			failures = append(failures, fmt.Sprintf("未知的检查类型 %q（支持 max_runes / min_runes / contains / not_contains）", item.Kind))
		}
	}
	return failures
}

// loadCases 读取目录下所有 *.yaml 的用例，按文件名、再按声明顺序排列
// （稳定顺序很重要：报告和基线都是按这个顺序产出的）。
func loadCases(dir string) ([]checkCase, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)

	var cases []checkCase
	seen := map[string]string{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var file caseFile
		if err := yaml.Unmarshal(raw, &file); err != nil {
			return nil, fmt.Errorf("%s 解析失败：%w", path, err)
		}
		for _, one := range file.Cases {
			if one.ID == "" {
				return nil, fmt.Errorf("%s 里有用例没写 id", path)
			}
			if where, dup := seen[one.ID]; dup {
				return nil, fmt.Errorf("用例 id %q 重复：%s 与 %s", one.ID, where, path)
			}
			seen[one.ID] = path
			cases = append(cases, one)
		}
	}
	return cases, nil
}

func loadBaseline(path string) baseline {
	raw, err := os.ReadFile(path)
	if err != nil {
		return baseline{} // 还没有基线是正常状态（第一次跑）
	}
	var parsed baseline
	if err := json.Unmarshal(raw, &parsed); err != nil {
		fail("基线文件 %s 解析失败：%v（可以删掉它重新用 -update 生成）", path, err)
	}
	return parsed
}

func writeBaseline(path string, data baseline) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o644)
}

// promptFingerprint 给"实际发出去的提示词"算一个指纹。
//
// 用 sha256 而不是存原文：基线文件是给人看的，把几十行提示词逐条塞进去会淹没真正要看的输出。
// 两段之间加个分隔符，免得 "ab"+"c" 与 "a"+"bc" 撞成同一个指纹。
func promptFingerprint(systemPrompt, userMessage string) string {
	sum := sha256.Sum256([]byte(systemPrompt + "\x00" + userMessage))
	return hex.EncodeToString(sum[:])
}

// shortHash 只显示前 8 位：报告里够用了，全 64 位反而难对照。
func shortHash(hash string) string {
	if len(hash) <= 8 {
		return hash
	}
	return hash[:8]
}

// promptChangeNote 在指纹与基线不同时说明"差异从哪来"。
// 这句话是这一整套机制存在的理由：没有它，输出变了只能是"不一样了"，
// 而"我改了提示词"和"上游模型漂了"要做的处置完全相反。
func promptChangeNote(changed bool) string {
	if changed {
		return "  ← 与基线不同：这次的输出差异由此解释（改了提示词）"
	}
	return ""
}

// splitLines 按行切分，忽略结尾的换行造成的空行。
func splitLines(text string) []string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// diffLines 输出逐行差异。用的是最朴素的 LCS 动态规划——
// 用例输出只有几行到几十行，没必要为它引一个 diff 库。
func diffLines(oldLines, newLines []string) []string {
	rows, cols := len(oldLines), len(newLines)
	table := make([][]int, rows+1)
	for i := range table {
		table[i] = make([]int, cols+1)
	}
	for i := rows - 1; i >= 0; i-- {
		for j := cols - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				table[i][j] = table[i+1][j+1] + 1
			} else if table[i+1][j] >= table[i][j+1] {
				table[i][j] = table[i+1][j]
			} else {
				table[i][j] = table[i][j+1]
			}
		}
	}

	var out []string
	i, j := 0, 0
	for i < rows && j < cols {
		switch {
		case oldLines[i] == newLines[j]:
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			out = append(out, "基线 - "+oldLines[i])
			i++
		default:
			out = append(out, "本次 + "+newLines[j])
			j++
		}
	}
	for ; i < rows; i++ {
		out = append(out, "基线 - "+oldLines[i])
	}
	for ; j < cols; j++ {
		out = append(out, "本次 + "+newLines[j])
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// toInt / toStr 处理 YAML 解出来的 any：yaml.v3 对整数给 int，
// 但写成 "500" 这样的字符串也要能用（配置文件里两种写法都常见）。
func toInt(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		var parsed int
		if _, err := fmt.Sscanf(typed, "%d", &parsed); err == nil {
			return parsed
		}
	}
	fail("检查项的 value 不是整数：%v", value)
	return 0
}

func toStr(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
