package config

import (
	"strings"
	"testing"
)

func TestWorkspaceAccounts校验(t *testing.T) {
	cases := []struct {
		name     string
		accounts []string
		wantErr  bool
	}{
		{"空名单可运行", nil, false},
		{"合法 ID", []string{"qq_official_bot_1906xxxx", "telegram_app_foobar"}, false},
		{"通配符", []string{"*"}, false},
		{"空项", []string{"qq_official_bot_1", ""}, true},
		{"非法字符", []string{"../etc"}, true},
		{"重复", []string{"qq_official_bot_1", "qq_official_bot_1"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			section := &botSection{AllowWorkspaceAccounts: tc.accounts}
			err := section.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("期望校验失败，实际通过：%+v", tc.accounts)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("不应失败：%v", err)
			}
		})
	}
}

func TestWorkspaceAccounts错误信息可读(t *testing.T) {
	err := (&botSection{AllowWorkspaceAccounts: []string{"bad id"}}).Validate()
	if err == nil || !strings.Contains(err.Error(), "allow_workspace_accounts") {
		t.Fatalf("错误信息应点名配置项，实际：%v", err)
	}
}
