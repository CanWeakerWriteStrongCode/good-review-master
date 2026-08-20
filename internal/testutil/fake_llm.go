// Package testutil 提供测试模式下替代外部依赖的假实现。
package testutil

import (
	"context"
	"sync"
)

// LLMCall 记录一次 Review 调用（含实际返回的回复，供断言）
type LLMCall struct {
	ChatLog      string `json:"chat_log"`
	SystemPrompt string `json:"system_prompt"`
	Reply        string `json:"reply"`
}

// FakeLLM 实现 llm.Client：返回固定回复并记录每次调用
type FakeLLM struct {
	mu    sync.Mutex
	calls []LLMCall
	reply string
}

// NewFakeLLM 创建返回固定回复的 FakeLLM
func NewFakeLLM() *FakeLLM {
	return &FakeLLM{reply: "【测试回复】锐评完毕"}
}

// SetReply 自定义固定回复内容
func (f *FakeLLM) SetReply(reply string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = reply
}

// Review 实现 llm.Client，记录调用并返回固定回复
func (f *FakeLLM) Review(ctx context.Context, chatLog, systemPrompt string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, LLMCall{
		ChatLog:      chatLog,
		SystemPrompt: systemPrompt,
		Reply:        f.reply,
	})
	return f.reply, nil
}

// Calls 返回已记录调用的副本
func (f *FakeLLM) Calls() []LLMCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]LLMCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// Reset 清空已记录调用
func (f *FakeLLM) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}
