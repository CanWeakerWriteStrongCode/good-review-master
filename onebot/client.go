package onebot

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"good-review-master/logutil"

	"github.com/go-resty/resty/v2"
)

// Account 一个机器人账号的出站连接：ID 是工作空间 ID（platform_account），
// APIBase 是该账号在实现端的 OneBot 12 HTTP API 根地址。
type Account struct {
	ID      string
	APIBase string
}

// Client 是 OneBot 12 HTTP API 客户端。
//
// 一个进程里只有一个根 Client（持有共享的 resty 连接），按工作空间 ID 用 For()
// 取到绑定该账号 base URL 的轻量副本；动作请求因此不需要扩展参数 self_id。
type Client struct {
	token string
	base  string
	bases map[string]string
	resty *resty.Client
}

// NewClient 创建多账号客户端。base URL 末尾的 / 会被去掉，调用时统一拼 /。
func NewClient(accounts []Account, token string) *Client {
	bases := make(map[string]string, len(accounts))
	for _, account := range accounts {
		id := strings.TrimSpace(account.ID)
		base := strings.TrimRight(strings.TrimSpace(account.APIBase), "/")
		if id != "" && base != "" {
			bases[id] = base
		}
	}
	restyClient := resty.New().
		SetHeader("Content-Type", "application/json").
		SetTimeout(10 * time.Second).
		SetRetryCount(2)
	if token != "" {
		restyClient.SetAuthToken(token)
	}
	return &Client{token: token, bases: bases, resty: restyClient}
}

// For 返回绑定指定工作空间的客户端。没有配置 base URL 时返回一个不可用的客户端，
// 调用动作会报错而不是悄悄发到别的账号。
func (c *Client) For(workspaceID string) *Client {
	if c == nil {
		return nil
	}
	return &Client{token: c.token, base: c.bases[workspaceID], bases: c.bases, resty: c.resty}
}

// Base 返回当前客户端绑定的 base URL（空表示未配置）。
func (c *Client) Base() string {
	if c == nil {
		return ""
	}
	return c.base
}

// SelfInfo 是 get_self_info 的响应数据。
type SelfInfo struct {
	UserID          string `json:"user_id"`
	UserName        string `json:"user_name"`
	UserDisplayName string `json:"user_displayname"`
}

// GroupInfo 是 get_group_info 的响应数据。
type GroupInfo struct {
	GroupID   string `json:"group_id"`
	GroupName string `json:"group_name"`
}

// MemberInfo 是 get_group_member_info 的响应数据。
type MemberInfo struct {
	UserID          string `json:"user_id"`
	UserName        string `json:"user_name"`
	UserDisplayName string `json:"user_displayname"`
}

// DisplayName 优先群名片，其次账号名。
func (m MemberInfo) DisplayName() string {
	if m.UserDisplayName != "" {
		return m.UserDisplayName
	}
	return m.UserName
}

// GetSelfInfo 查询机器人自身信息。
func (c *Client) GetSelfInfo() (*SelfInfo, error) {
	info, err := call[SelfInfo](c, "get_self_info", map[string]any{})
	if err != nil {
		return nil, err
	}
	return info, nil
}

// UserInfo 是 get_user_info 的响应数据。
type UserInfo struct {
	UserID          string `json:"user_id"`
	UserName        string `json:"user_name"`
	UserDisplayName string `json:"user_displayname"`
}

// VersionInfo 是 get_version 的响应数据。
type VersionInfo struct {
	Impl          string `json:"impl"`
	Version       string `json:"version"`
	OneBotVersion string `json:"onebot_version"`
	Platform      string `json:"platform,omitempty"`
}

// GetGroupInfo 获取群信息（群名称等）。
func (c *Client) GetGroupInfo(groupID string) (*GroupInfo, error) {
	return call[GroupInfo](c, "get_group_info", map[string]any{"group_id": groupID})
}

// GetUserInfo 查询用户信息（无群上下文时使用）。
func (c *Client) GetUserInfo(userID string) (*UserInfo, error) {
	return call[UserInfo](c, "get_user_info", map[string]any{"user_id": userID})
}

// GetVersion 查询实现端版本与 OneBot 版本。
func (c *Client) GetVersion() (*VersionInfo, error) {
	return call[VersionInfo](c, "get_version", map[string]any{})
}

