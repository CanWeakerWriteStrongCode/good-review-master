package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"good-review-master/logutil"
)

// groupList 是 allow_groups 的解码类型：同时接受两种 YAML 写法。
//
//	allow_groups: "10001,10002"   # 历史形态：逗号分隔字符串
//	allow_groups:                 # 列表形态（允许逐项写注释）
//	  - "10001"
//	  - "10002"
//
// 两种统一归一成逗号分隔字符串，下游 parseCommaList 不用感知差异；
// 保留字符串形态是为了存量配置不必改。
type groupList string

func (g *groupList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*g = groupList(node.Value)
		return nil
	case yaml.SequenceNode:
		var items []string
		if err := node.Decode(&items); err != nil {
			return err
		}
		trimmed := make([]string, 0, len(items))
		for _, item := range items {
			if item = strings.TrimSpace(item); item != "" {
				trimmed = append(trimmed, item)
			}
		}
		*g = groupList(strings.Join(trimmed, ","))
		return nil
	default:
		return fmt.Errorf("allow_groups 只支持逗号分隔字符串或 YAML 列表，实际是 %s", node.Tag)
	}
}

// String 返回归一后的逗号分隔写法。
func (g groupList) String() string { return string(g) }

// workspaceIDList 是 allow_workspace_accounts 的解码类型，与 groupList 同理：
// 同时接受逗号分隔字符串和 YAML 列表，两种写法归一成同一个切片。
//
//	allow_workspace_accounts: "qq_official_bot_1906xxxx,qq_official_bot_1904xxxx"
//	allow_workspace_accounts:
//	  - "qq_official_bot_1906xxxx"
//	  - "qq_official_bot_1904xxxx"
type workspaceIDList []string

func (l *workspaceIDList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*l = workspaceIDList(parseCommaList(node.Value))
		return nil
	case yaml.SequenceNode:
		var items []string
		if err := node.Decode(&items); err != nil {
			return err
		}
		trimmed := make([]string, 0, len(items))
		for _, item := range items {
			if item = strings.TrimSpace(item); item != "" {
				trimmed = append(trimmed, item)
			}
		}
		*l = workspaceIDList(trimmed)
		return nil
	default:
		return fmt.Errorf("allow_workspace_accounts 只支持逗号分隔字符串或 YAML 列表，实际是 %s", node.Tag)
	}
}

// botSection 机器人白名单（config.yaml 的 bot 段）
type botSection struct {
	// AllowGroups 群白名单，支持逗号分隔字符串与 YAML 列表两种写法（见 groupList）。
	// 填 "*" 表示允许所有群；空串/空列表表示不响应任何群。
	AllowGroups groupList `yaml:"allow_groups"`

	// AllowWorkspaceAccounts 允许**自动开通**的工作空间 ID（platform_account_kind_account），
	// 填 "*" 表示全部；空列表表示一个都不自动开通（安全默认）。
	// 支持逗号分隔字符串与 YAML 列表两种写法（见 workspaceIDList）。
	//
	// 只控制新建：已经有目录的工作空间不受名单限制，删名字不会让它停止工作。
	// 必须匹配完整 ID 而不是单独的 account：不同平台可能出现相同的 account。
	AllowWorkspaceAccounts workspaceIDList `yaml:"allow_workspace_accounts"`

	// ---- 主动插话（非 @ 时由大模型裁决要不要接话） ----
	// 全局一份，所有工作空间共用；关闭时行为与改造前完全一致。

	// ProactiveEnabled 主动插话总开关。默认 false。
	ProactiveEnabled bool `yaml:"proactive_enabled"`
	// 裁判看的窗口与生成一致：都用 runtime.llm_send_count 条，不再单独配置。
	// ProactiveCooldownSec 机器人主动发言后，该群多少秒内不再判定。默认 90；显式 0 = 不冷却。
	// 用指针区分"没写（用默认值）"与"显式写 0"。
	ProactiveCooldownSec *int `yaml:"proactive_cooldown_sec"`
	// ProactiveMaxStreak 同一话题最多连续主动发言次数。默认 5；显式 0 = 不主动发言。
	ProactiveMaxStreak *int `yaml:"proactive_max_streak"`
	// ProactiveJudgeIntervalSec 两次判定的最小间隔（秒）。默认 20；显式 0 = 不设最小间隔。
	// 从"上一次裁判开始"计时，含该轮裁判+生成的大模型耗时：单轮耗时 ≥ 间隔时，
	// 下一轮会立刻放行；间隔内到达的消息直接丢弃（不排队延后）。
	// 主动发言后的冷却（ProactiveCooldownSec）优先于它，冷却期内根本不判定。
	ProactiveJudgeIntervalSec *int `yaml:"proactive_judge_interval_sec"`
	// ProactiveJudgeTimeoutSec 单次判定调用超时（秒）。默认 20；显式 0 = 沿用 runtime.llm_timeout_sec。
	ProactiveJudgeTimeoutSec *int `yaml:"proactive_judge_timeout_sec"`
	// ProactiveJudgeImages 每次判定最多附几张图（纯图片消息）。默认 1；0 = 图片消息不判定。
	ProactiveJudgeImages *int `yaml:"proactive_judge_images"`
}

