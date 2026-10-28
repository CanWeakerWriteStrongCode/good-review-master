package server

import (
	"testing"

	"good-review-master/cache"
	"good-review-master/config"
)

func TestCollectGroupDetails按账号归属(t *testing.T) {
	cache.ResetAll()
	defer cache.ResetAll()

	gc1 := cache.GetGroupCache("g1", 4)
	gc1.Add(cache.Message{MessageID: 1, SelfID: "bot-a", UserID: "u", Content: "a", Time: 100})
	gc2 := cache.GetGroupCache("g2", 4)
	gc2.Add(cache.Message{MessageID: 2, SelfID: "bot-b", UserID: "u", Content: "b", Time: 200})

	cfg := &config.Config{
		AllowGroups:  []string{"g1", "g2"},
		AllowSelfIDs: []string{"*"},
	}
	s := &Server{
		groupNames:   map[string]string{"g1": "群1", "g2": "群2"},
		accountNames: map[string]string{"bot-a": "账号A", "bot-b": "账号B"},
	}

	groups, accounts := s.collectGroupDetails(cfg)
	if len(groups) != 2 {
		t.Fatalf("groups = %+v", groups)
	}
	if groups[0].SelfID != "bot-a" || groups[0].AccountName != "账号A" {
		t.Fatalf("g1 应归属 bot-a：%+v", groups[0])
	}
	if groups[1].SelfID != "bot-b" || groups[1].AccountName != "账号B" {
		t.Fatalf("g2 应归属 bot-b：%+v", groups[1])
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts = %+v", accounts)
	}
	byID := map[string]AccountDetail{}
	for _, acc := range accounts {
		byID[acc.SelfID] = acc
	}
	if acc := byID["bot-a"]; acc.Nickname != "账号A" || acc.GroupCount != 1 || acc.MessageCount != 1 {
		t.Fatalf("bot-a 汇总 = %+v", acc)
	}
	if acc := byID["bot-b"]; acc.Nickname != "账号B" || acc.GroupCount != 1 || acc.MessageCount != 1 {
		t.Fatalf("bot-b 汇总 = %+v", acc)
	}
}

func TestCollectGroupDetails显式账号无群(t *testing.T) {
	cache.ResetAll()
	defer cache.ResetAll()

	cfg := &config.Config{AllowSelfIDs: []string{"bot-a"}}
	s := &Server{groupNames: map[string]string{}, accountNames: map[string]string{"bot-a": "账号A"}}

	groups, accounts := s.collectGroupDetails(cfg)
	if len(groups) != 0 {
		t.Fatalf("groups = %+v", groups)
	}
	if len(accounts) != 1 || accounts[0].SelfID != "bot-a" || accounts[0].GroupCount != 0 {
		t.Fatalf("accounts = %+v", accounts)
	}
}

func TestAccountIDs合并配置与缓存(t *testing.T) {
	cache.ResetAll()
	defer cache.ResetAll()

	gc := cache.GetGroupCache("g1", 4)
	gc.Add(cache.Message{MessageID: 1, SelfID: "bot-b", UserID: "u", Content: "a", Time: 1})

	cfg := &config.Config{
		AllowGroups:  []string{"g1"},
		AllowSelfIDs: []string{"bot-a"},
	}
	ids := accountIDs(cfg)
	if len(ids) != 2 || ids[0] != "bot-a" || ids[1] != "bot-b" {
		t.Fatalf("accountIDs = %v", ids)
	}
}
