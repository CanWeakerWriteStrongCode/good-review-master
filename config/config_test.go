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
	explicit := &Config{WorkspaceAccounts: []string{"qq_1906xxxx", "qq_1904xxxx"}}
	if !explicit.HasWorkspaceAccount("qq_1906xxxx") || !explicit.HasWorkspaceAccount("qq_1904xxxx") {
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
	if empty.HasWorkspaceAccount("qq_1906xxxx") || empty.AllowsAllWorkspaceAccounts() {
		t.Fatal("empty list must allow nothing")
	}
}
