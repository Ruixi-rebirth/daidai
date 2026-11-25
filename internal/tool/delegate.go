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
	return fmt.Sprintf("检查、修改或执行项目任务时创建后台工单；普通网页检索使用 search_web。排队后立即返回编号，完成时主动通知，可用 /task 查看、/stop 取消；执行时限 %s。任务不受本轮对话超时影响，服务重启会中断未完成工单。工作者可直接修改工作区文件，不是系统隔离沙箱。仅在用户明确要求时通过附件参数交付文件。", timeLimit)
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
			"goal": map[string]any{
				"type":        "string",
				"description": fmt.Sprintf("需要完成的具体目标或当前阶段目标；较长目标请说明可独立验证的阶段和完成标准（选用 local 引擎时传入 Shell 命令）。当前工单时限：%s", t.executionTimeoutDescription()),
			},
			"worker": workerProp,
			"model": map[string]any{
				"type":        "string",
				"description": "仅 AGY 工作者支持：可选指定 AGY 模型 ID 或关键字；留空沿用 AGY 当前模型",
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
		"required": []string{"goal"},
	}
}

func (t *DelegateTool) executionTimeoutDescription() string {
	if t.delegates == nil || t.delegates.ExecutionTimeout() == 0 {
		return "无调度器硬时限"
	}
	return t.delegates.ExecutionTimeout().String()
}

type delegateArgs struct {
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
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, fmt.Errorf("invalid delegate_ticket args: %w", err)
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
