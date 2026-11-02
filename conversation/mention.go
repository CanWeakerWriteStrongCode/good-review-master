package conversation

// 消息正文里的 @ 占位。
//
// 渲染时被 @ 的人还没有名字，onebot.Event.RenderText 会写成 @用户(openid)；
// 名字随后从 sender 透传或 get_group_member_info 学到，这里负责把占位换成真名。
// 待办状态不单独存：正文里还有 @用户(...) 就说明没换完。

import (
	"regexp"
	"strings"
)

// MentionPlaceholder 匹配还不是真名的 @ 占位：@用户(openid)。
// openid 是平台给的不透明串，长度和字符集都不固定，所以只约束到右括号为止。
var MentionPlaceholder = regexp.MustCompile(`@用户\(([^()\s]+)\)`)

// HasMentionPlaceholder 判断正文里还有没有没换掉的 @。
func HasMentionPlaceholder(content string) bool {
	return strings.Contains(content, "@用户(")
}

// MentionIDs 按出现顺序取出去重后的 openid。
// 同一个人 @ 两次只算一次：这里回答的是"这条消息 @ 过谁"，不是出现次数。
func MentionIDs(content string) []string {
	matches := MentionPlaceholder.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		id := match[1]
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// ResolveMentions 把正文里已知名字的占位换成 @名字(openid)，名字还不知道的原样留着。
//
// 名字里含 "@用户(" 时不换：换完正文里又多出一个占位，这条永远清不掉。
func ResolveMentions(content string, names map[string]string) string {
	if len(names) == 0 || !strings.Contains(content, "@用户(") {
		return content
	}
	return MentionPlaceholder.ReplaceAllStringFunc(content, func(match string) string {
		id := MentionPlaceholder.FindStringSubmatch(match)[1]
		name := names[id]
		if name == "" || strings.Contains(name, "@用户(") {
			return match
		}
		return "@" + name + "(" + id + ")"
	})
}
