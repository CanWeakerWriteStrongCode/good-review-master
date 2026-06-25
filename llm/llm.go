package llm

import (
	"context"
	"fmt"
	"strings"

	"good-review-master/logutil"

	openai "github.com/sashabaranov/go-openai"
)

// Client 大模型统一接口
type Client interface {
	Review(ctx context.Context, chatLog, systemPrompt string) (string, error)
}

// OpenAIAdapter 适配所有OpenAI协议的大模型（基于 go-openai SDK）
type OpenAIAdapter struct {
	client *openai.Client
	model  string
	temp   float32
	topP   float32
}

// NewOpenAIAdapter 创建OpenAI兼容的大模型客户端
func NewOpenAIAdapter(apiKey, apiBase, model string, temp, topP float64) Client {
	cfg := openai.DefaultConfig(apiKey)
	cfg.BaseURL = strings.TrimSuffix(apiBase, "/")
	return &OpenAIAdapter{
		client: openai.NewClientWithConfig(cfg),
		model:  model,
		temp:   float32(temp),
		topP:   float32(topP),
	}
}

// Review 调用大模型
func (adapter *OpenAIAdapter) Review(ctx context.Context, chatLog, systemPrompt string) (string, error) {
	logutil.Info("发送给大模型", "systemPrompt", systemPrompt, "chatLog", chatLog)
	resp, err := adapter.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: adapter.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: "以下是群聊记录：\n" + chatLog + "\n请回复"},
		},
		Temperature: adapter.temp,
		TopP:        adapter.topP,
	})
	if err != nil {
		return "", fmt.Errorf("大模型调用失败: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("大模型返回为空")
	}
	return resp.Choices[0].Message.Content, nil
}
