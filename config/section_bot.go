package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"good-review-master/logutil"
)

// botSection 机器人白名单（config.yaml 的 bot 段）
type botSection struct {
	// AllowGroups 是逗号分隔的字符串，不是 YAML 列表——保持既有形态不变，
	// 改形态会让所有存量配置失效。转换在 convert.go 里做。
	// 填 "*" 表示允许所有群；空串表示不响应任何群。
	AllowGroups string `yaml:"allow_groups"`

	// WorkspaceAccounts 允许**自动开通**的工作空间 ID（platform_account），
	// 填 "*" 表示全部；空列表表示一个都不自动开通（安全默认）。
	//
	// 只控制新建：已经有目录的工作空间不受名单限制，删名字不会让它停止工作。
	// 必须匹配完整 ID 而不是单独的 account：不同平台可能出现相同的 account。
	WorkspaceAccounts []string `yaml:"workspace_accounts"`

	// ---- 主动插话（非 @ 时由大模型裁决要不要接话） ----
	// 全局一份，所有工作空间共用；关闭时行为与改造前完全一致。

	// ProactiveEnabled 主动插话总开关。默认 false。
	ProactiveEnabled bool `yaml:"proactive_enabled"`
	// ProactiveWindowMsgs 裁判能看到最近多少条消息。默认 15。
	ProactiveWindowMsgs int `yaml:"proactive_window_msgs"`
	// ProactiveCooldownSec 机器人主动发言后，该群多久内不再判定。默认 90。
	ProactiveCooldownSec int `yaml:"proactive_cooldown_sec"`
	// ProactiveMaxStreak 同一话题最多连续主动发言次数。默认 5。
	ProactiveMaxStreak int `yaml:"proactive_max_streak"`
	// ProactiveJudgeIntervalSec 两次判定的最小间隔（防止每条消息都调一次裁判）。默认 20。
	ProactiveJudgeIntervalSec int `yaml:"proactive_judge_interval_sec"`
	// ProactiveJudgeTimeoutSec 单次判定调用超时，超时按"不回复"处理。默认 20。
	ProactiveJudgeTimeoutSec int `yaml:"proactive_judge_timeout_sec"`
	// ProactiveJudgeImages 每次判定最多附几张图（纯图片消息）。默认 1；0 = 图片消息不判定。
	ProactiveJudgeImages *int `yaml:"proactive_judge_images"`
}

func (s *botSection) Name() string { return "bot" }

func (s *botSection) Unmarshal(document *yaml.Node) error {
	return decodeSection(document, s.Name(), s)
}

// SetDefaults 补齐主动插话的默认值。
//
// 必须幂等：配置热更新的 informer 会反复调 Load。
// 这里用 <=0 判定"没写"：判定间隔/冷却/上限写成 0 在业务上都没有意义，
// 只有 proactive_judge_images 用指针区分"没写"与"显式写 0（不判图）"。
func (s *botSection) SetDefaults() {
	if s.ProactiveWindowMsgs <= 0 {
		s.ProactiveWindowMsgs = 15
	}
	if s.ProactiveCooldownSec <= 0 {
		s.ProactiveCooldownSec = 90
	}
	if s.ProactiveMaxStreak <= 0 {
		s.ProactiveMaxStreak = 5
	}
	if s.ProactiveJudgeIntervalSec <= 0 {
		s.ProactiveJudgeIntervalSec = 20
	}
	if s.ProactiveJudgeTimeoutSec <= 0 {
		s.ProactiveJudgeTimeoutSec = 20
	}
	if s.ProactiveJudgeImages == nil {
		defaultImages := 1
		s.ProactiveJudgeImages = &defaultImages
	}
	if *s.ProactiveJudgeImages < 0 {
		zero := 0
		s.ProactiveJudgeImages = &zero
	}
}

// Validate 只做"不可能工作"的判定。
//
// 刻意**不**把 allow_groups 为空做成硬错误：缺失时程序能跑，只是不响应任何消息。
// 把它升级成启动失败会让"改配置时把白名单清空试一下"这种正常操作变成起不来，
// 代价大于收益——所以只告警。
func (s *botSection) Validate() error {
	groups := parseCommaList(s.AllowGroups)
	if len(groups) == 0 {
		logutil.Warn("bot.allow_groups 为空，机器人不会响应任何群消息")
	}
	for _, group := range groups {
		if group == "*" {
			logutil.Warn("bot.allow_groups 包含 *：所有群都会响应，注意大模型调用成本")
			break
		}
	}

	if len(s.WorkspaceAccounts) == 0 {
		logutil.Warn("bot.workspace_accounts 为空：不会自动开通新工作空间（档案里已有的仍会恢复）")
	}
	seen := make(map[string]struct{}, len(s.WorkspaceAccounts))
	for i, account := range s.WorkspaceAccounts {
		account = strings.TrimSpace(account)
		s.WorkspaceAccounts[i] = account
		if account == "" {
			return fmt.Errorf("bot.workspace_accounts[%d] 不能为空", i)
		}
		if account != "*" && !workspaceIDPattern.MatchString(account) {
			return fmt.Errorf("bot.workspace_accounts[%d]=%q 不是合法的工作空间 ID（字母/数字/_/-，最多 64 字符，例如 qq_1906xxxx）", i, account)
		}
		if _, ok := seen[account]; ok {
			return fmt.Errorf("bot.workspace_accounts 中重复：%s", account)
		}
		seen[account] = struct{}{}
	}
	return nil
}
