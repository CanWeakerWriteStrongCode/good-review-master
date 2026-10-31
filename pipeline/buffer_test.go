package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"good-review-master/cache"
	"good-review-master/onebot"
)

func mustItem(t *testing.T, stage, key, id string, payload any) Item {
	t.Helper()
	item, err := NewItem(stage, key, id, payload)
	if err != nil {
		t.Fatalf("NewItem: %v", err)
	}
	return item
}

func TestMemoryBuffer同分区FIFO(t *testing.T) {
	b := NewMemoryBuffer(8, 8)
	ctx := context.Background()
	for _, id := range []string{"1", "2", "3"} {
		if err := b.Put(ctx, mustItem(t, StageIngest, SerialKey, id, InboundEvent{})); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	got, err := b.Take(ctx, SerialKey, 10)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应取 3 条，得到 %d", len(got))
	}
	for i, item := range got {
		want := []string{"ingest|serial|1", "ingest|serial|2", "ingest|serial|3"}[i]
		if item.ID != want {
			t.Fatalf("第 %d 条 = %q, want %q", i, item.ID, want)
		}
	}
	if b.Total() != 0 {
		t.Fatalf("取完后应为空，实际 %d", b.Total())
	}
}

func TestMemoryBuffer批量上限与分区隔离(t *testing.T) {
	b := NewMemoryBuffer(8, 8)
	ctx := context.Background()
	for _, id := range []string{"a1", "a2", "a3"} {
		_ = b.Put(ctx, mustItem(t, StageSync, "a", id, InboundEvent{}))
	}
	_ = b.Put(ctx, mustItem(t, StageSync, "b", "b1", InboundEvent{}))

	first, err := b.Take(ctx, "a", 2)
	if err != nil || len(first) != 2 {
		t.Fatalf("第一批 = %d, %v", len(first), err)
	}
	second, err := b.Take(ctx, "a", 2)
	if err != nil || len(second) != 1 {
		t.Fatalf("第二批 = %d, %v", len(second), err)
	}
	other, err := b.Take(ctx, "b", 2)
	if err != nil || len(other) != 1 || other[0].ID != "sync|b|b1" {
		t.Fatalf("分区 b = %+v, %v", other, err)
	}
}

func TestMemoryBuffer满时等待并尊重超时(t *testing.T) {
	b := NewMemoryBuffer(1, 1)
	item := mustItem(t, StageIngest, SerialKey, "1", InboundEvent{})
	if err := b.Put(context.Background(), item); err != nil {
		t.Fatalf("首次 Put: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := b.Put(ctx, mustItem(t, StageIngest, SerialKey, "2", InboundEvent{})); err == nil {
		t.Fatal("满队列应阻塞到超时")
	}

	// 取走一条后应能继续放。
	if _, err := b.Take(context.Background(), SerialKey, 1); err != nil {
		t.Fatalf("Take: %v", err)
	}
	if err := b.Put(context.Background(), item); err != nil {
		t.Fatalf("腾出空间后 Put: %v", err)
	}
}

func TestMemoryBuffer关闭后唤醒阻塞方(t *testing.T) {
	b := NewMemoryBuffer(1, 1)
	ctx := context.Background()
	_ = b.Put(ctx, mustItem(t, StageIngest, SerialKey, "1", InboundEvent{}))

	putDone := make(chan error, 1)
	go func() {
		putDone <- b.Put(ctx, mustItem(t, StageIngest, SerialKey, "2", InboundEvent{}))
	}()
	time.Sleep(20 * time.Millisecond)
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-putDone:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Close 后 Put 应返回 ErrClosed，得到 %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close 未唤醒阻塞的 Put")
	}

	// 关闭后先排空已有项，再返回 ErrClosed。
	if got, err := b.Take(ctx, SerialKey, 1); err != nil || len(got) != 1 {
		t.Fatalf("关闭后应能排空，得到 %+v, %v", got, err)
	}
	if _, err := b.Take(ctx, SerialKey, 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("排空后应返回 ErrClosed，得到 %v", err)
	}
}

func Test阶段载荷可序列化(t *testing.T) {
	in := InboundEvent{
		WorkspaceID: "qq_official_bot_1906",
		Event: onebot.Event{
			MessageID:   "m1",
			GroupID:     "g1",
			UserID:      "u1",
			AccountKind: "official_bot",
			Self:        onebot.Self{Platform: "qq", UserID: "u1"},
			Message:     []onebot.Segment{onebot.TextSegment("你好")},
			QQMsgIdx:    "REFIDX",
			QQAtMe:      true,
		},
	}
	item := mustItem(t, StageIngest, SerialKey, "m1", in)
	got, err := DecodeItem[InboundEvent](item)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.WorkspaceID != in.WorkspaceID || got.Event.MessageID != "m1" ||
		got.Event.AccountKind != "official_bot" || !got.Event.QQAtMe {
		t.Fatalf("InboundEvent 往返丢失字段：%+v", got)
	}

	enr := EnrichedMessage{
		SyncedMessage: SyncedMessage{
			WorkspaceID: "w1",
			ChatID:      "g1",
			Message: cache.Message{
				MessageID: "m1",
				GroupID:   "g1",
				UserID:    "u1",
				Content:   "你好",
				ImageIDs:  []cache.ImageID{{SHA: "abc", Size: 3}},
			},
		},
		ImageNotes: []cache.ImageNote{{ImageID: cache.ImageID{SHA: "abc", Size: 3}, Note: "一张图"}},
	}
	item = mustItem(t, StageAssets, SerialKey, "m1", enr)
	back, err := DecodeItem[EnrichedMessage](item)
	if err != nil {
		t.Fatalf("Decode Enriched: %v", err)
	}
	if back.Message.Content != "你好" || len(back.ImageNotes) != 1 ||
		back.ImageNotes[0].Note != "一张图" || len(back.Message.ImageIDs) != 1 {
		t.Fatalf("EnrichedMessage 往返丢失字段：%+v", back)
	}
}
