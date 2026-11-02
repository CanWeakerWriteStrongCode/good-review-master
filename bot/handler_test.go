package bot

import (
	"testing"
)

// "这条消息是不是冲机器人来的" 在 speech.IsAddressed 里判，
// 对应单测也搬到了 speech/judge_test.go。这里只剩本包自己的小工具。
func TestTruncate按字符截断(t *testing.T) {
	text := "今天好累啊"
	if got := truncate(text, 3); got != "今天好" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate(text, 0); got != text {
		t.Fatalf("limit<=0 不截断，got %q", got)
	}
	if got := truncate(text, 100); got != text {
		t.Fatalf("未超限不截断，got %q", got)
	}
}
