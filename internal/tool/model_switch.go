package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/provider/llm"
)

// ModelSwitchTool 异构心智模型热切换与思考强度配置工具 (实现 core.Tool，基于运行时动态发现的模型池)
type ModelSwitchTool struct {
	router *llm.MindRouter
	immune *entity.ImmuneSystem
}

// NewModelSwitchTool 实例化模型热切换双手工具
func NewModelSwitchTool(router *llm.MindRouter, immune *entity.ImmuneSystem) *ModelSwitchTool {
	return &ModelSwitchTool{
		router: router,
		immune: immune,
	}
}

func (t *ModelSwitchTool) Name() string {
	return "switch_model"
}

func (t *ModelSwitchTool) Description() string {
	return "查看、列出或切换心智模型与思考强度 (effort)，查询各通道模型池与用量配额状态 (usage)。当用户询问当前模型、有哪些可用模型、AGY 状态/模型/用量/配额 (usage)、API 通道模型或请求切换模型时，必须调用本工具获取最新权威数据，严禁凭空臆造。支持 action: 'list' (列出所有或指定通道可用模型与配额说明)、'current' (查看当前激活模型)、'switch' (热切换模型或调整思考强度)、'usage' (查询模型通道状态与用量计费说明)。"
}

func (t *ModelSwitchTool) Parameters() map[string]any {
	channels := t.router.Channels()
	channelDesc := "可选指定通道类型，如 'AGY' 或 'API'"
	if len(channels) > 0 {
		channelDesc = fmt.Sprintf("可选指定通道类型，如 'AGY' 或 'API'，当前已挂载通道: [%s]", strings.Join(channels, ", "))
	}
	channelProp := map[string]any{
		"type":        "string",
		"description": channelDesc,
	}
	if len(channels) > 0 {
		channelProp["enum"] = channels
	}

	models := t.router.ListModels()
	var modelSummaries []string
	for _, m := range models {
		if len(m.SupportedEfforts) > 0 {
			modelSummaries = append(modelSummaries, fmt.Sprintf("%s (%s, 思考强度: %s)", m.ID, m.Channel, strings.Join(m.SupportedEfforts, "/")))
		} else {
			modelSummaries = append(modelSummaries, fmt.Sprintf("%s (%s)", m.ID, m.Channel))
		}
	}

	modelDesc := "目标模型 ID 或名称关键字"
	if len(modelSummaries) > 0 {
		modelDesc = "目标模型 ID 或名称关键字。当前实时探测到的可用模型池: " + strings.Join(modelSummaries, "；")
	}

	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"current", "list", "switch", "usage"},
				"description": "执行动作：'list' 列出所有通道或指定通道可用模型与配额状态，'current' 查看当前激活模型与思考强度，'switch' 切换模型或调整思考强度，'usage' 查询各通道模型用量与计费配额说明",
			},
			"model": map[string]any{
				"type":        "string",
				"description": modelDesc,
			},
			"channel": channelProp,
			"effort": map[string]any{
				"type":        "string",
				"enum":        []string{"low", "medium", "high", "xhigh", "max"},
				"description": "可选指定的思考强度（推理深度），适用于支持推理深度调节的模型",
			},
		},
	}
}

type switchArgs struct {
	Action  string `json:"action"`
	Model   string `json:"model"`
	Channel string `json:"channel"`
	Effort  string `json:"effort"`
}

func (t *ModelSwitchTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	var args switchArgs
	if len(rawArgs) > 0 {
		_ = json.Unmarshal(rawArgs, &args)
	}

	action := strings.ToLower(strings.TrimSpace(args.Action))
	model := strings.TrimSpace(args.Model)
	effort := strings.TrimSpace(args.Effort)
	channel := strings.TrimSpace(args.Channel)

	if action == "" {
		if model != "" || effort != "" {
			action = "switch"
		} else if channel != "" {
			action = "list"
		} else {
			action = "current"
		}
	}

	switch action {
	case "current":
		return t.handleCurrent(), nil
	case "usage":
		return t.handleUsage(ctx, channel), nil
	case "list":
		return t.handleList(channel), nil
	case "switch":
		if ev := entity.SensoryEventFromContext(ctx); ev != nil && t.immune != nil {
			if err := t.immune.VerifyDelegationPerm(ev.Role); err != nil {
				return &core.ToolResult{Text: err.Error()}, nil
			}
		}
		return t.handleSwitch(model, channel, effort), nil
	default:
		return &core.ToolResult{
			Text: fmt.Sprintf("未知操作 %q，支持的操作为 current, list, switch, usage", action),
		}, nil
	}
}

func (t *ModelSwitchTool) handleUsage(ctx context.Context, channelFilter string) *core.ToolResult {
	filter := strings.ToUpper(strings.TrimSpace(channelFilter))

	var sb strings.Builder
	listRes := t.handleList(filter)
	sb.WriteString(listRes.Text)

	targetChan := filter
	if targetChan == "" {
		curr := t.router.CurrentMind()
		if strings.EqualFold(curr.Channel, "AGY") {
			targetChan = "AGY"
		}
	}

	if targetChan != "" {
		if usageText, err := t.router.QueryChannelUsage(ctx, targetChan); err == nil && usageText != "" {
			sb.WriteString("\n\n---\n\n")
			sb.WriteString(usageText)
		}
	} else {
		if usageText, err := t.router.QueryChannelUsage(ctx, "AGY"); err == nil && usageText != "" {
			sb.WriteString("\n\n---\n\n")
			sb.WriteString(usageText)
		}
	}

	return &core.ToolResult{
		Text: strings.TrimSpace(sb.String()),
	}
}

