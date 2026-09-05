package cmd

import (
	"context"
	"fmt"
	"good-review-master/logutil"
	"regexp"
	"strings"

	"good-review-master/onebot"
)

var addCmdRe = regexp.MustCompile(`添加关键字\((.+?)\)指令\((\w+)\)大模型想提示词\((.+)\)`)
var delCmdRe = regexp.MustCompile(`删除关键字\((.+)\)`)
var addRuleRe = regexp.MustCompile(`添加指令规则\((.+?)\)规则要点\((.+)\)`)
var delRuleRe = regexp.MustCompile(`删除指令规则\((.+)\)`)

const ruleGenSystem = `你是一个提示词工程师。根据用户描述的规则要点，生成简洁、清晰的规则文本。每行一条，直接输出规则，不要多余解释。`

const (
	fmtAddCmd  = "添加关键字(关键词)指令(指令类型)大模型想提示词(要点)"
	helpAddCmd = "格式：" + fmtAddCmd + "\n可用指令类型: chat_review"

	fmtDelCmd  = "删除关键字(关键词)"
	helpDelCmd = "格式：" + fmtDelCmd

	fmtAddRule  = "添加指令规则(指令类型)规则要点(要点)"
	helpAddRule = "格式：" + fmtAddRule + "\n可用指令类型: chat_review\n动态修改 prompt 中特定类型指令的共享规则，如：禁止骂人"

	fmtDelRule  = "删除指令规则(类型)"
	helpDelRule = "格式：" + fmtDelRule + "\n删除 prompt 中特定类型指令的共享规则"

	fmtSwitchPersona  = "#切换人格 (人格名)"
	helpSwitchPersona = "格式：" + fmtSwitchPersona + "\n切换本群普通@聊天的回复人格（取自带人格的关键字）。\n发 #切换人格 可查看可选人格。\n仅对未命中关键字的@聊天生效，功能指令不受影响。"

	fmtCancelPersona  = "#取消人格"
	helpCancelPersona = "格式：" + fmtCancelPersona + "\n取消本群人格，恢复普通聊天。"

	helpHelp = "查看可用指令"
)

// registerInternalCommands 向路由器注册所有内置管理指令
func (r *Router) registerInternalCommands() {
	r.register(Command{Keyword: "#添加关键字", Help: helpAddCmd, Category: "internal", Handler: r.handleAddCommand})
	r.register(Command{Keyword: "#删除关键字", Help: helpDelCmd, Category: "internal", Handler: r.handleDeleteCommand})
	r.register(Command{Keyword: "#添加指令规则", Help: helpAddRule, Category: "internal", Handler: r.handleAddRule})
	r.register(Command{Keyword: "#删除指令规则", Help: helpDelRule, Category: "internal", Handler: r.handleDeleteRule})
	r.register(Command{Keyword: "#切换人格", Help: helpSwitchPersona, Category: "internal", Handler: r.handleSwitchPersona})
	r.register(Command{Keyword: "#取消人格", Help: helpCancelPersona, Category: "internal", Handler: r.handleCancelPersona})
	r.register(Command{Keyword: "#帮助", Help: helpHelp, Category: "internal", Handler: r.handleListCommands})
}

