package config

import (
	"gopkg.in/yaml.v3"

	"good-review-master/logutil"
)

// botSection 机器人白名单（config.yaml 的 bot 段）
type botSection struct {
	// AllowGroups 是逗号分隔的字符串，不是 YAML 列表——保持既有形态不变，
	// 改形态会让所有存量配置失效。转换在 convert.go 里做。
	// 填 "*" 表示允许所有群；空串表示不响应任何群。
	AllowGroups string `yaml:"allow_groups"`
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
	return nil
}
