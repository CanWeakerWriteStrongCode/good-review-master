// Package testutil 提供测试模式下替代外部依赖的假实现。
package testutil

import (
	"context"
	"sync"

	"good-review-master/llm"
)

// LLMCall 记录一次 Review 调用（含实际返回的回复，供断言）
type LLMCall struct {
	ChatLog      string `json:"chat_log"`
	SystemPrompt string `json:"system_prompt"`
	Reply        string `json:"reply"`
}

// ChatCall 记录一次 Chat 调用（完整消息快照、下发的工具名、本轮返回）
type ChatCall struct {
	MessageCount int               `json:"message_count"`
	LastRole     string            `json:"last_role"`
	ToolNames    []string          `json:"tool_names"`
	Messages     []llm.Message     `json:"messages"`
	Response     *llm.ChatResponse `json:"response"`
}

// FakeLLM 实现 llm.Client：返回固定回复并记录每次调用
type FakeLLM struct {
	mu        sync.Mutex
	calls     []LLMCall
	chatCalls []ChatCall
	script    []*llm.ChatResponse // Chat 的依次返回值，用完后回落固定 reply
	reply     string
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

// SetChatScript 预设 Chat 的依次返回值（用于测试工具调用循环）。
// 列表耗尽后回落到 SetReply 设的固定回复。
func (f *FakeLLM) SetChatScript(responses ...*llm.ChatResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = responses
}

// Chat 实现 llm.Client：记录消息与工具清单，优先返回脚本预设值
func (f *FakeLLM) Chat(_ context.Context, messages []llm.Message, tools []llm.Tool) (*llm.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var resp *llm.ChatResponse
	if len(f.script) > 0 {
		resp = f.script[0]
		f.script = f.script[1:]
	} else {
		resp = &llm.ChatResponse{Content: f.reply}
	}

	call := ChatCall{MessageCount: len(messages), Response: resp}
	if len(messages) > 0 {
		call.LastRole = messages[len(messages)-1].Role
	}
	// 深拷消息切片：调用方（工具循环）会在同一个 slice 上继续 append，
	// 不拷会让早先记录的快照被后续轮次污染
	call.Messages = make([]llm.Message, len(messages))
	copy(call.Messages, messages)
	for _, t := range tools {
		call.ToolNames = append(call.ToolNames, t.Name)
	}
	f.chatCalls = append(f.chatCalls, call)
	return resp, nil
}

// ChatCalls 返回已记录 Chat 调用的副本
func (f *FakeLLM) ChatCalls() []ChatCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ChatCall, len(f.chatCalls))
	copy(out, f.chatCalls)
	return out
}

// Reset 清空已记录调用
func (f *FakeLLM) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
	f.chatCalls = nil
	f.script = nil
}