func (r *Router) handleAddCommand(event onebot.Event, groupID string, systemPrompt string, keywordPrompt string, mentionerNick string, extra string) {
	content := event.RawMessage
	matches := addCmdRe.FindStringSubmatch(content)
	if len(matches) != 4 {
		r.obClient.SendGroupMessage(groupID, "❌ 格式错误\n正确格式："+fmtAddCmd)
		return
	}
	keyword := matches[1]
	category := matches[2]
	requirements := matches[3]

	if r.promptCfg.KeywordInSystemCmd(keyword) || r.isInternalKeyword(keyword) {
		r.obClient.SendGroupMessage(groupID, "❌ 该关键字为系统/内部指令，禁止覆盖")
		return
	}

	logutil.Info("使用 LLM 生成提示词+人格", "category", category, "keyword", keyword)
	ctx, cancel := context.WithTimeout(context.Background(), r.appCfg.LLMTimeout)
	defer cancel()
	generated, err := r.llmClient.SingleChat(ctx, requirements, addCommandGenPrompt)
	if err != nil {
		logutil.Error("LLM 生成提示词失败", "err", err)
		r.obClient.SendGroupMessage(groupID, "❌ 生成提示词失败: "+err.Error())
		return
	}
	finalPrompt, persona, err := parseAddCommandOutput(generated)
	if err != nil {
		logutil.Error("解析 LLM 输出失败（persona 缺失或不完整）", "err", err)
		r.obClient.SendGroupMessage(groupID, "❌ 生成失败，请重试: "+err.Error())
		return
	}

	if err := r.promptCfg.AddCommand(category, keyword, finalPrompt, persona); err != nil {
		logutil.Error("添加指令失败", "err", err)
		r.obClient.SendGroupMessage(groupID, "❌ 添加指令失败: "+err.Error())
		return
	}
	r.promptCfg.Reload()
	r.rebuild()
	r.obClient.SendGroupMessage(groupID, "✅ 指令已添加: "+keyword)
}

func (r *Router) handleDeleteCommand(event onebot.Event, groupID string, systemPrompt string, keywordPrompt string, mentionerNick string, extra string) {
	content := event.RawMessage
	matches := delCmdRe.FindStringSubmatch(content)
	if len(matches) != 2 {
		r.obClient.SendGroupMessage(groupID, "❌ 格式错误\n正确格式："+fmtDelCmd)
		return
	}
	keyword := matches[1]

	if r.promptCfg.KeywordInSystemCmd(keyword) || r.isInternalKeyword(keyword) {
		r.obClient.SendGroupMessage(groupID, "❌ 该关键字为系统/内部指令，禁止删除")
		return
	}

	if err := r.promptCfg.DeleteCommand(keyword); err != nil {
		r.obClient.SendGroupMessage(groupID, "❌ 删除失败: "+err.Error())
		return
	}
	r.promptCfg.Reload()
	r.rebuild()
	r.obClient.SendGroupMessage(groupID, "✅ 关键字已删除: "+keyword)
}

func (r *Router) handleAddRule(event onebot.Event, groupID string, systemPrompt string, keywordPrompt string, mentionerNick string, extra string) {
	content := event.RawMessage
	matches := addRuleRe.FindStringSubmatch(content)
	if len(matches) != 3 {
		r.obClient.SendGroupMessage(groupID, "❌ 格式错误\n正确格式："+fmtAddRule)
		return
	}
	category := matches[1]
	requirements := matches[2]

	if r.promptCfg.CategoryInSystemRule(category) {
		r.obClient.SendGroupMessage(groupID, "❌ 该类型的规则在 prompt_system.yaml 中已存在，禁止覆盖")
		return
	}

	logutil.Info("使用 LLM 生成规则", "category", category)
	ctx, cancel := context.WithTimeout(context.Background(), r.appCfg.LLMTimeout)
	defer cancel()
	generated, err := r.llmClient.SingleChat(ctx, requirements, ruleGenSystem)
	if err != nil {
		logutil.Error("LLM 生成规则失败", "err", err)
		r.obClient.SendGroupMessage(groupID, "❌ 生成规则失败: "+err.Error())
		return
	}
	ruleText := strings.TrimSpace(generated)

	if err := r.promptCfg.AddRule(category, ruleText); err != nil {
		logutil.Error("添加规则失败", "err", err)
		r.obClient.SendGroupMessage(groupID, "❌ 添加规则失败: "+err.Error())
		return
	}
	r.promptCfg.Reload()
	r.obClient.SendGroupMessage(groupID, "✅ 规则已添加: "+category)
}

func (r *Router) handleDeleteRule(event onebot.Event, groupID string, systemPrompt string, keywordPrompt string, mentionerNick string, extra string) {
	content := event.RawMessage
	matches := delRuleRe.FindStringSubmatch(content)
	if len(matches) != 2 {
		r.obClient.SendGroupMessage(groupID, "❌ 格式错误\n正确格式："+fmtDelRule)
		return
	}
	category := matches[1]

	if r.promptCfg.CategoryInSystemRule(category) {
		r.obClient.SendGroupMessage(groupID, "❌ 该类型的规则在 prompt_system.yaml 中，禁止删除")
		return
	}

	if err := r.promptCfg.DeleteRule(category); err != nil {
		r.obClient.SendGroupMessage(groupID, "❌ 删除失败: "+err.Error())
		return
	}
	r.promptCfg.Reload()
	r.obClient.SendGroupMessage(groupID, "✅ 规则已删除: "+category)
}