// GetSupportedActions 查询实现端支持的动作，用于能力协商。
func (c *Client) GetSupportedActions() ([]string, error) {
	data, err := call[[]string](c, "get_supported_actions", map[string]any{})
	if err != nil {
		return nil, err
	}
	return *data, nil
}

// GetGroupMemberInfo 获取群成员信息（昵称/群名片）。
func (c *Client) GetGroupMemberInfo(groupID, userID string) (*MemberInfo, error) {
	return call[MemberInfo](c, "get_group_member_info", map[string]any{
		"group_id": groupID,
		"user_id":  userID,
	})
}

// SendGroupMessage 发送纯文本群消息。
func (c *Client) SendGroupMessage(groupID, content string) {
	c.SendGroupMessageSegments(groupID, []Segment{TextSegment(content)})
}

// SendGroupReply 发送「引用 + @ + 文本」的群消息。
// replyID 是触发消息的 message_id，插件据此做 QQ 被动回复；为空时退化为普通文本。
func (c *Client) SendGroupReply(groupID, replyID, atUser, content string) {
	segments := make([]Segment, 0, 3)
	if replyID != "" {
		segments = append(segments, ReplySegment(replyID))
	}
	if atUser != "" {
		segments = append(segments, MentionSegment(atUser))
		content = " " + content
	}
	segments = append(segments, TextSegment(content))
	c.SendGroupMessageSegments(groupID, segments)
}

// SendGroupMessageSegments 发送结构化群消息。失败只记日志，不阻塞业务。
func (c *Client) SendGroupMessageSegments(groupID string, segments []Segment) {
	if c == nil || c.base == "" {
		logutil.Error("发送群消息失败：该账号没有配置出站地址", "group", groupID)
		return
	}
	params := map[string]any{
		"detail_type": "group",
		"group_id":    groupID,
		"message":     segments,
	}
	payload, err := json.Marshal(params)
	if err != nil {
		logutil.Error("send_message 请求序列化失败", "err", err, "group", groupID)
		return
	}
	logutil.Debug("调用 OneBot API", "action", "send_message", "group", groupID,
		"segments", len(segments), "types", segmentTypes(segments))
	if _, err := call[struct{}](c, "send_message", params); err != nil {
		logutil.Error("send_message 被平台拒绝", "err", err, "group", groupID, "request", string(payload))
		return
	}
	logutil.Info("send_message 成功", "group", groupID, "segments", len(segments), "request", string(payload))
}

// segmentTypes 返回消息段类型列表，仅用于日志排查。
func segmentTypes(segments []Segment) []string {
	types := make([]string, 0, len(segments))
	for _, seg := range segments {
		types = append(types, seg.Type)
	}
	return types
}

// ActionRequest 是 OneBot 12 动作请求的公共外壳。Echo 原样回传，用于把
// 并发请求的响应配回请求；本项目串行发送，仍然按标准带上。
type ActionRequest struct {
	Action string `json:"action"`
	Params any    `json:"params"`
	Echo   any    `json:"echo,omitempty"`
}

// actionResponse 是 OneBot 12 动作响应的公共外壳。
type actionResponse[T any] struct {
	Status  string `json:"status"`
	Retcode int64  `json:"retcode"`
	Data    T      `json:"data"`
	Message string `json:"message"`
	Echo    string `json:"echo"`
}

func (r actionResponse[T]) err(action string) error {
	if r.Status != "ok" || r.Retcode != 0 {
		return fmt.Errorf("%s 失败: status=%s retcode=%d message=%s", action, r.Status, r.Retcode, r.Message)
	}
	return nil
}

// call 发一个 OneBot 12 动作请求并解出 data。
func call[T any](c *Client, action string, params any) (*T, error) {
	if c == nil || c.base == "" {
		return nil, fmt.Errorf("%s 未配置出站地址", action)
	}
	body := ActionRequest{Action: action, Params: params, Echo: action}
	var result actionResponse[T]
	resp, err := c.resty.R().
		SetBody(body).
		SetResult(&result).
		Post(c.base + "/")
	if err != nil {
		return nil, fmt.Errorf("%s 请求失败: %w", action, err)
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("%s HTTP %d: %s", action, resp.StatusCode(), resp.Body())
	}
	if err := result.err(action); err != nil {
		return nil, err
	}
	return &result.Data, nil
}
