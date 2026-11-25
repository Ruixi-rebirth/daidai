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
	return "查询当前模型、可用模型和通道用量，或切换模型与思考强度；状态问题须读取实时数据。"
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
				"description": "目标模型 ID 或名称关键字；如果用户提到的模型不明确，先用 list 查询当前可用模型及其 ID，再按用户意图切换",
			},
			"channel": channelProp,
			"effort": map[string]any{
				"type":        "string",
				"enum":        []string{"low", "medium", "high", "xhigh", "max"},
				"description": "可选指定的思考强度（推理深度），适用于支持推理深度调节的模型",
			},
			"scope": map[string]any{
				"type":        "string",
				"enum":        []string{"partner", "public", "both"},
				"description": "切换作用轨道：'partner' 仅切换搭档私聊模型，'public' 仅切换公用模型（思考强度上限 medium），'both' 双轨同步切换。留空时按当前会话自动决定（私聊默认 'partner'，群聊默认 'public'）",
			},
		},
	}
}

type switchArgs struct {
	Action  string `json:"action"`
	Model   string `json:"model"`
	Channel string `json:"channel"`
	Effort  string `json:"effort"`
	Scope   string `json:"scope"`
}

func (t *ModelSwitchTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	var args switchArgs
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, fmt.Errorf("invalid switch_model args: %w", err)
		}
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
		return redactModelStatus(ctx, t.handleCurrent()), nil
	case "usage":
		return redactModelStatus(ctx, t.handleUsage(ctx, channel)), nil
	case "list":
		return redactModelStatus(ctx, t.handleList(ctx, channel)), nil
	case "switch":
		ev := entity.SensoryEventFromContext(ctx)
		if ev != nil {
			ev.SkipFactExtraction = true
		}
		if ev == nil || t.immune == nil {
			return &core.ToolResult{Text: "权限校验不可用，拒绝切换模型"}, nil
		}
		if err := t.immune.VerifyDelegationPerm(ev.Role); err != nil {
			return &core.ToolResult{Text: err.Error()}, nil
		}
		scope := strings.ToLower(strings.TrimSpace(args.Scope))
		if scope == "" {
			if ev.IsGroup {
				scope = llm.MindScopePublic
			} else {
				scope = llm.MindScopePartner
			}
		}
		return t.handleSwitch(ctx, model, channel, effort, scope), nil
	default:
		return &core.ToolResult{
			Text: fmt.Sprintf("未知操作 %q，支持的操作为 current, list, switch, usage", action),
		}, nil
	}
}

func (t *ModelSwitchTool) handleUsage(ctx context.Context, channelFilter string) *core.ToolResult {
	return t.handleList(ctx, channelFilter)
}

func redactModelStatus(ctx context.Context, result *core.ToolResult) *core.ToolResult {
	if result == nil {
		return nil
	}
	if event := entity.SensoryEventFromContext(ctx); event == nil || !event.IsPartnerPrivate() {
		result.Text = core.RedactSensitiveIPs(result.Text)
	}
	return result
}

func (t *ModelSwitchTool) handleCurrent() *core.ToolResult {
	status := strings.TrimSpace(t.router.FormatCurrentStatus())
	if status == "" {
		status = "当前没有激活的模型。请检查 API 或 AGY 通道配置，以及模型发现和认证日志。"
	}
	return &core.ToolResult{
		Text: status,
	}
}

