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

var addCmdRe = regexp.MustCompile(`添加关键字\((.+?)\)指令\((\w+)\)大模型想提示词\((.+)\)`)
var delCmdRe = regexp.MustCompile(`删除关键字\((.+)\)`)

const promptGenSystem = `你是一个提示词工程师。根据用户要求，生成一个简洁、有效的系统提示词。直接输出提示词，不要多余解释。`

func init() {
	Register(Command{Keyword: "添加关键字", Help: "格式：添加关键字(关键词)指令(类型)大模型想提示词(要点)", Category: "internal", Handler: addCommand})
	Register(Command{Keyword: "删除关键字", Help: "格式：删除关键字(关键词)", Category: "internal", Handler: deleteCommand})
	Register(Command{Keyword: "帮助", Help: "查看可用指令", Category: "internal", Handler: listCommands})
}

func addCommand(event onebot.Event, groupID string, _ string) {
	content := event.RawMessage
	matches := addCmdRe.FindStringSubmatch(content)
	if len(matches) != 4 {
		onebot.SendGroupMessage(groupID, "❌ 格式错误\n正确格式：添加关键字(关键词)指令(类型)大模型想提示词(要点)")
		return
	}
	keyword := matches[1]
	category := matches[2]
	requirements := matches[3]

	if config.KeywordInMainPrompt(category, keyword) || IsInternalKeyword(keyword) {
		onebot.SendGroupMessage(groupID, "❌ 该关键字为系统/内部指令，禁止覆盖")
		return
	}

	slog.Info("使用 LLM 生成提示词", "category", category, "keyword", keyword)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.LLMTimeout))
	defer cancel()
	generated, err := llm.DefaultClient.Review(ctx, requirements, promptGenSystem)
	if err != nil {
		slog.Error("LLM 生成提示词失败", "err", err)
		onebot.SendGroupMessage(groupID, "❌ 生成提示词失败: "+err.Error())
		return
	}
	finalPrompt := strings.TrimSpace(generated)

	if err := config.AddPromptCommand(category, keyword, finalPrompt); err != nil {
		slog.Error("添加指令失败", "err", err)
		onebot.SendGroupMessage(groupID, "❌ 添加指令失败: "+err.Error())
		return
	}
	config.ReloadPrompts()
	RebuildRoutes()
	onebot.SendGroupMessage(groupID, "✅ 指令已添加: "+keyword)
}

func deleteCommand(event onebot.Event, groupID string, _ string) {
	content := event.RawMessage
	matches := delCmdRe.FindStringSubmatch(content)
	if len(matches) != 2 {
		onebot.SendGroupMessage(groupID, "❌ 格式错误\n正确格式：删除关键字(关键词)")
		return
	}
	keyword := matches[1]

	if config.KeywordInMainPromptAny(keyword) || IsInternalKeyword(keyword) {
		onebot.SendGroupMessage(groupID, "❌ 该关键字为系统/内部指令，禁止删除")
		return
	}

	if err := config.DeletePromptCommand(keyword); err != nil {
		onebot.SendGroupMessage(groupID, "❌ 删除失败: "+err.Error())
		return
	}
	config.ReloadPrompts()
	RebuildRoutes()
	onebot.SendGroupMessage(groupID, "✅ 关键字已删除: "+keyword)
}

func listCommands(event onebot.Event, groupID string, _ string) {
	var sb strings.Builder
	sb.WriteString("【指令帮助】\n\n")
	sb.WriteString("使用方式：@机器人 + 关键词\n\n")
	sb.WriteString("▎管理指令：\n")
	for _, cmd := range registry {
		if cmd.Help == "" {
			continue
		}
		sb.WriteString("  " + cmd.Keyword + "\n    → " + cmd.Help + "\n")
	}
	sb.WriteString("\n▎功能指令：\n")
	for _, r := range Routes {
		if r.Keyword == "" {
			continue
		}
		isSystem := false
		for _, cmd := range registry {
			if cmd.Keyword == r.Keyword {
				isSystem = true
				break
			}
		}
		if !isSystem {
			sb.WriteString("  " + r.Keyword + "\n")
		}
	}
	onebot.SendGroupMessage(groupID, sb.String())
}
