package config

import (
	"testing"
)

func TestEffectiveWorkspaces无配置时生成default(t *testing.T) {
	cfg := &Config{AllowGroups: []string{"g1"}, AllowSelfIDs: []string{"bot-a", "*"}, BotNickname: "昵称"}
	list := cfg.EffectiveWorkspaces()
	if len(list) != 1 || list[0].ID != "default" {
		t.Fatalf("EffectiveWorkspaces() = %+v", list)
	}
	if list[0].Nickname != "昵称" || len(list[0].AllowGroups) != 1 || list[0].AllowGroups[0] != "g1" {
		t.Fatalf("default 应继承全局配置：%+v", list[0])
	}
	if len(list[0].SelfIDs) != 1 || list[0].SelfIDs[0] != "bot-a" {
		t.Fatalf("default 的 self_ids 应去掉通配符：%+v", list[0].SelfIDs)
	}
}

func TestHasSelfID空白名单但有工作空间时接管(t *testing.T) {
	cfg := &Config{Workspaces: []WorkspaceConf{{ID: "a", PlatformID: "平台A"}}}
	if !cfg.HasSelfID("any") {
		t.Fatal("配了工作空间后，接管范围由工作空间声明决定，不应被空 allow_self_ids 拒绝")
	}
}

func TestValidateWorkspaces拒绝冲突(t *testing.T) {
	cases := []struct {
		name string
		ws   []workspaceSection
	}{
		{"空 id", []workspaceSection{{PlatformID: "p"}, {ID: "b", PlatformID: "q"}}},
		{"非法 id", []workspaceSection{{ID: "../a", PlatformID: "p"}}},
		{"id 重复", []workspaceSection{{ID: "a", PlatformID: "p"}, {ID: "a", PlatformID: "q"}}},
		{"platform_id 重复", []workspaceSection{{ID: "a", PlatformID: "p"}, {ID: "b", PlatformID: "p"}}},
		{"self_id 重复", []workspaceSection{
			{ID: "a", PlatformID: "p", SelfIDs: []string{"x"}},
			{ID: "b", PlatformID: "q", SelfIDs: []string{"x"}},
		}},
		{"缺 platform_id", []workspaceSection{{ID: "a"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			section := &botSection{Workspaces: tc.ws}
			if err := section.validateWorkspaces(); err == nil {
				t.Fatalf("期望校验失败，实际通过：%+v", tc.ws)
			}
		})
	}
}

func TestValidateWorkspaces接受合法配置(t *testing.T) {
	section := &botSection{Workspaces: []workspaceSection{
		{ID: "a", PlatformID: "平台A", SelfIDs: []string{"a1"}},
		{ID: "b", PlatformID: "平台B", SelfIDs: []string{"b1"}},
	}}
	if err := section.validateWorkspaces(); err != nil {
		t.Fatalf("合法配置不应失败：%v", err)
	}
	if _, ok := (&Config{Workspaces: []WorkspaceConf{{ID: "a", PlatformID: "平台A"}}}).WorkspaceByID("a"); !ok {
		t.Fatal("WorkspaceByID 应能找到已声明的工作空间")
	}
}
