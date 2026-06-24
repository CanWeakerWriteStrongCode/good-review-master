package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Client 大模型统一接口
type Client interface {
	Review(ctx context.Context, chatLog, systemPrompt string) (string, error)
}

// DefaultClient 全局大模型客户端实例
var DefaultClient Client

// OpenAIAdapter 适配所有OpenAI协议的大模型
type OpenAIAdapter struct {
	apiKey      string
	apiBase     string
	modelName   string
	temperature float64
	topP        float64
}

// NewOpenAIAdapter 创建OpenAI兼容的大模型客户端
func NewOpenAIAdapter(apiKey, apiBase, model string, temp, topP float64) Client {
	return &OpenAIAdapter{
		apiKey:      apiKey,
		apiBase:     strings.TrimSuffix(apiBase, "/"),
		modelName:   model,
		temperature: temp,
		topP:        topP,
	}
}

// Review 调用大模型
func (o *OpenAIAdapter) Review(ctx context.Context, chatLog, systemPrompt string) (string, error) {
	url := o.apiBase + "/chat/completions"

	reqBody := map[string]any{
		"model":       o.modelName,
		"temperature": o.temperature,
		"top_p":       o.topP,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": "以下是群聊记录：\n" + chatLog + "\n请开始你的锐评"},
		},
	}

	jsonData, _ := json.Marshal(reqBody)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
	req.Header.Set("Authorization", "Bearer "+o.apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	choices, ok := result["choices"].([]any)
	if !ok || len(choices) == 0 {
		return "", fmt.Errorf("大模型返回为空")
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	return msg["content"].(string), nil
}
