package app

import (
	"testing"

	"good-review-master/config"
)

// TestSameMCPServers 覆盖"是否要提示重启"的判定。
// 这里不能退化成 reflect.DeepEqual：它会把 nil 与空切片判为不等，
// 从而在同一份配置上反复误报"mcp.servers 变了，需要重启"。
func TestSameMCPServers(t *testing.T) {
	base := []config.MCPServerConf{
		{Name: "a", Transport: "http", URL: "https://a/mcp", Token: "t", Args: []string{"x", "y"}},
		{Name: "b", Transport: "stdio", Command: "uvx"},
	}

	cases := []struct {
		name  string
		left  []config.MCPServerConf
		right []config.MCPServerConf
		want  bool
	}{
		{"完全相同", base, base, true},
		{"nil 与空切片等价", nil, []config.MCPServerConf{}, true},
		{"多一个服务", base, append(append([]config.MCPServerConf{}, base...),
			config.MCPServerConf{Name: "c"}), false},
		{"改了 url", base, []config.MCPServerConf{
			{Name: "a", Transport: "http", URL: "https://CHANGED/mcp", Token: "t", Args: []string{"x", "y"}},
			{Name: "b", Transport: "stdio", Command: "uvx"},
		}, false},
		{"改了 token", base, []config.MCPServerConf{
			{Name: "a", Transport: "http", URL: "https://a/mcp", Token: "new", Args: []string{"x", "y"}},
			{Name: "b", Transport: "stdio", Command: "uvx"},
		}, false},
		{"改了 args", base, []config.MCPServerConf{
			{Name: "a", Transport: "http", URL: "https://a/mcp", Token: "t", Args: []string{"x", "z"}},
			{Name: "b", Transport: "stdio", Command: "uvx"},
		}, false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := sameMCPServers(testCase.left, testCase.right); got != testCase.want {
				t.Fatalf("sameMCPServers = %v，期望 %v", got, testCase.want)
			}
		})
	}
}

// TestSameMCPServers看注入开关 覆盖 *bool 的三态：
// Inject 是"区分没写和写了 false"的指针，比较必须走 ShouldInject，
// 否则 nil 与 &true 会被判为不同而误报重启。
func TestSameMCPServers看注入开关(t *testing.T) {
	noInject := false
	yesInject := true

	unset := []config.MCPServerConf{{Name: "a", Transport: "http", URL: "u"}}
	explicitTrue := []config.MCPServerConf{{Name: "a", Transport: "http", URL: "u", Inject: &yesInject}}
	explicitFalse := []config.MCPServerConf{{Name: "a", Transport: "http", URL: "u", Inject: &noInject}}

	if !sameMCPServers(unset, explicitTrue) {
		t.Error("没写 inject 与显式 true 语义相同（默认注入），不该判为变化")
	}
	if sameMCPServers(unset, explicitFalse) {
		t.Error("显式 false 会真正改变行为（连接但不注入），必须判为变化")
	}
}
