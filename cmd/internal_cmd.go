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
var addRuleRe = regexp.MustCompile(`添加指令规则\((.+?)\)规则要点\((.+)\)`)
var delRuleRe = regexp.MustCompile(`删除指令规则\((.+)\)`)

const promptGenSystem = `你是一个提示词工程师。根据用户要求，生成一个简洁、有效的系统提示词。直接输出提示词，不要多余解释。`
const ruleGenSystem = `你是一个提示词工程师。根据用户描述的规则要点，生成简洁、清晰的规则文本。每行一条，直接输出规则，不要多余解释。`

const (
	fmtAddCmd  = "添加关键字(关键词)指令(指令类型)大模型想提示词(要点)"
	fmtDelCmd  = "删除关键字(关键词)"
	fmtAddRule = "添加指令规则(指令类型)规则要点(要点)"
	fmtDelRule = "删除指令规则(类型)"
)

func init() {
	Register(Command{Keyword: "添加关键字", Help: "格式：" + fmtAddCmd, Category: "internal", Handler: addCommand})
	Register(Command{Keyword: "删除关键字", Help: "格式：" + fmtDelCmd, Category: "internal", Handler: deleteCommand})
	Register(Command{Keyword: "添加指令规则", Help: "格式：" + fmtAddRule + "\n动态修改 prompt 中特定类型指令的共享规则", Category: "internal", Handler: addRule})
	Register(Command{Keyword: "删除指令规则", Help: "格式：" + fmtDelRule + "\n删除 prompt 中特定类型指令的共享规则", Category: "internal", Handler: deleteRule})
	Register(Command{Keyword: "帮助", Help: "查看可用指令", Category: "internal", Handler: listCommands})
}

func addCommand(event onebot.Event, groupID string, _ string) {
	content := event.RawMessage
	matches := addCmdRe.FindStringSubmatch(content)
	if len(matches) != 4 {
		onebot.SendGroupMessage(groupID, "❌ 格式错误\n正确格式："+fmtAddCmd)
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
		onebot.SendGroupMessage(groupID, "❌ 格式错误\n正确格式："+fmtDelCmd)
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

func addRule(event onebot.Event, groupID string, _ string) {
	content := event.RawMessage
	matches := addRuleRe.FindStringSubmatch(content)
	if len(matches) != 3 {
		onebot.SendGroupMessage(groupID, "❌ 格式错误\n正确格式："+fmtAddRule)
		return
	}
	category := matches[1]
	requirements := matches[2]

	if config.RuleInMainPrompt(category) {
		onebot.SendGroupMessage(groupID, "❌ 该类型的规则在 prompt_system.yaml 中已存在，禁止覆盖")
		return
	}

	slog.Info("使用 LLM 生成规则", "category", category)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.LLMTimeout))
	defer cancel()
	generated, err := llm.DefaultClient.Review(ctx, requirements, ruleGenSystem)
	if err != nil {
		slog.Error("LLM 生成规则失败", "err", err)
		onebot.SendGroupMessage(groupID, "❌ 生成规则失败: "+err.Error())
		return
	}
	ruleText := strings.TrimSpace(generated)

	if err := config.AddPromptRule(category, ruleText); err != nil {
		slog.Error("添加规则失败", "err", err)
		onebot.SendGroupMessage(groupID, "❌ 添加规则失败: "+err.Error())
		return
	}
	config.ReloadPrompts()
	onebot.SendGroupMessage(groupID, "✅ 规则已添加: "+category)
}

func deleteRule(event onebot.Event, groupID string, _ string) {
	content := event.RawMessage
	matches := delRuleRe.FindStringSubmatch(content)
	if len(matches) != 2 {
		onebot.SendGroupMessage(groupID, "❌ 格式错误\n正确格式："+fmtDelRule)
		return
	}
	category := matches[1]

	if config.RuleInMainPrompt(category) {
		onebot.SendGroupMessage(groupID, "❌ 该类型的规则在 prompt_system.yaml 中，禁止删除")
		return
	}

	if err := config.DeletePromptRule(category); err != nil {
		onebot.SendGroupMessage(groupID, "❌ 删除失败: "+err.Error())
		return
	}
	config.ReloadPrompts()
	onebot.SendGroupMessage(groupID, "✅ 规则已删除: "+category)
}

func listCommands(event onebot.Event, groupID string, _ string) {
	var buf strings.Builder
	buf.WriteString("【指令帮助】\n\n")
	buf.WriteString("使用方式：@机器人 + 关键词\n\n")
	buf.WriteString("▎管理指令：\n")
	for _, cmd := range registry {
		if cmd.Help == "" {
			continue
		}
		buf.WriteString("  " + cmd.Keyword + "\n")
		for _, line := range strings.Split(cmd.Help, "\n") {
			buf.WriteString("    → " + line + "\n")
		}
		if cmd.Keyword == "添加关键字" || cmd.Keyword == "添加指令规则" {
			var types []string
			for typ := range handlerMap {
				types = append(types, typ)
			}
			buf.WriteString("    → 指令类型: " + strings.Join(types, "/") + "\n")
		}
	}
	buf.WriteString("\n▎功能指令：\n")
	for _, route := range Routes {
		if route.Keyword == "" {
			continue
		}
		isSystem := false
		for _, cmd := range registry {
			if cmd.Keyword == route.Keyword {
				isSystem = true
				break
			}
		}
		if !isSystem {
			buf.WriteString("  " + route.Keyword + " [" + route.Category + "]\n")
		}
	}
	onebot.SendGroupMessage(groupID, buf.String())
}
