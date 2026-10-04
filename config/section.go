package config

import (
	"fmt"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

// Section 是配置里的一个"域"，概念上对应 K8s 的一个 API 组：
// 自己解析、自己补默认值、自己校验，三件事就近维护在同一个文件里。
//
// 改造前这些都散在一个 200 行的 LoadConfig 里——字段在 configFile、默认值在函数中段、
// 校验（几乎没有）在别处，加一个配置项要同时改三个地方。
//
// 执行顺序固定为 解码 → SetDefaults → Validate（读取由 loader 统一做），顺序不可换：
// Validate 只负责判定合法，不再兼职填默认值——这正是改造前最容易出错的地方
// （llm_send_count 既在那段里被补默认值，又被当成已经合法的值继续用）。
type Section interface {
	// Name 是它在 config.yaml 里的顶层键，也是注册表里的唯一标识
	Name() string
	// Unmarshal 从整份文档里取自己那一段解码。整段缺失不算错误，缺省值交给 SetDefaults
	Unmarshal(document *yaml.Node) error
	// SetDefaults 填默认值。**必须幂等**：阶段 3 的 informer 会反复调 Load
	SetDefaults()
	// Validate 只校验本域自身。校验错误是给"改 YAML 的人"看的，
	// 措辞要指明是哪一段、当前值是什么、应该写成什么样——
	// 不要出现 "Field validation for 'X' failed on the 'gt' tag" 这种只有机器看得懂的话
	Validate() error
}

// decodeSection 从整份 YAML 文档的顶层映射里取出名为 name 的那一段，解码进 target。
// 该段不存在时返回 nil（不是错误）：缺省值由各域的 SetDefaults 负责。
func decodeSection(document *yaml.Node, name string, target any) error {
	node := topLevelValue(document, name)
	if node == nil {
		return nil
	}
	return node.Decode(target)
}

// topLevelValue 在文档映射节点里找顶层键，找不到返回 nil。
// yaml.Node 的映射内容是扁平的 [key, value, key, value, ...]，所以按 2 步跨。
func topLevelValue(document *yaml.Node, name string) *yaml.Node {
	if document == nil || document.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(document.Content); i += 2 {
		if document.Content[i].Value == name {
			return document.Content[i+1]
		}
	}
	return nil
}

// parseCommaList 把逗号分隔的字符串切成去空白、去空项的切片（群号白名单、CORS 来源共用）。
func parseCommaList(raw string) []string {
	var items []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			items = append(items, item)
		}
	}
	return items
}

// validateEndpoint 校验一个必填的 http(s) 端点。
//
// 这里校验得比较实：没有 scheme 或没有 host 的地址，resty 在运行期是连不上的，
// 与其等到第一次拉消息时才发现，不如启动就把话说清楚。
func validateEndpoint(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s 不能为空，例如 http://127.0.0.1:3000", field)
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("%s 不是合法 URL（%v）：%q", field, err, value)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%s 必须以 http:// 或 https:// 开头，当前是 %q", field, value)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%s 缺少主机名，例如 http://127.0.0.1:3000，当前是 %q", field, value)
	}
	return nil
}

// positiveInt 生成"必须为正整数"的校验错误，措辞统一。
func positiveInt(field string, value int, hint string) error {
	return fmt.Errorf("%s 必须大于 0，当前是 %d（%s）", field, value, hint)
}
