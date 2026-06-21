package onebot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"good-review-master/config"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

// SendGroupMessage 发送群消息
func SendGroupMessage(groupID, content string) {
	slog.Debug("调用 NapCat API", "action", "send_group_msg", "group", groupID)
	body, _ := json.Marshal(map[string]any{
		"group_id": groupID,
		"message":  content,
	})

	req, _ := http.NewRequest("POST", config.NapCatHTTPAPI+"/send_group_msg", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	if config.NapCatAccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+config.NapCatAccessToken)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Error("send_group_msg 请求失败", "err", err, "group", groupID)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		slog.Error("send_group_msg 返回非200", "status", resp.StatusCode, "body", string(respBody), "group", groupID)
		return
	}
	slog.Info("send_group_msg 成功", "group", groupID, "resp", string(respBody))
}

// FetchGroupMsgHistory 拉取群消息历史
func FetchGroupMsgHistory(groupID string, count int) ([]HistoryMsg, error) {
	slog.Debug("调用 NapCat API", "action", "get_group_msg_history", "group", groupID, "count", count)
	body, _ := json.Marshal(map[string]any{
		"group_id": groupID,
		"count":    count,
	})

	req, _ := http.NewRequest("POST", config.NapCatHTTPAPI+"/get_group_msg_history", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	if config.NapCatAccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+config.NapCatAccessToken)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Error("get_group_msg_history 请求失败", "err", err, "group", groupID)
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		slog.Error("get_group_msg_history 返回非200", "status", resp.StatusCode, "body", string(respBody), "group", groupID)
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var result struct {
		Status  string `json:"status"`
		Retcode int    `json:"retcode"`
		Data    struct {
			Messages []HistoryMsg `json:"messages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		slog.Error("get_group_msg_history 解析失败", "err", err, "body", string(respBody))
		return nil, fmt.Errorf("解析消息历史失败: %w", err)
	}
	slog.Debug("get_group_msg_history 成功", "group", groupID, "条数", len(result.Data.Messages))
	return result.Data.Messages, nil
}
