package config

import (
	"strings"

	"gopkg.in/yaml.v3"

	"good-review-master/logutil"
)

// napcatSectionName 是旧版配置里的顶层键，仅用于一次性迁移。
const napcatSectionName = "napcat"

// astrbotSection AstrBot 桥接插件动作代理配置（config.yaml 的 astrbot 段）。
// Go 通过它发消息、查群信息；入站事件走 runtime.im_listen_addr。
type astrbotSection struct {
	HTTPAPI string `yaml:"http_api"`
	// AccessToken 留在本域是为了兼容旧配置（密钥原本就写在 config.yaml 里）。
	// 新配置应写进 secret.yaml；两处都写时 secret.yaml 优先，见 convert.go。
	AccessToken string `yaml:"access_token"`
}

func (s *astrbotSection) Name() string { return "astrbot" }

func (s *astrbotSection) Unmarshal(document *yaml.Node) error {
	// 一次性迁移：旧配置的 napcat 段按 astrbot 读取，避免升级后静默丢掉地址和 token。
	if topLevelValue(document, s.Name()) == nil && topLevelValue(document, napcatSectionName) != nil {
		logutil.Warn("config.yaml 的 napcat 段已更名为 astrbot（NapCat 接入已移除），请尽快修改")
		return decodeSection(document, napcatSectionName, s)
	}
	return decodeSection(document, s.Name(), s)
}

func (s *astrbotSection) SetDefaults() {
	s.HTTPAPI = strings.TrimSpace(s.HTTPAPI)
	if s.HTTPAPI == "" {
		s.HTTPAPI = "http://127.0.0.1:6199"
	}
	s.AccessToken = strings.TrimSpace(s.AccessToken)
}

func (s *astrbotSection) Validate() error {
	return validateEndpoint("astrbot.http_api", s.HTTPAPI)
}
