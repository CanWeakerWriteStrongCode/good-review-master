package router

import (
	"strings"
	"testing"

	"good-review-master/cache"
)

// Test候选列表注入图片描述 有描述的行带"内容/信息量/文字"，没有描述的保持原样。
func Test候选列表注入图片描述(t *testing.T) {
	cfg := proactiveTestConfig()
	instance, _ := newProactiveTestRouter(t, cfg)
	msgs := []cache.Message{{
		MessageID: "m1", Nickname: "张三",
		Images: []string{"u1", "u2"},
		ImageNotes: []cache.ImageNote{
			{Note: "新闻截图，标题关于台风", InfoLevel: "high", HasText: true, Status: "ok"},
			{},
		},
	}}
	block := instance.imageCandidatesBlock(msgs)
	if !strings.Contains(block, "内容:新闻截图，标题关于台风 信息量:high 文字:有") {
		t.Fatalf("有描述的行应带内容/信息量：%s", block)
	}
	for _, line := range strings.Split(block, "\n") {
		if strings.Contains(line, "url:u2") && strings.Contains(line, "内容:") {
			t.Fatalf("无描述的行不应带内容：%s", line)
		}
	}
}
