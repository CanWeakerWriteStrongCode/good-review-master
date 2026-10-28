package config

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"good-review-master/logutil"
)

// workspaceIDPattern 同时约束工作空间 ID 与 im.accounts[].id：它会拼进数据库列与面板 URL，
// 放开就等于把路径穿越和 URL 编码问题交给每个调用方自己处理。
var workspaceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// imAccountSection 一个机器人账号的出站地址覆盖（config.yaml 的 im.accounts 项）。
type imAccountSection struct {
	// ID 是工作空间 ID（platform_account），入站路径里的 account 由它反查账号。
	ID string `yaml:"id"`
	// APIBase 是该账号在实现端的 OneBot 12 HTTP API 根地址（请求发到 base + "/"）。
	APIBase string `yaml:"api_base"`
}

// imSection OneBot 12 接入配置（config.yaml 的 im 段）。
//
// 入站监听地址在 runtime.im_listen_addr；这里只放出站地址。
// 常见部署是「一个 AstrBot 插件服务所有账号」：只配 api_base 即够，
// accounts 只在多套 AstrBot/多端口、需要按账号分地址时才用。
type imSection struct {
	// APIBase 所有账号默认的出站地址（实现端 OneBot 12 HTTP API 根地址）。
	APIBase string `yaml:"api_base"`
	// Accounts 可选的按账号覆盖；api_base 留空表示沿用上面的默认地址。
	Accounts []imAccountSection `yaml:"accounts"`
}

func (s *imSection) Name() string { return "im" }

func (s *imSection) Unmarshal(document *yaml.Node) error {
	return decodeSection(document, s.Name(), s)
}

func (s *imSection) SetDefaults() {
	s.APIBase = strings.TrimRight(strings.TrimSpace(s.APIBase), "/")
	for i := range s.Accounts {
		s.Accounts[i].ID = strings.TrimSpace(s.Accounts[i].ID)
		s.Accounts[i].APIBase = strings.TrimRight(strings.TrimSpace(s.Accounts[i].APIBase), "/")
	}
}

// Validate 校验出站地址。没配地址不硬失败：程序仍能接收事件（写缓存），
// 只是发不出消息——这和「群白名单为空」属于同一类可运行但不可用的配置。
func (s *imSection) Validate() error {
	if s.APIBase != "" {
		if err := validateEndpoint("im.api_base", s.APIBase); err != nil {
			return err
		}
	}
	if len(s.Accounts) == 0 {
		if s.APIBase == "" {
			logutil.Warn("未配置 im.api_base：能接收事件，但无法发送任何消息")
		}
		return nil
	}
	seen := make(map[string]struct{}, len(s.Accounts))
	for i, account := range s.Accounts {
		if account.ID == "" {
			return fmt.Errorf("im.accounts[%d].id 不能为空（填工作空间 ID，如 qq_1906xxxx）", i)
		}
		if !workspaceIDPattern.MatchString(account.ID) {
			return fmt.Errorf("im.accounts[%d].id=%q 只能包含字母、数字、下划线和短横线（最多 64 字符）", i, account.ID)
		}
		if _, ok := seen[account.ID]; ok {
			return fmt.Errorf("im.accounts 中 id 重复：%s", account.ID)
		}
		seen[account.ID] = struct{}{}
		if account.APIBase == "" {
			if s.APIBase == "" {
				return fmt.Errorf("im.accounts[%s].api_base 为空，且未配置全局 im.api_base", account.ID)
			}
			continue
		}
		if err := validateEndpoint("im.accounts["+account.ID+"].api_base", account.APIBase); err != nil {
			return err
		}
	}
	return nil
}