func (r *Router) handleListCommands(event onebot.Event, groupID string, systemPrompt string, keywordPrompt string, mentionerNick string, extra string) {
	var buf strings.Builder
	buf.WriteString("【指令帮助】\n\n")
	buf.WriteString("使用方式：@机器人 + 关键词\n\n")
	buf.WriteString("▎管理指令：\n")
	for _, cmd := range r.routes {
		if cmd.Category != "internal" || cmd.Help == "" {
			continue
		}
		buf.WriteString("→ " + cmd.Keyword + "\n")
		helpLines := strings.Split(cmd.Help, "\n")
		for i, line := range helpLines {
			buf.WriteString(fmt.Sprintf("    %d.%s\n", i+1, line))
		}
	}
	buf.WriteString("\n▎功能指令：\n")
	for _, cmd := range r.routes {
		if cmd.Category == "internal" || cmd.Keyword == "" {
			continue
		}
		buf.WriteString("  " + cmd.Keyword + " [" + cmd.Category + "]\n")
	}
	r.obClient.SendGroupMessage(groupID, buf.String())
}

// handleSwitchPersona #切换人格 <人格名>：在路由表里找"自带人格"的关键字，
// 找到就把该人格值拷贝记为本群当前人格（#取消人格 清除，重启清空）。
// 仅对未命中关键字的纯 @ 聊天生效，命中关键字的指令仍用自带人格。
func (r *Router) handleSwitchPersona(event onebot.Event, groupID string, systemPrompt string, keywordPrompt string, mentionerNick string, extra string) {
	name := strings.TrimSpace(extra)

	// 不带参数：给出用法并列出可选人格
	if name == "" {
		var buf strings.Builder
		buf.WriteString("用法：" + fmtSwitchPersona + "\n")
		if names := r.availablePersonaNames(); len(names) > 0 {
			buf.WriteString("可选人格：\n")
			for _, n := range names {
				buf.WriteString("  " + n + "\n")
			}
		} else {
			buf.WriteString("当前无可切换人格，可用 #添加关键字 创建自带人格的指令")
		}
		r.obClient.SendGroupMessage(groupID, strings.TrimSpace(buf.String()))
		return
	}

	for _, route := range r.routes {
		if route.Keyword != name {
			continue
		}
		if route.Persona == nil {
			r.obClient.SendGroupMessage(groupID, "❌ 关键字「"+name+"」没有可绑定的人格")
			return
		}
		r.setGroupPersona(groupID, &groupPersonaBinding{
			keyword:     name,
			persona:     *route.Persona,
			sharedRules: route.SharedRules,
		})
		r.obClient.SendGroupMessage(groupID, "✅ 已切换人格："+name+"\n仅对普通@聊天生效，功能指令不受影响。")
		return
	}

	msg := "❌ 未找到人格：" + name
	if names := r.availablePersonaNames(); len(names) > 0 {
		msg += "\n可选人格：" + strings.Join(names, "、")
	} else {
		msg += "\n当前无可切换人格。"
	}
	r.obClient.SendGroupMessage(groupID, msg)
}

// handleCancelPersona #取消人格：清空本群当前人格，纯 @ 聊天恢复无人格。
func (r *Router) handleCancelPersona(event onebot.Event, groupID string, systemPrompt string, keywordPrompt string, mentionerNick string, extra string) {
	if _, ok := r.getGroupPersona(groupID); !ok {
		r.obClient.SendGroupMessage(groupID, "ℹ️ 本群未设置人格")
		return
	}
	r.clearGroupPersona(groupID)
	r.obClient.SendGroupMessage(groupID, "✅ 已取消人格，恢复正常聊天")
}
