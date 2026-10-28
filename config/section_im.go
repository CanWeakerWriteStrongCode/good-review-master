package config

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"good-review-master/logutil"
)

// napcatSectionName 是旧版配置里的顶层键，仅用于一次性迁移告警。
const napcatSectionName = "napcat"

// workspaceIDPattern 同时约束工作空间 ID 与 im.accounts[].id：它会拼进数据库列与面板 URL，
// 放开就等于把路径穿越和 URL 编码问题交给每个调用方自己处理。
var workspaceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// imAccountSection 一个机器人账号的出站连接（config.yaml 的 im.accounts 项）。
type imAccountSection struct {
	// ID 是工作空间 ID（platform_account），入站路径里的 account 由它反查账号。
	ID string `yaml:"id"`
	// APIBase 是该账号在实现端的 OneBot 12 HTTP API 根地址（请求发到 base + "/"）。
	APIBase string `yaml:"api_base"`
}

// imSection OneBot 12 接入配置（config.yaml 的 im 段）。
//
// 入站监听地址在 runtime.im_listen_addr；这里只放共享 token 与每账号的出站地址。
// 一个账号一条连接：QQ 官方的 self.user_id 是群作用域 openid，无法稳定选账号，
// 所以出站必须按「工作空间 → 地址」选，而不是共用一个根地址再靠 self 路由。
type imSection struct {
	Token    string             `yaml:"token"`
	Accounts []imAccountSection `yaml:"accounts"`

	// 旧 astrbot 段的一次性兼容字段，只在 im 段整体缺失时被解码。
	LegacyHTTPAPI     string `yaml:"http_api"`
	LegacyAccessToken string `yaml:"access_token"`
}

func (s *imSection) Name() string { return "im" }

func (s *imSection) Unmarshal(document *yaml.Node) error {
	if topLevelValue(document, s.Name()) != nil {
		return decodeSection(document, s.Name(), s)
	}
	legacy := "astrbot"
	if topLevelValue(document, legacy) == nil {
		if topLevelValue(document, napcatSectionName) == nil {
			return nil
		}
		legacy = napcatSectionName
	}
	// 旧段只有一份地址和一个 token，没有账号维度；账号 ID 必须由人来填（它等于工作空间 ID），
	// 所以这里只把 token 接住，地址留到 Validate 里提示怎么迁移。
	logutil.Warn("config.yaml 的 " + legacy + " 段已改为 im 段，请迁移为 im.token 与 im.accounts[{id, api_base}]")
	return decodeSection(document, legacy, s)
}

func (s *imSection) SetDefaults() {
	s.Token = strings.TrimSpace(s.Token)
	if s.Token == "" && s.LegacyAccessToken != "" {
		s.Token = strings.TrimSpace(s.LegacyAccessToken)
		logutil.Warn("im.token 为空，暂用旧的 astrbot.access_token；建议迁移到 secret.yaml 的 im_token")
	}
	for i := range s.Accounts {
		s.Accounts[i].ID = strings.TrimSpace(s.Accounts[i].ID)
		s.Accounts[i].APIBase = strings.TrimRight(strings.TrimSpace(s.Accounts[i].APIBase), "/")
	}
}

// Validate 校验每个出站账号。缺账号不硬失败：程序仍能接收事件（写缓存），
// 只是发不出消息——这和「群白名单为空」属于同一类可运行但不可用的配置。
func (s *imSection) Validate() error {
	if len(s.Accounts) == 0 {
		logutil.Warn("im.accounts 为空：能接收事件，但无法发送任何消息")
		if s.LegacyHTTPAPI != "" {
			logutil.Warn("检测到旧的 astrbot.http_api，但缺少账号 ID，无法自动迁移；" +
				"请写成 im.accounts: [{id: \"qq_<AppID>\", api_base: \"http://127.0.0.1:6199\"}]")
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
		if err := validateEndpoint("im.accounts["+account.ID+"].api_base", account.APIBase); err != nil {
			return err
		}
	}
	return nil
}
