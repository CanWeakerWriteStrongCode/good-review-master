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
