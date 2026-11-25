package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/media"
)

// DelegateTool 工作区工程任务委派工具 (实现 core.Tool)
type DelegateTool struct {
	delegates  *entity.Delegates
	immune     *entity.ImmuneSystem
	workspaces *entity.WorkspaceManager
	inbox      *media.Inbox
}

// NewDelegateTool 实例化工程任务委派工具
func NewDelegateTool(delegates *entity.Delegates, immune *entity.ImmuneSystem) *DelegateTool {
	return NewDelegateToolWithWorkspace(delegates, immune, nil)
}

// NewDelegateToolWithWorkspace 支持工作空间感知实例化工程任务委派工具
func NewDelegateToolWithWorkspace(delegates *entity.Delegates, immune *entity.ImmuneSystem, workspaces *entity.WorkspaceManager) *DelegateTool {
	return NewDelegateToolWithInbox(delegates, immune, workspaces, nil)
}

func NewDelegateToolWithInbox(delegates *entity.Delegates, immune *entity.ImmuneSystem, workspaces *entity.WorkspaceManager, inbox *media.Inbox) *DelegateTool {
	return &DelegateTool{
		delegates:  delegates,
		immune:     immune,
		workspaces: workspaces,
		inbox:      inbox,
	}
}

func (t *DelegateTool) Name() string {
	return "delegate_ticket"
}

func (t *DelegateTool) Description() string {
	timeLimit := "无调度器硬时限（可通过 /stop 中断）"
	if t.delegates != nil && t.delegates.ExecutionTimeout() > 0 {
		timeLimit = t.delegates.ExecutionTimeout().String()
	}
	return fmt.Sprintf("创建、列出或查询项目后台工程工单（普通网页检索请用 search_web）。action='create'（默认）创建后台工单并立即返回短编号，执行时限 %s，完成后主动通知；当搭档询问工单进度、报错原因或要求接着做之前未完成的工单时，先用 action='status'（或 'list'）读取工单实时进度与完整输出。工作者可直接修改工作区文件。仅在用户明确要求时通过附件参数交付文件。", timeLimit)
}

func (t *DelegateTool) Parameters() map[string]any {
	var workerDescs []string
	var workerNames []string
	if t.delegates != nil {
		for _, w := range t.delegates.ListWorkers() {
			workerNames = append(workerNames, w.Name)
			workerDescs = append(workerDescs, fmt.Sprintf("%s (%s)", w.Name, w.Description))
		}
	}
	workerDescText := "可选指定执行工单的工作者引擎，留空则自动选用主力引擎。"
	if len(workerDescs) > 0 {
		workerDescText += "当前已挂载引擎：" + strings.Join(workerDescs, "；")
	}

	workerProp := map[string]any{
		"type":        "string",
		"description": workerDescText,
	}
	if len(workerNames) > 0 {
		workerProp["enum"] = workerNames
	}

	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"create", "status", "list"},
				"description": "执行动作：'create'（默认）创建新工单；'status' 查看指定或最近一张工单的实时步骤与完整输出；'list' 列出当前私聊近期工单",
			},
			"ticket_id": map[string]any{
				"type":        "string",
				"description": "仅 action='status' 时可选：指定工单编号（支持短编号或前缀，留空自动读取正在运行或最近一张工单）",
			},
			"goal": map[string]any{
				"type":        "string",
				"description": fmt.Sprintf("action='create' 时必填：需要完成的具体目标或当前阶段目标；较长目标请说明可独立验证的阶段和完成标准（选用 local 引擎时传入 Shell 命令）。当前工单时限：%s", t.executionTimeoutDescription()),
			},
			"worker": workerProp,
			"model": map[string]any{
				"type":        "string",
				"description": "仅 AGY 工作者支持：可选指定 AGY 模型 ID 或关键字；留空沿用搭档私聊当前模型",
			},
			"workspace": map[string]any{
				"type":        "string",
				"description": "可选指定的目标工作空间目录，默认当前项目目录",
			},
			"include_pending_attachments": map[string]any{
				"type":        "boolean",
				"description": "仅当用户明确要求处理此前消息发送的文件时设为 true；可以与 include_current_attachments 同时设为 true",
			},
			"include_current_attachments": map[string]any{
				"type":        "boolean",
				"description": "仅当用户明确要求处理当前消息新上传的文件时设为 true；不要因调用工程工单就默认带上附件",
			},
		},
	}
}

func (t *DelegateTool) executionTimeoutDescription() string {
	if t.delegates == nil || t.delegates.ExecutionTimeout() == 0 {
		return "无调度器硬时限"
	}
	return t.delegates.ExecutionTimeout().String()
}

type delegateArgs struct {
	Action                    string `json:"action"`
	TicketID                  string `json:"ticket_id"`
	Goal                      string `json:"goal"`
	Worker                    string `json:"worker"`
	Model                     string `json:"model"`
	Workspace                 string `json:"workspace"`
	IncludePendingAttachments bool   `json:"include_pending_attachments"`
	IncludeCurrentAttachments bool   `json:"include_current_attachments"`
}

