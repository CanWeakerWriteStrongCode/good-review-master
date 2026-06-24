package cmd

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"good-review-master/config"
	"good-review-master/llm"
	"good-review-master/onebot"
)

var addCmdRe = regexp.MustCompile(`添加永久命令\((\w+)\)关键字\((.+?)\)(提示词|大模型想提示词)\((.+)\)`)

const promptGenSystem = "你是一个提示词工程师。根据用户要求，生成一个简洁、有效的系统提示词。直接输出提示词，不要多余解释。"

func addCommand(event onebot.Event, groupID string, _ string) {
	content := event.RawMessage
	matches := addCmdRe.FindStringSubmatch(content)
	if len(matches) != 5 {
		onebot.SendGroupMessage(groupID, "❌ 格式错误\n正确格式：\n添加永久命令(类型)关键字(关键词)提示词(提示词内容)\n添加永久命令(类型)关键字(关键词)大模型想提示词(要点)")
		return
	}
	category := matches[1]
	keyword := matches[2]
	mode := matches[3] // "提示词" 或 "大模型想提示词"
	promptOrReq := matches[4]

	finalPrompt := promptOrReq
	if mode == "大模型想提示词" {
		slog.Info("使用 LLM 生成提示词", "category", category, "keyword", keyword)
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.LLMTimeout))
		defer cancel()
		generated, err := llm.DefaultClient.Review(ctx, promptOrReq, promptGenSystem)
		if err != nil {
			slog.Error("LLM 生成提示词失败", "err", err)
			onebot.SendGroupMessage(groupID, "❌ 生成提示词失败: "+err.Error())
			return
		}
		finalPrompt = strings.TrimSpace(generated)
	}

	if err := config.AddPromptCommand(category, keyword, finalPrompt); err != nil {
		slog.Error("添加命令失败", "err", err)
		onebot.SendGroupMessage(groupID, "❌ 添加命令失败: "+err.Error())
		return
	}
	config.ReloadPrompts()
	RebuildRoutes()
	onebot.SendGroupMessage(groupID, "✅ 命令已添加: "+keyword)
}
