package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"gopkg.in/yaml.v3"
)

// Sources 配置来源路径。
type Sources struct {
	Config string // config.yaml（必填存在）
	Secret string // secret.yaml（可不存在，见 readOptionalDocument）
}

// Load 按四步曲加载配置：读取 → 解码 → SetDefaults → Validate，
// 最后把域类型转成业务侧读的 Config。
//
// **要求：无副作用、幂等。**
// 这条不是洁癖，是被阶段 3 的 informer 倒逼的：informer 会在文件变化时反复调 Load，
// 还有 resync 兜底。所以这里绝不能有"顺手建个文件""顺手写回默认值"这类动作——
// InitDefaultFiles（建模板）只能留在首跑路径上（cmd/good-review → app）。
func Load(sources Sources) (*Config, error) {
	// 1 读取
	configDocument, err := readDocument(sources.Config, true)
	if err != nil {
		return nil, err
	}
	secretDocument, err := readDocument(sources.Secret, false)
	if err != nil {
		return nil, err
	}

	sections := newSections() // 每次新建实例，见 scheme.go 的说明

	// 2 解码：config.yaml 的各域
	for _, section := range sections.registry() {
		if err := section.Unmarshal(configDocument); err != nil {
			return nil, fmt.Errorf("解析 config.yaml 的 %s 段失败：%w", section.Name(), err)
		}
	}
	// Secret 域来自另一份文档 + 环境变量
	if err := sections.Secret.Unmarshal(secretDocument); err != nil {
		return nil, fmt.Errorf("解析 %s 失败：%w", sources.Secret, err)
	}
	sections.Secret.applyEnv()

	// 3 默认值（顺序不能与第 4 步互换：缺省值会被 Validate 误判成非法值）
	for _, section := range sections.registry() {
		section.SetDefaults()
	}
	sections.Secret.SetDefaults()

	// 4 校验（域内）
	for _, section := range sections.registry() {
		if err := section.Validate(); err != nil {
			return nil, fmt.Errorf("config.yaml 的 %s 段不合法：%w", section.Name(), err)
		}
	}
	if err := sections.Secret.Validate(); err != nil {
		return nil, err
	}

	// external → internal，并在这一步做密钥来源的优先级与兼容处理
	cfg := assemble(sections)

	// 跨域的最终校验（能看到拼装完成的整体，比如 web_port 与密码的联动）
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// readDocument 读一份 YAML 成文档节点。
// required=true 时文件不存在直接报错；false 时返回 (nil, nil) 表示"没有这份文件"。
func readDocument(path string, required bool) (*yaml.Node, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !required {
			return nil, nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s 不存在，请先运行一次程序生成默认配置：%w", path, err)
		}
		return nil, fmt.Errorf("读取 %s 失败：%w", path, err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("%s 格式错误：%w", path, err)
	}
	// yaml.Unmarshal 解到 yaml.Node 时，拿到的是 **DocumentNode**，
	// 真正的内容（映射/序列）在它的 Content[0] 里。
	// 在这里统一下降到内容节点，后面的域就不必各自处理这一层——
	// 忘了降的后果是"所有域都取不到自己的段、全部退化成默认值"，
	// 一个不报错、不崩溃、只是配置静默失效的故障（本项目真实踩过）。
	if document.Kind == yaml.DocumentNode {
		if len(document.Content) == 0 {
			return nil, nil // 空文档
		}
		return document.Content[0], nil
	}
	return &document, nil
}
