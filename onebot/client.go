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
	logutil.Info("调用 NapCat API", "action", "get_login_info")
	var result struct {
		Status string    `json:"status"`
		Data   LoginInfo `json:"data"`
	}
	resp, err := ob.restyClient.R().
		SetResult(&result).
		Post("/get_login_info")
	if err != nil {
		return nil, fmt.Errorf("get_login_info 请求失败: %w", err)
	}
	if resp.StatusCode() != 200 {
		return nil, fmt.Errorf("get_login_info HTTP %d: %s", resp.StatusCode(), resp.Body())
	}
	logutil.Info("get_login_info 成功", "nickname", result.Data.Nickname, "user_id", result.Data.UserID)
	return &result.Data, nil
}

// GetGroupInfo 获取群信息（群名称等）
func (ob *Client) GetGroupInfo(groupID string) (*GroupInfo, error) {
	logutil.Debug("调用 NapCat API", "action", "get_group_info", "group", groupID)
	var result struct {
		Status string    `json:"status"`
		Data   GroupInfo `json:"data"`
	}
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
	return &result.Data, nil
}

// SendGroupMessage 发送群消息
func (ob *Client) SendGroupMessage(groupID, content string) {
	logutil.Debug("调用 NapCat API", "action", "send_group_msg", "group", groupID)
	resp, err := ob.restyClient.R().
		SetBody(map[string]any{"group_id": groupID, "message": content}).
		Post("/send_group_msg")
	if err != nil {
		logutil.Error("send_group_msg 请求失败", "err", err, "group", groupID)
		return
	}
	if resp.StatusCode() != 200 {
		logutil.Error("send_group_msg 返回非200", "status", resp.StatusCode(), "body", string(resp.Body()), "group", groupID)
		return
	}
	logutil.Info("send_group_msg 成功", "group", groupID)
}

// FetchGroupMsgHistory 拉取群消息历史
func (ob *Client) FetchGroupMsgHistory(groupID string, count int) ([]HistoryMsg, error) {
	logutil.Debug("调用 NapCat API", "action", "get_group_msg_history", "group", groupID, "count", count)
	var result struct {
		Status  string `json:"status"`
		Retcode int    `json:"retcode"`
		Data    struct {
			Messages []HistoryMsg `json:"messages"`
		} `json:"data"`
	}
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
	logutil.Debug("get_group_msg_history 成功", "group", groupID, "条数", len(result.Data.Messages))
	return result.Data.Messages, nil
}
