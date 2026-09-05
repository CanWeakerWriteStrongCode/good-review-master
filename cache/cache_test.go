package cache

import "testing"

// TestBuildChatLogJSONLines 锁定 BuildChatLog 的 JSON 行格式：
// 每条非空消息一行 {"user":"昵称","user_id":QQ,"content":"内容"}；
// 空内容跳过；UserID 空/非数字时省略 user_id；引号等被转义。
func TestBuildChatLogJSONLines(t *testing.T) {
	in := []Message{
		{Nick: "张三", UserID: "123456", Content: "今天好累"},
		{Nick: "李四", UserID: "888888", Content: "你也好"},
		{Nick: "空消息", UserID: "1", Content: ""},     // 空内容跳过
		{Nick: "无名", UserID: "", Content: "没QQ号"},   // UserID 为空 → 省略 user_id
		{Nick: "非数字", UserID: "abc", Content: "特殊"}, // UserID 非数字 → 省略 user_id
		{Nick: `引号"君`, UserID: "9", Content: `他说"嗨"`},
	}
	got := BuildChatLog(in)
	want := `{"user":"张三","user_id":123456,"content":"今天好累"}
{"user":"李四","user_id":888888,"content":"你也好"}
{"user":"无名","content":"没QQ号"}
{"user":"非数字","content":"特殊"}
{"user":"引号\"君","user_id":9,"content":"他说\"嗨\""}
`
	if got != want {
		t.Fatalf("BuildChatLog 输出不符:\ngot:\n%q\nwant:\n%q", got, want)
	}
}