func (t *DelegateTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	if t.delegates == nil {
		return nil, fmt.Errorf("delegates subsystem not configured")
	}

	event := entity.SensoryEventFromContext(ctx)
	if event == nil || t.immune == nil {
		return &core.ToolResult{Text: "权限校验不可用，拒绝签发工程工单"}, nil
	}
	if permErr := t.immune.VerifyDelegationPerm(event.Role); permErr != nil {
		return &core.ToolResult{Text: permErr.Error()}, nil
	}
	if event.IsGroup {
		return &core.ToolResult{Text: fmt.Sprintf("工程工单仅用于私聊工程协作，请在与%s的私聊中发起。", config.BotName())}, nil
	}

	var args delegateArgs
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, fmt.Errorf("invalid delegate_ticket args: %w", err)
		}
	}
	action := strings.ToLower(strings.TrimSpace(args.Action))
	if action == "" {
		if strings.TrimSpace(args.Goal) == "" && strings.TrimSpace(args.TicketID) != "" {
			action = "status"
		} else {
			action = "create"
		}
	}
	switch action {
	case "list":
		return t.listTicketsForPartner(ctx, event), nil
	case "status":
		return t.inspectTicketForPartner(ctx, event, args.TicketID), nil
	case "create":
	default:
		return &core.ToolResult{Text: fmt.Sprintf("未知工单操作 %q，支持的操作为 create, status, list", action)}, nil
	}
	goal := strings.TrimSpace(args.Goal)
	if goal == "" {
		return &core.ToolResult{Text: "工单目标不能为空，请提供一个可执行且可验证的任务阶段。"}, nil
	}
	if strings.TrimSpace(args.Model) != "" {
		workerName := strings.TrimSpace(args.Worker)
		if workerName == "" {
			workerName = t.delegates.DefaultName()
		}
		if !strings.EqualFold(workerName, "agy") {
			return &core.ToolResult{Text: "model 参数目前只支持 AGY 工作者；其他 CLI 工作者使用配置中的固定命令参数。"}, nil
		}
	}
	attachmentsToProcess, attachmentErr := t.selectTicketAttachments(event, &args)
	if attachmentErr != nil {
		return &core.ToolResult{Text: attachmentErr.Error()}, nil
	}
	channelID := event.ChannelID

	ws, workspaceErr := t.resolveTicketWorkspace(event.Platform, channelID, args.Workspace)
	if workspaceErr != nil {
		return &core.ToolResult{Text: workspaceErr.Error()}, nil
	}
	submission := ticketSubmission{event: event, args: args, goal: goal, workspace: ws, attachments: attachmentsToProcess}
	if result, err := t.stageTicketAttachments(ctx, &submission); result != nil || err != nil {
		return result, err
	}
	return t.submitTicket(ctx, submission)
}

func (t *DelegateTool) listTicketsForPartner(ctx context.Context, event *entity.SensoryEvent) *core.ToolResult {
	tickets, err := t.delegates.TicketsFor(ctx, event.Platform, event.SenderID, event.ChannelID, 8)
	if len(tickets) == 0 {
		if err != nil {
			return &core.ToolResult{Text: fmt.Sprintf("查询近期工单失败：%v", err)}
		}
		return &core.ToolResult{Text: "当前私聊暂无近期工程工单。"}
	}
	var sb strings.Builder
	sb.WriteString("【近期工程工单列表】\n")
	for _, tk := range tickets {
		modelHint := ""
		if tk.Model != "" {
			modelHint = " · " + tk.Model
		}
		fmt.Fprintf(&sb, "- `%s` [%s%s]：%s\n", tk.ID, tk.Status, modelHint, core.TruncateRunes(strings.TrimSpace(tk.Goal), 120, "…"))
	}
	return &core.ToolResult{Text: strings.TrimSpace(sb.String())}
}

func (t *DelegateTool) inspectTicketForPartner(ctx context.Context, event *entity.SensoryEvent, rawID string) *core.ToolResult {
	targetID := strings.TrimSpace(rawID)
	if targetID == "" {
		recent, err := t.delegates.TicketsFor(ctx, event.Platform, event.SenderID, event.ChannelID, 1)
		if len(recent) == 0 {
			if err != nil {
				return &core.ToolResult{Text: fmt.Sprintf("查询近期工单失败：%v", err)}
			}
			return &core.ToolResult{Text: "当前私聊没有可查询的工程工单。"}
		}
		targetID = recent[0].ID
	}
	tk, err := t.delegates.TicketFor(ctx, event.Platform, event.SenderID, event.ChannelID, targetID)
	if tk == nil {
		if err != nil {
			return &core.ToolResult{Text: fmt.Sprintf("读取工单 %q 失败：%v", targetID, err)}
		}
		return &core.ToolResult{Text: fmt.Sprintf("未找到编号为 %q 的工程工单。", targetID)}
	}
	out := strings.TrimSpace(tk.Output)
	if out == "" {
		out = "（暂无执行输出）"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "【工单详情 · %s】\n- 状态：%s\n- 目标：%s\n", tk.ID, tk.Status, core.TruncateRunes(strings.TrimSpace(tk.Goal), 300, "…"))
	if tk.Workspace != "" {
		fmt.Fprintf(&sb, "- 工作区：%s\n", tk.Workspace)
	}
	if tk.Worker != "" {
		fmt.Fprintf(&sb, "- 工作者：%s\n", tk.Worker)
	}
	if tk.Model != "" {
		fmt.Fprintf(&sb, "- 模型：%s\n", tk.Model)
	}
	sb.WriteString("\n【执行输出】\n")
	sb.WriteString(core.TruncateRunes(out, 6000, "\n…[输出较长已截断]"))
	return &core.ToolResult{Text: strings.TrimSpace(sb.String())}
}
