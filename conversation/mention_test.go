package conversation

import "testing"

// TestMentionIDs只认完整占位 名字没学到时正文里是 @用户(openid)；
// @全体、被截断的半个占位、普通文本都不算。
func TestMentionIDs只认完整占位(t *testing.T) {
	const (
		fish  = "DBF09B44D411F63340CE2924D744BA86"
		rice  = "ADA3988201B97492C659ADDAB0EAF113"
		spare = "51999F19A1B2C3D4E5F60718293A4B5C"
	)
	cases := []struct {
		content string
		want    []string
	}{
		{"@用户(" + rice + ") 你好啊", []string{rice}},
		// 同一个人 @ 两次只记一次，顺序按第一次出现
		{"@用户(" + fish + ") @用户(" + rice + ") @用户(" + fish + ") 你俩看下", []string{fish, rice}},
		{"今天好热", nil},
		{"@所有人 下午三点开会", nil},
		// 正文被 max_msg_rune 截断，占位只剩一半：认不出来，也就没法换
		{"@用户(" + spare[:8] + " 你好", nil},
	}
	for _, tc := range cases {
		got := MentionIDs(tc.content)
		if len(got) != len(tc.want) {
			t.Fatalf("%q 的 MentionIDs = %v, want %v", tc.content, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%q 的 MentionIDs = %v, want %v", tc.content, got, tc.want)
			}
		}
	}
}

// TestResolveMentions只换有名字的 一条消息 @ 两个人时，只知道一个人的名字就只换那个人；
// 名字里带占位形态的不换，否则正文里会多出一个永远清不掉的 @用户(。
func TestResolveMentions只换有名字的(t *testing.T) {
	const (
		fish = "DBF09B44D411F63340CE2924D744BA86"
		rice = "ADA3988201B97492C659ADDAB0EAF113"
	)
	names := map[string]string{fish: "谁是大肥鱼"}

	got := ResolveMentions("@用户("+fish+") @用户("+rice+") 你好", names)
	want := "@谁是大肥鱼(" + fish + ") @用户(" + rice + ") 你好"
	if got != want {
		t.Fatalf("ResolveMentions = %q, want %q", got, want)
	}
	if !HasMentionPlaceholder(got) || HasMentionPlaceholder(ResolveMentions(got, map[string]string{rice: "给你家白饭倒可乐的大肥鱼"})) {
		t.Fatal("两个人都有名字后正文里不应再有占位")
	}

	bad := ResolveMentions("@用户("+rice+") 在吗", map[string]string{rice: "@用户(坏名字"})
	if bad != "@用户("+rice+") 在吗" {
		t.Fatalf("名字本身含占位形态时不应替换，实际 %q", bad)
	}
}
