package config

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// napcatSection NapCat 连接配置（config.yaml 的 napcat 段）
type napcatSection struct {
	HTTPAPI string `yaml:"http_api"`
	// AccessToken 留在本域是为了兼容旧配置（密钥原本就写在 config.yaml 里）。
	// 新配置应写进 secret.yaml；两处都写时 secret.yaml 优先，见 convert.go。
	AccessToken string `yaml:"access_token"`
}

func (s *napcatSection) Name() string { return "napcat" }

func (s *napcatSection) Unmarshal(document *yaml.Node) error {
	return decodeSection(document, s.Name(), s)
}

func (s *napcatSection) SetDefaults() {
	s.HTTPAPI = strings.TrimSpace(s.HTTPAPI)
	if s.HTTPAPI == "" {
		s.HTTPAPI = "http://127.0.0.1:3000"
	}
	s.AccessToken = strings.TrimSpace(s.AccessToken)
}

func (s *napcatSection) Validate() error {
	return validateEndpoint("napcat.http_api", s.HTTPAPI)
}
