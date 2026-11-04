package policy

import (
	"testing"

	"good-review-master/config"
)

func TestAllowsStream(t *testing.T) {
	explicit := &config.Config{AllowGroups: []string{"10001", "10002"}}
	if !AllowsStream(explicit, "10001") || !AllowsStream(explicit, "10002") {
		t.Fatal("explicit groups should match")
	}
	if AllowsStream(explicit, "10003") {
		t.Fatal("unknown group must not match")
	}
	if AllowsAllStreams(explicit) {
		t.Fatal("explicit list must not allow all groups")
	}

	wildcard := &config.Config{AllowGroups: []string{"*"}}
	if !AllowsStream(wildcard, "any-openid") || !AllowsAllStreams(wildcard) {
		t.Fatal("wildcard should allow every group")
	}

	empty := &config.Config{}
	if AllowsStream(empty, "10001") || AllowsAllStreams(empty) {
		t.Fatal("empty list must allow nothing")
	}

	// 快照理论上不会是 nil，但测试与嵌入式调用会直接传字面量，判空要稳。
	if AllowsStream(nil, "10001") || AllowsAllStreams(nil) {
		t.Fatal("nil config must allow nothing")
	}
}

func TestAllowsAccount(t *testing.T) {
	explicit := &config.Config{WorkspaceAccounts: []string{"qq_official_bot_1906xxxx", "qq_official_bot_1904xxxx"}}
	if !AllowsAccount(explicit, "qq_official_bot_1906xxxx") || !AllowsAccount(explicit, "qq_official_bot_1904xxxx") {
		t.Fatal("explicit workspace accounts should match")
	}
	if AllowsAccount(explicit, "qq_9999") {
		t.Fatal("unknown workspace account must not match")
	}
	if AllowsAllAccounts(explicit) {
		t.Fatal("explicit list must not allow all accounts")
	}

	wildcard := &config.Config{WorkspaceAccounts: []string{"*"}}
	if !AllowsAccount(wildcard, "any-platform_any-account") || !AllowsAllAccounts(wildcard) {
		t.Fatal("wildcard should allow every workspace")
	}

	empty := &config.Config{}
	if AllowsAccount(empty, "qq_official_bot_1906xxxx") || AllowsAllAccounts(empty) {
		t.Fatal("empty list must allow nothing")
	}

	if AllowsAccount(nil, "qq_official_bot_1906xxxx") || AllowsAllAccounts(nil) {
		t.Fatal("nil config must allow nothing")
	}
}

func TestIsRecordOnly(t *testing.T) {
	cfg := &config.Config{RecordOnlyAccounts: []string{"qq_official_bot_1904xxxx"}}
	if !IsRecordOnly(cfg, "qq_official_bot_1904xxxx") {
		t.Fatal("名单内的账号应判为只记录")
	}
	if IsRecordOnly(cfg, "qq_official_bot_1906xxxx") {
		t.Fatal("名单外的账号不应判为只记录")
	}
	if IsRecordOnly(&config.Config{}, "qq_official_bot_1904xxxx") {
		t.Fatal("空名单不应命中任何账号")
	}
	if IsRecordOnly(nil, "qq_official_bot_1904xxxx") {
		t.Fatal("nil 配置不应命中任何账号")
	}
	// 通配符不参与只记录判定：配置校验就拒绝，这里确认不会被误当成"全部"
	wildcard := &config.Config{RecordOnlyAccounts: []string{"*"}}
	if IsRecordOnly(wildcard, "qq_official_bot_1904xxxx") {
		t.Fatal("\"*\" 不应让所有账号都变成只记录")
	}
}

func TestCanUseCommand(t *testing.T) {
	cfg := &config.Config{CommandAdmins: map[string][]string{
		"group-a": {"openid-admin", "openid-second"},
		"group-b": {"openid-admin"},
	}}

	if !CanUseCommand(cfg, "group-a", "openid-admin") {
		t.Fatal("名单内的管理员应放行")
	}
	if !CanUseCommand(cfg, "group-b", "openid-admin") {
		t.Fatal("同一个管理员在别的群也配了时应放行")
	}
	if CanUseCommand(cfg, "group-a", "openid-member") {
		t.Fatal("名单外的成员必须拒绝")
	}
	if CanUseCommand(cfg, "group-c", "openid-admin") {
		t.Fatal("没配置的群必须拒绝：管理员身份不跨群继承")
	}

	// 没配置任何群：所有群都拒绝（fail-closed）。
	if CanUseCommand(&config.Config{}, "group-a", "openid-admin") {
		t.Fatal("空名单必须拒绝所有群")
	}
	if CanUseCommand(nil, "group-a", "openid-admin") {
		t.Fatal("nil 配置必须拒绝")
	}

	// 群名字里写 "*" 不是通配符：它只是一个叫 "*" 的群。
	wildcardGroup := &config.Config{CommandAdmins: map[string][]string{"*": {"openid-admin"}}}
	if CanUseCommand(wildcardGroup, "group-a", "openid-admin") {
		t.Fatal("群名单不支持通配符：别的群不能借用")
	}

	// 群内管理员列表里写 "*" = 该群所有人都能用（只放开成员这一层）。
	openGroup := &config.Config{CommandAdmins: map[string][]string{
		"group-a": {"*"},
		"group-b": {"openid-admin"},
	}}
	if !CanUseCommand(openGroup, "group-a", "openid-anyone") {
		t.Fatal("列表里有 \"*\" 时该群任意成员都应放行")
	}
	if !CanUseCommand(openGroup, "group-a", "openid-admin") {
		t.Fatal("\"*\" 包含管理员本人")
	}
	if CanUseCommand(openGroup, "group-b", "openid-anyone") {
		t.Fatal("\"*\" 只作用于写了它的那个群，不跨群扩散")
	}
	if CanUseCommand(openGroup, "group-c", "openid-anyone") {
		t.Fatal("没配置的群照旧拒绝：fail-closed 不变")
	}

	// 私聊直接放行：对话人只有账号主人自己。
	if !CanUseCommand(&config.Config{}, "private:openid-user", "openid-user") {
		t.Fatal("私聊应放行，且不依赖配置")
	}
}
