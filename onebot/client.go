package onebot

import (
	"fmt"
	"time"

	"good-review-master/logutil"

	"github.com/go-resty/resty/v2"
)

// Client NapCat HTTP API 客户端（基于 resty）
type Client struct {
	httpAPI     string
	accessToken string
	restyClient *resty.Client
}

// NewClient 创建 OneBot HTTP 客户端
func NewClient(httpAPI, accessToken string) *Client {
	restyCli := resty.New().
		SetBaseURL(httpAPI).
		SetHeader("Content-Type", "application/json").
		SetTimeout(10 * time.Second).
		SetRetryCount(2)

	if accessToken != "" {
		restyCli.SetAuthToken(accessToken)
	}

	return &Client{
		httpAPI:     httpAPI,
		accessToken: accessToken,
		restyClient: restyCli,
	}
}

// GetLoginInfo 获取机器人登录信息（QQ号、昵称）
func (ob *Client) GetLoginInfo() (*LoginInfo, error) {
	logutil.Info("调用 OneBot API", "action", "get_login_info")
	var result actionResponse[LoginInfo]
	resp, err := ob.restyClient.R().
		SetResult(&result).
		Post("/get_login_info")
	if err != nil {
		return nil, fmt.Errorf("get_login_info 请求失败: %w", err)
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("get_login_info HTTP %d: %s", resp.StatusCode(), resp.Body())
	}
	if err := result.err("get_login_info"); err != nil {
		return nil, err
	}
	logutil.Info("get_login_info 成功", "nickname", result.Data.Nickname, "user_id", result.Data.UserID)
	return &result.Data, nil
}

// GetGroupInfo 获取群信息（群名称等）
func (ob *Client) GetGroupInfo(groupID string) (*GroupInfo, error) {
	logutil.Debug("调用 OneBot API", "action", "get_group_info", "group", groupID)
	var result actionResponse[GroupInfo]
	resp, err := ob.restyClient.R().
		SetBody(map[string]any{"group_id": groupID}).
		SetResult(&result).
		Post("/get_group_info")
	if err != nil {
		return nil, fmt.Errorf("get_group_info 请求失败: %w", err)
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("get_group_info HTTP %d: %s", resp.StatusCode(), resp.Body())
	}
	if err := result.err("get_group_info"); err != nil {
		return nil, err
	}
	return &result.Data, nil
}

// SendGroupMessage 发送群消息
func (ob *Client) SendGroupMessage(groupID, content string) {
	logutil.Debug("调用 OneBot API", "action", "send_group_msg", "group", groupID, "content", content)
	var result actionResponse[struct{}]
	resp, err := ob.restyClient.R().
		SetBody(map[string]any{"group_id": groupID, "message": content}).
		SetResult(&result).
		Post("/send_group_msg")
	if err != nil {
		logutil.Error("send_group_msg 请求失败", "err", err, "group", groupID)
		return
	}
	if resp.StatusCode() != 200 {
		logutil.Error("send_group_msg 返回非200", "status", resp.StatusCode(), "body", string(resp.Body()), "group", groupID)
		return
	}
	if err := result.err("send_group_msg"); err != nil {
		logutil.Error("send_group_msg 被平台拒绝", "err", err, "group", groupID)
		return
	}
	logutil.Info("send_group_msg 成功", "group", groupID)
}

// FetchGroupMsgHistory 拉取群消息历史
func (ob *Client) FetchGroupMsgHistory(groupID string, count int) ([]HistoryMsg, error) {
	logutil.Debug("调用 OneBot API", "action", "get_group_msg_history", "group", groupID, "count", count)
	var result actionResponse[struct {
		Messages []HistoryMsg `json:"messages"`
	}]
	resp, err := ob.restyClient.R().
		SetBody(map[string]any{"group_id": groupID, "count": count}).
		SetResult(&result).
		Post("/get_group_msg_history")
	if err != nil {
		logutil.Error("get_group_msg_history 请求失败", "err", err, "group", groupID)
		return nil, fmt.Errorf("get_group_msg_history 请求失败: %w", err)
	}
	if resp.StatusCode() != 200 {
		logutil.Error("get_group_msg_history 返回非200", "status", resp.StatusCode(), "body", string(resp.Body()), "group", groupID)
		return nil, fmt.Errorf("get_group_msg_history HTTP %d", resp.StatusCode())
	}
	if err := result.err("get_group_msg_history"); err != nil {
		return nil, err
	}
	logutil.Debug("get_group_msg_history 成功", "group", groupID, "条数", len(result.Data.Messages))
	return result.Data.Messages, nil
}

// actionResponse 是 OneBot 11 动作响应的公共外壳。
//
// 必须看 retcode 而不只是 HTTP 状态：桥接层/协议端会把"平台拒绝"表达成
// HTTP 200 + retcode!=0（例如 aiocqhttp 未连接时的 1404），只看状态码会把失败当成功。
type actionResponse[T any] struct {
	Status  string `json:"status"`
	Retcode int    `json:"retcode"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

func (r actionResponse[T]) err(action string) error {
	// status=async 是 OneBot 11 里"已受理、稍后完成"的正常响应，retcode 可能非 0；
	// 只有 failed 或同步动作的非 0 retcode 才算失败。
	if r.Status == "failed" || (r.Status != "async" && r.Retcode != 0) {
		return fmt.Errorf("%s 失败: status=%s retcode=%d message=%s", action, r.Status, r.Retcode, r.Message)
	}
	return nil
}
