package config

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"good-review-master/logutil"
)

// workspaceIDRe 限定工作空间 ID 的字符集：它会拼进目录名与面板 URL，
// 放开就等于把路径穿越和 URL 编码问题交给每个调用方自己处理。
var workspaceIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// workspaceSection 一个机器人账号的工作空间声明（config.yaml 的 bot.workspaces 项）。
type workspaceSection struct {
	ID          string   `yaml:"id"`
	Name        string   `yaml:"name"`
	PlatformID  string   `yaml:"platform_id"`
	SelfIDs     []string `yaml:"self_ids"`
	Nickname    string   `yaml:"nickname"`
	AllowGroups string   `yaml:"allow_groups"`
	PromptDir   string   `yaml:"prompt_dir"`
}

// botSection 机器人白名单（config.yaml 的 bot 段）
type botSection struct {
	// AllowGroups 是逗号分隔的字符串，不是 YAML 列表——保持既有形态不变，
	// 改形态会让所有存量配置失效。转换在 convert.go 里做。
	// 填 "*" 表示允许所有群；空串表示不响应任何群。
	AllowGroups string `yaml:"allow_groups"`

	// AllowSelfIDs 是机器人账号（OneBot self_id）白名单，逗号分隔，填 "*" 表示全部。
	// 空串表示 Go 不接管任何账号：桥接插件收到 200 ignored 后会把消息交还给
	// AstrBot 自己的 Agent，因此多账号部署可以只让 Go 处理其中几个。
	AllowSelfIDs string `yaml:"allow_self_ids"`

	// Workspaces 是多账号工作空间声明。不写时程序退化为一个隐式 default
	// 工作空间，行为与改造前完全一致。
	Workspaces []workspaceSection `yaml:"workspaces"`
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

	selfIDs := parseCommaList(s.AllowSelfIDs)
	if len(selfIDs) == 0 {
		logutil.Warn("bot.allow_self_ids 为空，Go 不接管任何机器人账号（消息会留给 AstrBot 自己的 Agent）")
	}
	for _, id := range selfIDs {
		if id == "*" {
			logutil.Warn("bot.allow_self_ids 包含 *：所有机器人账号都由 Go 接管")
			break
		}
	}

	return s.validateWorkspaces()
}

// validateWorkspaces 校验工作空间声明。这里判的都是"继续跑就会串台"的配置错误，
// 所以直接启动失败，而不是告警后猜一个账号。
func (s *botSection) validateWorkspaces() error {
	if len(s.Workspaces) == 0 {
		return nil
	}
	ids := make(map[string]struct{}, len(s.Workspaces))
	platforms := make(map[string]struct{}, len(s.Workspaces))
	selfIDs := make(map[string]string, len(s.Workspaces))
	for i, ws := range s.Workspaces {
		ws.ID = strings.TrimSpace(ws.ID)
		ws.PlatformID = strings.TrimSpace(ws.PlatformID)
		if ws.ID == "" {
			return fmt.Errorf("bot.workspaces[%d].id 不能为空", i)
		}
		if !workspaceIDRe.MatchString(ws.ID) {
			return fmt.Errorf("bot.workspaces[%d].id=%q 只能包含字母、数字、下划线和短横线", i, ws.ID)
		}
		if _, ok := ids[ws.ID]; ok {
			return fmt.Errorf("bot.workspaces 中 id 重复：%s", ws.ID)
		}
		ids[ws.ID] = struct{}{}

		if ws.PlatformID == "" {
			return fmt.Errorf("bot.workspaces[%s].platform_id 不能为空（填 AstrBot 平台实例 ID）", ws.ID)
		}
		if _, ok := platforms[ws.PlatformID]; ok {
			return fmt.Errorf("bot.workspaces 中 platform_id 重复：%s（每个工作空间必须绑定不同的平台实例）", ws.PlatformID)
		}
		platforms[ws.PlatformID] = struct{}{}

		for _, id := range ws.SelfIDs {
			id = strings.TrimSpace(id)
			if id == "" || id == "*" {
				continue
			}
			if owner, ok := selfIDs[id]; ok {
				return fmt.Errorf("self_id %s 同时属于工作空间 %s 与 %s", id, owner, ws.ID)
			}
			selfIDs[id] = ws.ID
		}
	}
	return nil
}
