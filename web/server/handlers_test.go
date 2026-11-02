package server

import (
	"testing"

	"good-review-master/conversation"
)

// testSelfID 是机器人这个账号的 openid（QQ 官方按账号分配，所有群同一个值，32 位十六进制）。
const testSelfID = "0C2F9A2A1E40E2FC627058D00B7ADBF2"

// 机器人自己发的话在库里没有发送者（出站回复的 user_id 常常是空的），
// 详情页要显示工作空间当前昵称，而不是前端那个「机器人」兜底词。
func TestStampSelfNickname补机器人昵称(t *testing.T) {
	msgs := []conversation.Message{
		{MessageID: "m1", UserID: "u1", Nickname: "小张"},
		{MessageID: "bot-1", UserID: ""},
	}

	stampSelfNickname(msgs, "好评大师的小助理", "")

	if msgs[0].Nickname != "小张" {
		t.Fatalf("别人的昵称不能被改，实际 %q", msgs[0].Nickname)
	}
	if msgs[1].Nickname != "好评大师的小助理" {
		t.Fatalf("机器人自己的消息应补成当前昵称，实际 %q", msgs[1].Nickname)
	}
}

// 身份回填之后 user_id 变成机器人自己的 openid，昵称照样取当前值：
// members 里那一行可能没有，也可能只是个旧群名片。
func TestStampSelfNickname回填后用当前昵称(t *testing.T) {
	msgs := []conversation.Message{
		{MessageID: "bot-1", UserID: testSelfID, Nickname: "旧群名片"},
		{MessageID: "m2", UserID: "u1", Nickname: "小张"},
	}

	stampSelfNickname(msgs, "好评大师的小助理", testSelfID)

	if msgs[0].Nickname != "好评大师的小助理" {
		t.Fatalf("机器人自己的行应换成当前昵称，实际 %q", msgs[0].Nickname)
	}
	if msgs[1].Nickname != "小张" {
		t.Fatalf("别人的昵称不能被改，实际 %q", msgs[1].Nickname)
	}
}

// 昵称取当前值而不是入库快照：机器人改名后，以前说过的话也跟着显示新名字。
func TestStampSelfNickname改名后用新名(t *testing.T) {
	msgs := []conversation.Message{{MessageID: "bot-1"}}

	stampSelfNickname(msgs, "旧名", "")
	stampSelfNickname(msgs, "新名", "")

	if msgs[0].Nickname != "新名" {
		t.Fatalf("应覆盖成最新昵称，实际 %q", msgs[0].Nickname)
	}
}

// 拿不到工作空间昵称时保持原样：前端仍会落到「机器人」兜底，
// 但不能在这里写入空串之外的东西。
func TestStampSelfNickname无昵称时不动(t *testing.T) {
	msgs := []conversation.Message{{MessageID: "bot-1", Nickname: "入库时的旧名"}}

	stampSelfNickname(msgs, "", "")

	if msgs[0].Nickname != "入库时的旧名" {
		t.Fatalf("昵称为空时不该改，实际 %q", msgs[0].Nickname)
	}
}

// 空发送者不等于机器人：入站时就没带 user_id 的怪消息也是空的，但它没有
// bot- 前缀（那才是出站回复的命名），不能给它贴机器人的名字。
func TestStampSelfNickname不认来源不明的空发送者(t *testing.T) {
	msgs := []conversation.Message{{MessageID: "m2", UserID: "", Content: "来源不明"}}

	stampSelfNickname(msgs, "好评大师的小助理", testSelfID)

	if msgs[0].Nickname != "" {
		t.Fatalf("不是机器人发的行不该改，实际 %q", msgs[0].Nickname)
	}
}
