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

func TestHasSelfID(t *testing.T) {
	explicit := &Config{AllowSelfIDs: []string{"bot-a", "bot-b"}}
	if !explicit.HasSelfID("bot-a") || !explicit.HasSelfID("bot-b") {
		t.Fatal("explicit self ids should match")
	}
	if explicit.HasSelfID("bot-c") {
		t.Fatal("unknown self id must not match")
	}
	if !explicit.IsManagedAccount("bot-a") || explicit.IsManagedAccount("bot-c") {
		t.Fatal("IsManagedAccount should only match explicit accounts")
	}

	// 适配器还没学到真实身份时 self_id 是占位值，无法判断归属，放行。
	if !explicit.HasSelfID("") || !explicit.HasSelfID("qq_official") {
		t.Fatal("placeholder self id should pass")
	}

	wildcard := &Config{AllowSelfIDs: []string{"*"}}
	if !wildcard.HasSelfID("any-bot") {
		t.Fatal("wildcard should allow every account")
	}
	if wildcard.IsManagedAccount("any-bot") {
		t.Fatal("wildcard cannot enumerate accounts, IsManagedAccount must be false")
	}

	empty := &Config{}
	if empty.HasSelfID("bot-a") {
		t.Fatal("empty list must allow no account")
	}
}
