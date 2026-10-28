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
}

func (s *botSection) Name() string { return "bot" }

func (s *botSection) Unmarshal(document *yaml.Node) error {
	return decodeSection(document, s.Name(), s)
}

// SetDefaults 该域没有需要补齐的缺省值；保留空实现以满足 Section 接口。
func (s *botSection) SetDefaults() {}

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
