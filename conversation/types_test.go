package conversation

import (
	"testing"

	"good-review-master/onebot"
	"good-review-master/pipeline"
)

// TestInbound可序列化 阶段载荷要能原样过缓冲（以后还要能过消息队列）。
func TestInbound可序列化(t *testing.T) {
	in := Inbound{
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
	item, err := pipeline.NewItem(pipeline.StageIngest, pipeline.SerialKey, "m1", in)
	if err != nil {
		t.Fatalf("NewItem: %v", err)
	}
	got, err := pipeline.DecodeItem[Inbound](item)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.WorkspaceID != in.WorkspaceID || got.Event.MessageID != "m1" ||
		got.Event.AccountKind != "official_bot" || !got.Event.QQAtMe {
		t.Fatalf("Inbound 往返丢失字段：%+v", got)
	}
}

// TestSynced带图片描述过缓冲 图片描述以前挂在 EnrichedMessage 上，
// 因为 Message.ImageNotes 是 json:"-"。现在同一个值对象走完全程，
// 这里守住"Stage 2 填的东西 Stage 3 还看得到"。
func TestSynced带图片描述过缓冲(t *testing.T) {
	id := ImageID{SHA: "abc", Size: 3}
	synced := Synced{
		WorkspaceID: "w1",
		ChatID:      "g1",
		Message: Message{
			MessageID: "m1",
			GroupID:   "g1",
			UserID:    "u1",
			Content:   "你好",
			ImageIDs:  []ImageID{id},
			ImageNotes: []ImageNote{
				{ImageID: id, Note: "一张图"},
			},
		},
		Assets: []Asset{
			{Index: 0, Kind: "image", URL: "http://x/1.jpg", ImageID: id, Status: StatusOK},
		},
		AssetStatus: StatusOK,
	}
	item, err := pipeline.NewItem(pipeline.StageAssets, pipeline.SerialKey, "m1", synced)
	if err != nil {
		t.Fatalf("NewItem: %v", err)
	}
	back, err := pipeline.DecodeItem[Synced](item)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if back.Message.Content != "你好" || len(back.Message.ImageNotes) != 1 ||
		back.Message.ImageNotes[0].Note != "一张图" || len(back.Message.ImageIDs) != 1 {
		t.Fatalf("图片描述没带过去：%+v", back.Message)
	}
	if len(back.Assets) != 1 || back.Assets[0].Status != StatusOK || back.AssetStatus != StatusOK {
		t.Fatalf("资产信息没带过去：%+v", back)
	}
}