func (t *ModelSwitchTool) handleCurrent() *core.ToolResult {
	return &core.ToolResult{
		Text: t.router.FormatCurrentStatus(),
	}
}

func (t *ModelSwitchTool) handleList(channelFilter string) *core.ToolResult {
	models := t.router.ListModels()
	curr := t.router.CurrentMind()

	channelsMap := make(map[string][]llm.ModelInfo)
	var channelOrder []string

	for _, m := range models {
		ch := m.Channel
		if _, exists := channelsMap[ch]; !exists {
			channelOrder = append(channelOrder, ch)
		}
		channelsMap[ch] = append(channelsMap[ch], m)
	}

	filter := strings.ToUpper(strings.TrimSpace(channelFilter))
	if filter != "" {
		if _, exists := channelsMap[filter]; !exists {
			t.router.RefreshModels(context.Background())
			models = t.router.ListModels()
			channelsMap = make(map[string][]llm.ModelInfo)
			channelOrder = nil
			for _, m := range models {
				ch := m.Channel
				if _, exists := channelsMap[ch]; !exists {
					channelOrder = append(channelOrder, ch)
				}
				channelsMap[ch] = append(channelsMap[ch], m)
			}
		}

		if _, exists := channelsMap[filter]; !exists {
			allChans := t.router.Channels()
			return &core.ToolResult{
				Text: fmt.Sprintf("【未探测到 %s 通道模型】\n当前系统挂载的通道为: [%s]。", filter, strings.Join(allChans, ", ")),
			}
		}
		channelOrder = []string{filter}
	}

	var sb strings.Builder
	if filter != "" {
		sb.WriteString(fmt.Sprintf("【实时探测可用心智模型池 · %s 通道】\n\n", filter))
	} else {
		sb.WriteString("【实时探测可用心智模型池】\n\n")
	}

	for _, ch := range channelOrder {
		chModels := channelsMap[ch]
		sb.WriteString(fmt.Sprintf("### %s 通道\n", ch))
		switch ch {
		case "AGY":
			sb.WriteString("- 通道属性：Google Antigravity 自驱智能体沙箱（走本地官方授权配额，0 额外 Token 计费开销）\n")
		case "API":
			sb.WriteString("- 通道属性：OpenAI 兼容协议高可用端点（按 Token 计费）\n")
		}
		for _, m := range chModels {
			activeTag := ""
			if strings.EqualFold(curr.Channel, ch) && curr.Model == m.ID {
				effortDesc := ""
				if curr.Effort != "" {
					effortDesc = fmt.Sprintf(", 思考强度: %s", curr.Effort)
				}
				activeTag = fmt.Sprintf(" *(当前激活%s)*", effortDesc)
			}
			efforts := ""
			if len(m.SupportedEfforts) > 0 {
				efforts = fmt.Sprintf(" [思考强度: %s]", strings.Join(m.SupportedEfforts, ", "))
			}
			desc := ""
			if m.Description != "" {
				desc = "：" + m.Description
			}
			if m.Name != "" && !strings.EqualFold(m.Name, m.ID) {
				sb.WriteString(fmt.Sprintf("- **%s** (`%s`)%s%s%s\n", m.Name, m.ID, efforts, activeTag, desc))
			} else {
				sb.WriteString(fmt.Sprintf("- `%s`%s%s%s\n", m.ID, efforts, activeTag, desc))
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString("说明：可通过 switch_model(model=\"...\", effort=\"...\") 无缝热切换心智或调整思考强度。")

	return &core.ToolResult{
		Text: strings.TrimSpace(sb.String()),
	}
}

func (t *ModelSwitchTool) handleSwitch(model, channel, effort string) *core.ToolResult {
	res, err := t.router.SwitchMind(llm.SwitchRequest{
		Target:  model,
		Channel: channel,
		Effort:  effort,
	})
	if err != nil {
		return &core.ToolResult{
			Text: fmt.Sprintf("切换模型出现异常: %v", err),
		}
	}

	if res.Status == "ambiguous" {
		var sb strings.Builder
		sb.WriteString("【模型匹配存在二义性】\n")
		sb.WriteString(res.Message + "\n\n")
		sb.WriteString("匹配到的候选心智列表：\n")
		for _, c := range res.Candidates {
			effortInfo := ""
			if len(c.SupportedEfforts) > 0 {
				effortInfo = fmt.Sprintf(" [思考强度: %s]", strings.Join(c.SupportedEfforts, ", "))
			}
			sb.WriteString(fmt.Sprintf("- [%s 通道] **%s** (`%s`)%s\n", c.Channel, c.Name, c.ID, effortInfo))
		}
		return &core.ToolResult{
			Text: strings.TrimSpace(sb.String()),
		}
	}

	if res.Status == "error" {
		return &core.ToolResult{
			Text: res.Message,
		}
	}

	return &core.ToolResult{
		Text: "【心智模型切换成功】\n" + res.Message,
	}
}
