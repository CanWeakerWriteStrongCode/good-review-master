package server

import (
	"testing"

	"good-review-master/cache"
	"good-review-master/config"
)

func TestCollectDetails按工作空间归属(t *testing.T) {
	cache.ResetAll()
	defer cache.ResetAll()

	store := cache.NewStore()
	store.GetGroupCache("g1", 4).Add(cache.Message{MessageID: "1", SelfID: "openid-a", UserID: "u", Content: "a", Time: 100})
	store.GetGroupCache("g2", 4).Add(cache.Message{MessageID: "2", SelfID: "openid-a", UserID: "u", Content: "b", Time: 200})

	s := &Server{groupNames: map[string]string{"g1": "群1", "g2": "群2"}}

	groups, accounts := s.collectDetails([]string{"g1", "g2"}, "1906xxxx", store, "qq_1906xxxx", "小助理")
	if len(groups) != 2 {
		t.Fatalf("groups = %+v", groups)
	}
	if groups[0].GroupName != "群1" || groups[0].AccountName != "小助理" || !groups[0].Cached {
		t.Fatalf("g1 信息不对：%+v", groups[0])
	}
	// self_id 仍是事件里的群内 openid，只作展示，不再当账号键
	if groups[0].SelfID != "openid-a" {
		t.Fatalf("g1 self_id = %q", groups[0].SelfID)
	}
	if len(accounts) != 1 || accounts[0].SelfID != "1906xxxx" ||
		accounts[0].Nickname != "小助理" || accounts[0].GroupCount != 2 || accounts[0].MessageCount != 2 {
		t.Fatalf("accounts = %+v", accounts)
	}
}

func TestCollectDetails无账号时不造账号行(t *testing.T) {
	cache.ResetAll()
	defer cache.ResetAll()

	s := &Server{groupNames: map[string]string{}}
	groups, accounts := s.collectDetails([]string{"g1"}, "", cache.NewStore(), "", "")
	if len(groups) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
	if len(accounts) != 0 {
		t.Fatalf("没有接收账号时不应生成账号汇总：%+v", accounts)
	}
}

func TestCollectGroupDetails旧路径(t *testing.T) {
	cache.ResetAll()
	defer cache.ResetAll()

	cache.GetGroupCache("g1", 4).Add(cache.Message{MessageID: "1", SelfID: "openid-a", Content: "a", Time: 1})

	cfg := &config.Config{AllowGroups: []string{"g1"}}
	s := &Server{groupNames: map[string]string{}}
	groups, accounts := s.collectGroupDetails(cfg)
	if len(groups) != 1 || groups[0].GroupID != "g1" || !groups[0].Cached {
		t.Fatalf("groups = %+v", groups)
	}
	if len(accounts) != 0 {
		t.Fatalf("旧路径没有账号来源，不应生成账号汇总：%+v", accounts)
	}
}
