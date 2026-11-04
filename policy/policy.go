// Package policy 是访问策略判定：群白名单、账号开通名单、只记录不回复名单。
//
// 这三条规则原来分散在 config（Config.HasGroup / HasWorkspaceAccount / IsRecordOnly）
// 与 workspace（Workspace.AllowsGroup）里，同一条规则两份实现，改一处漏一处。
// 集中到这里之后，每条规则只有一个实现。
//
// 本包只有纯函数：不持有状态，除 config 外不 import 项目内的任何包。
package policy

import (
	"strings"

	"good-review-master/config"
)

// wildcard 是所有名单都认的通配符。
const wildcard = "*"

// AllowsStream 判断一条信息流是否放行。
//
// streamID 现在是群号；接论坛、邮件之后是板块号或邮件会话 ID —— 判定规则一样：
// 名单里有它，或者名单里有 "*"。
func AllowsStream(cfg *config.Config, streamID string) bool {
	if cfg == nil {
		return false
	}
	return contains(cfg.AllowGroups, wildcard) || contains(cfg.AllowGroups, streamID)
}

// AllowsAllStreams 返回群白名单是否包含通配符。
//
// 通配符无法枚举具体群，面板的群列表要单独处理：有 "*" 时只能列已经产生过消息的群。
func AllowsAllStreams(cfg *config.Config) bool {
	return cfg != nil && contains(cfg.AllowGroups, wildcard)
}

// AllowsAccount 判断某个通道是否允许自动开通。名单含 "*" 时放行全部。
//
// 只用于「新建」判定：已经存在的通道由 Manager 直接加载，不受这里影响 ——
// 否则把一个 ID 从名单里删掉，会让一个正在运行的账号突然失联。
func AllowsAccount(cfg *config.Config, channelID string) bool {
	if cfg == nil {
		return false
	}
	return contains(cfg.WorkspaceAccounts, wildcard) || contains(cfg.WorkspaceAccounts, channelID)
}

// AllowsAllAccounts 返回账号名单是否包含通配符。
func AllowsAllAccounts(cfg *config.Config) bool {
	return cfg != nil && contains(cfg.WorkspaceAccounts, wildcard)
}

// IsRecordOnly 判断某个通道是否「只记录不回复」。
//
// 这类账号照常入库、打图片标签、写进群聊缓存，只是不触发任何回复；
// 被 @ 或私聊时回一句固定话术。主备双号时给备份号配。
//
// 不支持 "*"：全部静默等于没有机器人，配置校验就拒绝了。
func IsRecordOnly(cfg *config.Config, channelID string) bool {
	return cfg != nil && contains(cfg.RecordOnlyAccounts, channelID)
}

// privateChatPrefix 是私聊会话 ID 的前缀（见 onebot.Event.ChatID）：
// 私聊的 ID 是 "private:" + userID，无法当作固定的群 ID 配置。
const privateChatPrefix = "private:"

// CanUseCommand 判断某个会话成员能否执行管理指令（Category == "internal"）。
//
// 只认精确的群 ID：没配这个群、或这个群里没有这个人 → 拒绝（fail-closed）。
// 私聊直接放行：对话人只有账号主人自己，没有越权面。
func CanUseCommand(cfg *config.Config, chatID, userID string) bool {
	if cfg == nil {
		return false
	}
	if strings.HasPrefix(chatID, privateChatPrefix) {
		return true
	}
	admins, ok := cfg.CommandAdmins[chatID]
	if !ok {
		return false
	}
	return contains(admins, userID)
}

// contains 是这几个名单唯一的匹配方式：逐项相等比较，不做前缀或正则匹配。
func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
