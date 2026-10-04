package app

import (
	"testing"

	"good-review-master/config"
)

// TestDerive贴上昵称与内嵌看图服务 锁住快照的完整性。
//
// 这条不是可有可无的：昵称与内嵌看图地址都不在任何配置文件里，
// 重读磁盘拿到的配置**必然**不含它们。若重载路径忘了走 derive，
// 快照就会悄悄丢掉这两项——而且不会报错，只表现为"某次改配置之后
// @机器人 不响应了"/"view_image 工具不见了"。
func TestDerive贴上昵称与内嵌看图服务(t *testing.T) {
	application := &App{
		botNickname:      "好评大师",
		builtinImageAddr: "http://127.0.0.1:12345/mcp",
	}

	loaded := &config.Config{
		BotQQ:           "123456",
		McpBuiltinToken: "tok",
		MCPConfig: config.MCPConf{
			Enabled: false,
			Servers: []config.MCPServerConf{{Name: "ddg-search", Transport: "stdio", Command: "uvx"}},
		},
	}

	got := application.derive(loaded)

	if got.BotNickname != "好评大师" {
		t.Errorf("昵称应被贴上，实际 %q", got.BotNickname)
	}
	if !got.MCPConfig.Enabled {
		t.Error("注入了内嵌看图服务就应自动开启 mcp.enabled")
	}
	if len(got.MCPConfig.Servers) != 2 {
		t.Fatalf("应保留原有服务并追加 builtin_image，实际 %d 个：%v", len(got.MCPConfig.Servers), got.MCPConfig.Servers)
	}
	if got.MCPConfig.Servers[0].Name != "ddg-search" {
		t.Errorf("原有服务不该被动，实际第一个是 %q", got.MCPConfig.Servers[0].Name)
	}
	builtin := got.MCPConfig.Servers[1]
	if builtin.Name != "builtin_image" || builtin.URL != "http://127.0.0.1:12345/mcp" || builtin.Token != "tok" {
		t.Errorf("builtin_image 条目不对：%+v", builtin)
	}
}

// TestDerive不共享底层数组 防一个很隐蔽的别名问题：
// 若 derive 直接在原切片上 append，新快照与"已发布出去的旧快照"会共享底层数组，
// 旧快照就不再不可变——而不可变正是整个快照机制赖以成立的前提。
func TestDerive不共享底层数组(t *testing.T) {
	application := &App{builtinImageAddr: "http://127.0.0.1:9999/mcp"}

	// 容量刻意留足，让 append 有机会复用底层数组
	existing := make([]config.MCPServerConf, 1, 8)
	existing[0] = config.MCPServerConf{Name: "ddg-search"}
	loaded := &config.Config{MCPConfig: config.MCPConf{Enabled: true, Servers: existing}}

	got := application.derive(loaded)

	if len(existing) != 1 {
		t.Fatalf("原切片被 append 改动了（len 变成 %d），旧快照已被污染", len(existing))
	}
	if cap(got.MCPConfig.Servers) == cap(existing) {
		t.Error("新切片与原切片共享底层数组，derive 必须复制后再追加")
	}
}

// TestDerive未启用看图时不动MCP配置 保证 image_max<=0 时行为与改造前一致。
func TestDerive未启用看图时不动MCP配置(t *testing.T) {
	application := &App{botNickname: "n"}

	loaded := &config.Config{
		MCPConfig: config.MCPConf{
			Enabled: false,
			Servers: []config.MCPServerConf{{Name: "only", Transport: "http", URL: "https://x/mcp"}},
		},
	}
	got := application.derive(loaded)

	if got.MCPConfig.Enabled {
		t.Error("没启用看图就不该自动开启 mcp.enabled")
	}
	if len(got.MCPConfig.Servers) != 1 || got.MCPConfig.Servers[0].Name != "only" {
		t.Errorf("服务列表不该被动：%v", got.MCPConfig.Servers)
	}
}
