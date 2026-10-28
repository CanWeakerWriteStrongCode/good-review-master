package onebot

import (
	"html"
	"regexp"
	"strconv"
	"strings"
)

// cqImageRe 匹配一条 CQ image 段：`[CQ:image,attr=...,...]`。
// CQ 段内属性以逗号分隔，值里的逗号/方括号会被编码（如 &#44; &#91;），所以段内不会出现裸 ]。
var cqImageRe = regexp.MustCompile(`\[CQ:image[^\]]*\]`)

// cqReplyRe 匹配一条 CQ reply 段（引用）。
var cqReplyRe = regexp.MustCompile(`\[CQ:reply[^\]]*\]`)

// cqOtherRe 匹配图片之外的 CQ 段：引用、表情、语音、视频、文件。
// Go 侧不消费这些段的结构化数据，入库时统一换成占位词，避免 URL 和 CQ 语法占 token。
var cqOtherRe = regexp.MustCompile(`\[CQ:(reply|face|record|video|file)[^\]]*\]`)

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

// ParseCQReplyID 从原始消息的 reply 段里取被引用消息的 msg_id（没有引用时为 0）。
// 桥接层写进 reply 段的 id 已经是本项目统一的 int64，可以直接与 chat log 对齐。
func ParseCQReplyID(raw string) int64 {
	segment := cqReplyRe.FindString(raw)
	if segment == "" {
		return 0
	}
	id, err := strconv.ParseInt(cqAttr(segment, "id"), 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// MaskOtherCQ 把引用/表情/语音/视频/文件段替换成可读占位。
// 文件与视频保留 URL：模型没有对应的工具时，链接本身就是它唯一能引用的信息。
func MaskOtherCQ(raw string) string {
	return cqOtherRe.ReplaceAllStringFunc(raw, func(segment string) string {
		switch {
		case strings.HasPrefix(segment, "[CQ:reply"):
			return "[引用]"
		case strings.HasPrefix(segment, "[CQ:face"):
			return "[表情]"
		case strings.HasPrefix(segment, "[CQ:record"):
			return "[语音]"
		case strings.HasPrefix(segment, "[CQ:video"):
			if url := cqAttr(segment, "url"); url != "" {
				return "[视频:" + html.UnescapeString(url) + "]"
			}
			return "[视频]"
		case strings.HasPrefix(segment, "[CQ:file"):
			name := cqAttr(segment, "name")
			url := cqAttr(segment, "url")
			switch {
			case name != "" && url != "":
				return "[文件:" + html.UnescapeString(name) + " " + html.UnescapeString(url) + "]"
			case name != "":
				return "[文件:" + html.UnescapeString(name) + "]"
			case url != "":
				return "[文件:" + html.UnescapeString(url) + "]"
			}
			return "[文件]"
		}
		return segment
	})
}

// cqAttr 取 CQ 段里某个属性值（不做段内的逗号切分之外的解析）。
func cqAttr(segment, key string) string {
	marker := key + "="
	i := strings.Index(segment, marker)
	if i < 0 {
		return ""
	}
	rest := segment[i+len(marker):]
	if end := strings.IndexByte(rest, ','); end >= 0 {
		rest = rest[:end]
	}
	rest = strings.TrimSuffix(rest, "]")
	return strings.TrimSpace(rest)
}

// NormalizeContent 由原始消息生成入库的 content 与图片 URL 列表：
// 先按未截断 raw 解析 images，再把各类 CQ 段换成占位词，最后按 rune 截断成 content。
func NormalizeContent(raw string, maxRune int) (content string, images []string) {
	raw = strings.TrimSpace(raw)
	images = ParseCQImageURLs(raw)
	content = strings.TrimSpace(MaskOtherCQ(MaskCQImages(raw)))
	if maxRune > 0 && len([]rune(content)) > maxRune {
		content = string([]rune(content)[:maxRune]) + "..."
	}
	return content, images
}

var (
	cqAtRe    = regexp.MustCompile(`\[CQ:at,qq=([^,\]]+)[^\]]*\]`)
	angleAtRe = regexp.MustCompile(`<@!?([^>\s]+)>`)
	qqbotAtRe = regexp.MustCompile(`<qqbot-at-user\s+id="([^"]+)"\s*/>`)
)

// NormalizeMentions 把残留的 @ 写法统一成「@昵称(稳定ID)」。
//
// 桥接插件正常会直接给 DisplayMessage；这里是兜底：旧版插件、其他平台适配器或
// 手工构造的事件仍可能带 CQ at / <@id>。Go 侧没有群成员目录，未知成员只能用
// 「@用户(ID)」；机器人自己能对上 selfID 时用昵称（或「@你(ID)」）。
func NormalizeMentions(text, selfID, selfName string) string {
	if text == "" {
		return text
	}
	replace := func(match []string) string {
		id := strings.TrimSpace(match[1])
		switch {
		case id == "":
			return "@未知用户"
		case strings.EqualFold(id, "all"):
			return "@所有人"
		case selfID != "" && id == selfID:
			if selfName != "" {
				return "@" + selfName + "(" + id + ")"
			}
			return "@你(" + id + ")"
		default:
			return "@用户(" + id + ")"
		}
	}
	text = cqAtRe.ReplaceAllStringFunc(text, func(segment string) string {
		return replace(cqAtRe.FindStringSubmatch(segment))
	})
	text = angleAtRe.ReplaceAllStringFunc(text, func(segment string) string {
		return replace(angleAtRe.FindStringSubmatch(segment))
	})
	text = qqbotAtRe.ReplaceAllStringFunc(text, func(segment string) string {
		return replace(qqbotAtRe.FindStringSubmatch(segment))
	})
	return text
}