func (s *botSection) Name() string { return "bot" }

func (s *botSection) Unmarshal(document *yaml.Node) error {
	return decodeSection(document, s.Name(), s)
}

// SetDefaults 只给"没写"的项补默认值。显式写 0 是有效配置（不冷却/不设间隔/
// 不主动发言等），不能再被默认值覆盖——用指针区分"没写"和"写了 0"。
func (s *botSection) SetDefaults() {
	setDefault := func(target **int, def int) {
		if *target == nil {
			value := def
			*target = &value
		}
	}
	setDefault(&s.ProactiveCooldownSec, 90)
	setDefault(&s.ProactiveMaxStreak, 5)
	setDefault(&s.ProactiveJudgeIntervalSec, 20)
	setDefault(&s.ProactiveJudgeTimeoutSec, 20)
	setDefault(&s.ProactiveJudgeImages, 1)
}

// Validate 只做"不可能工作"的判定。
//
// 刻意**不**把 allow_groups 为空做成硬错误：缺失时程序能跑，只是不响应任何消息。
// 把它升级成启动失败会让"改配置时把白名单清空试一下"这种正常操作变成起不来，
// 代价大于收益——所以只告警。
func (s *botSection) Validate() error {
	groups := parseCommaList(s.AllowGroups.String())
	if len(groups) == 0 {
		logutil.Warn("bot.allow_groups 为空，机器人不会响应任何群消息")
	}
	for _, group := range groups {
		if group == "*" {
			logutil.Warn("bot.allow_groups 包含 *：所有群都会响应，注意大模型调用成本")
			break
		}
	}

	if len(s.AllowWorkspaceAccounts) == 0 {
		logutil.Warn("bot.allow_workspace_accounts 为空：不会自动开通新工作空间（档案里已有的仍会恢复）")
	}
	seen := make(map[string]struct{}, len(s.AllowWorkspaceAccounts))
	for i, account := range s.AllowWorkspaceAccounts {
		account = strings.TrimSpace(account)
		s.AllowWorkspaceAccounts[i] = account
		if account == "" {
			return fmt.Errorf("bot.allow_workspace_accounts[%d] 不能为空", i)
		}
		if account != "*" && !workspaceIDPattern.MatchString(account) {
			return fmt.Errorf("bot.allow_workspace_accounts[%d]=%q 不是合法的工作空间 ID（字母/数字/_/-，最多 128 字符，例如 qq_official_bot_1906xxxx）", i, account)
		}
		if _, ok := seen[account]; ok {
			return fmt.Errorf("bot.allow_workspace_accounts 中重复：%s", account)
		}
		seen[account] = struct{}{}
	}

	// 主动插话的数值项：0 是有效值（不冷却/不设间隔等），负数没有意义。
	for _, item := range []struct {
		name  string
		value *int
	}{
		{"proactive_cooldown_sec", s.ProactiveCooldownSec},
		{"proactive_max_streak", s.ProactiveMaxStreak},
		{"proactive_judge_interval_sec", s.ProactiveJudgeIntervalSec},
		{"proactive_judge_timeout_sec", s.ProactiveJudgeTimeoutSec},
		{"proactive_judge_images", s.ProactiveJudgeImages},
	} {
		if item.value != nil && *item.value < 0 {
			return fmt.Errorf("bot.%s 不能为负（当前 %d）：0 是有效配置，不想自定义就删掉这一行用默认值",
				item.name, *item.value)
		}
	}
	return nil
}
