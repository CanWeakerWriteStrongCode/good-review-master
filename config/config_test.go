package config

import "testing"

func TestHasGroup(t *testing.T) {
	explicit := &Config{AllowGroups: []string{"10001", "10002"}}
	if !explicit.HasGroup("10001") || !explicit.HasGroup("10002") {
		t.Fatal("explicit groups should match")
	}
	if explicit.HasGroup("10003") {
		t.Fatal("unknown group must not match")
	}
	if explicit.AllowsAllGroups() {
		t.Fatal("explicit list must not allow all groups")
	}

	wildcard := &Config{AllowGroups: []string{"*"}}
	if !wildcard.HasGroup("any-openid") || !wildcard.AllowsAllGroups() {
		t.Fatal("wildcard should allow every group")
	}

	empty := &Config{}
	if empty.HasGroup("10001") || empty.AllowsAllGroups() {
		t.Fatal("empty list must allow nothing")
	}
}

func TestHasWorkspaceAccount(t *testing.T) {
	explicit := &Config{WorkspaceAccounts: []string{"qq_official_bot_1906xxxx", "qq_official_bot_1904xxxx"}}
	if !explicit.HasWorkspaceAccount("qq_official_bot_1906xxxx") || !explicit.HasWorkspaceAccount("qq_official_bot_1904xxxx") {
		t.Fatal("explicit workspace accounts should match")
	}
	if explicit.HasWorkspaceAccount("qq_9999") {
		t.Fatal("unknown workspace account must not match")
	}
	if explicit.AllowsAllWorkspaceAccounts() {
		t.Fatal("explicit list must not allow all accounts")
	}

	wildcard := &Config{WorkspaceAccounts: []string{"*"}}
	if !wildcard.HasWorkspaceAccount("any-platform_any-account") || !wildcard.AllowsAllWorkspaceAccounts() {
		t.Fatal("wildcard should allow every workspace")
	}

	empty := &Config{}
	if empty.HasWorkspaceAccount("qq_official_bot_1906xxxx") || empty.AllowsAllWorkspaceAccounts() {
		t.Fatal("empty list must allow nothing")
	}
}

func TestIsRecordOnly(t *testing.T) {
	cfg := &Config{RecordOnlyAccounts: []string{"qq_official_bot_1904xxxx"}}
	if !cfg.IsRecordOnly("qq_official_bot_1904xxxx") {
		t.Fatal("名单内的账号应判为只记录")
	}
	if cfg.IsRecordOnly("qq_official_bot_1906xxxx") {
		t.Fatal("名单外的账号不应判为只记录")
	}
	if (&Config{}).IsRecordOnly("qq_official_bot_1904xxxx") {
		t.Fatal("空名单不应命中任何账号")
	}
	// 通配符不参与只记录判定：配置校验阶段就拒绝，这里确认不会被误当成"全部"
	wildcard := &Config{RecordOnlyAccounts: []string{"*"}}
	if wildcard.IsRecordOnly("qq_official_bot_1904xxxx") {
		t.Fatal("\"*\" 不应让所有账号都变成只记录")
	}
}
