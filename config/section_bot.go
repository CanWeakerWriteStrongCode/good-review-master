package config

import (
	"strings"

	"gopkg.in/yaml.v3"

	"good-review-master/logutil"
)

// botSection 机器人身份与白名单（config.yaml 的 bot 段）
type botSection struct {
	QQ string `yaml:"qq"`
	// AllowGroups 是逗号分隔的字符串，不是 YAML 列表——保持既有形态不变，
	// 改形态会让所有存量配置失效。转换在 convert.go 里做。
	AllowGroups string `yaml:"allow_groups"`
}

func (s *botSection) Name() string { return "bot" }

func (s *botSection) Unmarshal(document *yaml.Node) error {
	return decodeSection(document, s.Name(), s)
}

func (s *botSection) SetDefaults() {
	s.QQ = strings.TrimSpace(s.QQ)
}

// Validate 只做"不可能工作"的判定。
//
// 刻意**不**把 qq 为空、allow_groups 为空做成硬错误：这两个值缺失时程序能跑，
// 只是不响应任何消息。把它们升级成启动失败会让"改配置时把白名单清空试一下"这种
// 正常操作变成起不来，代价大于收益——所以只告警。
func (s *botSection) Validate() error {
	if s.QQ == "" {
		logutil.Warn("bot.qq 未配置，@机器人 检测会退化为仅按昵称匹配（昵称也要从 NapCat 取，取不到就永远不响应）")
	}
	if len(parseCommaList(s.AllowGroups)) == 0 {
		logutil.Warn("bot.allow_groups 为空，机器人不会响应任何群消息")
	}
	return nil
}
