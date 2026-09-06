package cache

import (
	"html"
	"regexp"
	"strings"
)

// cqImageRe 匹配一条 CQ image 段：`[CQ:image,attr=...,...]`。
// CQ 段内属性以逗号分隔，值里的逗号/方括号会被编码（如 &#44; &#91;），所以段内不会出现裸 ]。
var cqImageRe = regexp.MustCompile(`\[CQ:image[^\]]*\]`)

// ParseCQImageURLs 从原始消息里提取所有 CQ image 的 url（逐个 HTML 解码，去空）。
// 必须在入库截断前用未截断的 raw 调用：长 url 被 rune 截断后会变成残码。
func ParseCQImageURLs(raw string) []string {
	var urls []string
	for _, seg := range cqImageRe.FindAllString(raw, -1) {
		u := cqImageURL(seg)
		if u == "" {
			continue
		}
		urls = append(urls, u)
	}
	return urls
}

// cqImageURL 取单条 image 段内 url 属性值并 HTML 解码。
// url 原始值内不含裸逗号（逗号是 CQ 的属性分隔符，字面逗号会被编码成 &#44;），
// 故取到下一个裸逗号/结束即完整；解码把 &amp; &#44; &#91; 等还原。
func cqImageURL(seg string) string {
	i := strings.Index(seg, "url=")
	if i < 0 {
		return ""
	}
	rest := seg[i+len("url="):]
	end := strings.IndexByte(rest, ',')
	if end < 0 {
		end = len(rest)
	}
	v := rest[:end]
	// 段可能以 ] 结尾且该值恰在末尾：去掉可能残留的 ]
	v = strings.TrimSuffix(v, "]")
	return strings.TrimSpace(html.UnescapeString(v))
}

// MaskCQImages 把 CQ image 段替换成占位「[图片]」：
// 避免把整段（含超长 url）塞进聊天文本占 token，也避免截断产生残码。
func MaskCQImages(raw string) string {
	return cqImageRe.ReplaceAllString(raw, "[图片]")
}

// NormalizeContent 由原始消息生成入库的 content 与图片 URL 列表：
// 先按未截断 raw 解析 images，再 MaskCQImages 后按 rune 截断成 content。
func NormalizeContent(raw string, maxRune int) (content string, images []string) {
	raw = strings.TrimSpace(raw)
	images = ParseCQImageURLs(raw)
	content = strings.TrimSpace(MaskCQImages(raw))
	if maxRune > 0 && len([]rune(content)) > maxRune {
		content = string([]rune(content)[:maxRune]) + "..."
	}
	return content, images
}
