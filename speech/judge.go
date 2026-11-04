package speech

import (
	"strings"

	"good-review-master/config"
	"good-review-master/onebot"
)

// 判定：这条消息是不是冲着机器人来的。纯函数，不读 Router 状态，便于穷举单测。
// 这里不判断"要不要回"（那是主动接话的裁判，见 proactive.go），
// 只判断"有没有明确对象"，是分发前的第一道门。

// IsAddressed 判断一条消息是不是冲机器人来的：私聊、标准 mention 段、
// 实现端 @ 标记，或手打的「@昵称」都算。
//
// allowNickname 控制昵称前缀是否作数：多账号且昵称重名时，@昵称 分不清是哪个号，
// 调用方要传 false（工作空间里只有一个号、或昵称唯一时才传 true）。
//
// 只 @ 了别人（mention 段里没有本账号）时返回 false——这种消息不该回复。
func IsAddressed(event onebot.Event, content string, cfg *config.Config, allowNickname bool) bool {
	if event.IsPrivate() || event.MentionsBot() || event.AdapterAtMe() {
		return true
	}
	// 「@你」不作数：实现端不会把 @ 替换成这个字面量，它出现在正文里
	// 只可能是聊天内容本身。
	return allowNickname && cfg.BotNickname != "" && strings.HasPrefix(content, "@"+cfg.BotNickname)
}

// stripCQPrefix 去除消息开头的 @ 标记，让后面的文本能命中关键字。
// 支持「@昵称(ID)」与手打的「@昵称」两种写法。
// 只用于路由匹配；聊天记录里的完整内容不会被改动。
func stripCQPrefix(rawMsg string, cfg *config.Config) string {
	text := strings.TrimSpace(rawMsg)
	// 每轮只剥一个标记，剥完再看下一个；一条消息可能先后 @ 了多个人，机器人不一定排第一
	for {
		before := text
		if trimmed := trimLeadingMentionToken(text); trimmed != text {
			text = trimmed
		} else if cfg.BotNickname != "" && strings.HasPrefix(text, "@"+cfg.BotNickname) {
			text = strings.TrimPrefix(text, "@"+cfg.BotNickname)
		}
		text = strings.TrimSpace(text)
		if text == before {
			break
		}
	}
	return text
}

// trimLeadingMentionToken 去掉开头的「@昵称(ID)」；没有 ID 括号时原样返回。
func trimLeadingMentionToken(text string) string {
	if !strings.HasPrefix(text, "@") {
		return text
	}
	open := strings.IndexByte(text, '(')
	if open < 0 {
		return text
	}
	closeIdx := strings.IndexByte(text[open+1:], ')')
	if closeIdx < 0 {
		return text
	}
	return text[open+1+closeIdx+1:]
}
