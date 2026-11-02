package state

import (
	"path/filepath"
	"testing"
)

// 报警按时间倒序返回，total 是总条数（不受 limit 影响）。
func TestLoadAlerts按时间倒序(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	st.SaveAlert(Alert{WorkspaceID: "ws-a", Kind: "window_too_small", Detail: `{"a":1}`, CreatedAt: 100})
	st.SaveAlert(Alert{WorkspaceID: "ws-b", Kind: "dead_letter_stage3", Detail: `{"b":2}`, CreatedAt: 300})
	st.SaveAlert(Alert{WorkspaceID: "ws-a", Kind: "dead_letter_stage1", Detail: `{}`, CreatedAt: 200})

	list, total, err := st.LoadAlerts(2)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(list) != 2 {
		t.Fatalf("list = %+v", list)
	}
	if list[0].Kind != "dead_letter_stage3" || list[0].CreatedAt != 300 || list[0].WorkspaceID != "ws-b" {
		t.Fatalf("第一条 = %+v，want 最新的 stage3", list[0])
	}
	if list[1].CreatedAt != 200 {
		t.Fatalf("第二条 = %+v", list[1])
	}
}

// 没开 state.db（Store 为 nil）时读报警不能 panic，按空列表返回。
func TestLoadAlerts空库(t *testing.T) {
	var st *Store
	list, total, err := st.LoadAlerts(10)
	if err != nil || total != 0 || len(list) != 0 {
		t.Fatalf("nil store 应返回空：err=%v total=%d list=%+v", err, total, list)
	}
}