func (t *ModelSwitchTool) handleList(ctx context.Context, channelFilter string) *core.ToolResult {
	// A user-requested list is an explicit request for the latest available
	// models. Tool definition generation uses the cached pool and does not
	// trigger network or CLI discovery on every conversation turn.
	t.router.RefreshModels(ctx)
	models := t.router.CachedModels()
	curr := t.router.CurrentMind()
	pub := t.router.PublicMind()

	channelsMap := make(map[string][]llm.ModelInfo)
	channelOrder := t.router.Channels()
	for _, ch := range channelOrder {
		channelsMap[ch] = nil
	}

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
			allChans := t.router.Channels()
			return &core.ToolResult{
				Text: fmt.Sprintf("【未探测到 %s 通道模型】\n当前系统挂载的通道为: [%s]。", filter, strings.Join(allChans, ", ")),
			}
		}
		channelOrder = []string{filter}
	}

	var sb strings.Builder
	if filter != "" {
		fmt.Fprintf(&sb, "【当前心智模型池 · %s 通道】\n\n", filter)
	} else {
		sb.WriteString("【当前心智模型池】\n\n")
	}

	for _, ch := range channelOrder {
		chModels := channelsMap[ch]
		configuredModel, modelUsage := t.router.ChannelModelUsage(ch)
		fmt.Fprintf(&sb, "### %s 通道\n", ch)
		if configuredModel != "" {
			fmt.Fprintf(&sb, "%s：`%s`\n", modelUsage, configuredModel)
		}
		if status, ok := t.router.ModelDiscoveryStatus(ch); ok && status.Failed {
			if status.LastSuccess.IsZero() {
				sb.WriteString("- 模型发现最近失败；以下模型来自已配置或缓存信息，当前是否可用尚未确认。\n")
			} else {
				fmt.Fprintf(&sb, "- 模型发现最近失败（%s）；以下清单保留自最近一次成功发现，当前是否可用尚未确认。\n",
					status.LastAttempt.Local().Format("2006-01-02 15:04:05"))
			}
		}

		if usageText, err := t.router.QueryChannelUsage(ctx, ch); err == nil && usageText != "" {
			sb.WriteString("\n" + usageText + "\n\n")
		}

		sb.WriteString("可用模型列表：\n")
		if len(chModels) == 0 {
			sb.WriteString("- 当前没有可列出的模型。请检查该通道的配置、认证和模型发现日志。\n\n")
			continue
		}
		for _, m := range chModels {
			isPartnerActive := strings.EqualFold(curr.Channel, ch) && curr.Model == m.ID
			isPublicActive := strings.EqualFold(pub.Channel, ch) && pub.Model == m.ID
			activeTag := ""
			switch {
			case isPartnerActive && isPublicActive:
				effortDesc := ""
				if curr.Effort != "" {
					effortDesc = fmt.Sprintf(", 私聊强度: %s / 公用强度: %s", curr.Effort, pub.Effort)
				}
				activeTag = fmt.Sprintf(" *(首选聊天 · 双轨共用%s)*", effortDesc)
			case isPartnerActive:
				effortDesc := ""
				if curr.Effort != "" {
					effortDesc = fmt.Sprintf(", 思考强度: %s", curr.Effort)
				}
				activeTag = fmt.Sprintf(" *(首选聊天 · 搭档私聊%s)*", effortDesc)
			case isPublicActive:
				effortDesc := ""
				if pub.Effort != "" {
					effortDesc = fmt.Sprintf(", 思考强度: %s", pub.Effort)
				}
				activeTag = fmt.Sprintf(" *(公用模型%s)*", effortDesc)
			case m.ID == configuredModel:
				activeTag = " *(" + modelUsage + ")*"
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
				fmt.Fprintf(&sb, "- **%s** (`%s`)%s%s%s\n", m.Name, m.ID, efforts, activeTag, desc)
			} else {
				fmt.Fprintf(&sb, "- `%s`%s%s%s\n", m.ID, efforts, activeTag, desc)
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString("想切换模型，直接说“切换到 <模型名称>”（私聊默认切搭档私聊模型，群聊默认切公用模型）；也可明确说“把公用模型切换到 <模型名称>”或“全局切换到 <模型名称>”。")

	return &core.ToolResult{
		Text: strings.TrimSpace(sb.String()),
	}
}

func (t *ModelSwitchTool) handleSwitch(ctx context.Context, model, channel, effort, scope string) *core.ToolResult {
	if ctx == nil {
		ctx = context.Background()
	}
	t.router.RefreshModels(ctx)
	if err := ctx.Err(); err != nil {
		return &core.ToolResult{Text: "模型池刷新已取消，未执行切换。"}
	}

	res, err := t.router.SwitchMind(llm.SwitchRequest{
		Target:  model,
		Channel: channel,
		Effort:  effort,
		Scope:   scope,
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
			fmt.Fprintf(&sb, "- [%s 通道] **%s** (`%s`)%s\n", c.Channel, c.Name, c.ID, effortInfo)
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

	message := "【心智模型切换成功】\n" + res.Message
	if status, ok := t.router.ModelDiscoveryStatus(res.Channel); ok && status.Failed {
		message += "\n注意：该通道本次模型发现失败，当前模型清单来自配置或上次成功记录；模型设置已切换，但目标模型当前是否可调用尚未确认。"
	}
	return &core.ToolResult{
		Text: message,
	}
}
