package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
)

// DelegateTool 独立沙箱工单委派工具 (实现 core.Tool)
type DelegateTool struct {
	delegates  *entity.Delegates
	immune     *entity.ImmuneSystem
	workspaces *entity.WorkspaceManager
}

// NewDelegateTool 实例化沙箱工单委派工具
func NewDelegateTool(delegates *entity.Delegates, immune *entity.ImmuneSystem) *DelegateTool {
	return NewDelegateToolWithWorkspace(delegates, immune, nil)
}

// NewDelegateToolWithWorkspace 支持工作空间感知实例化沙箱工单委派工具
func NewDelegateToolWithWorkspace(delegates *entity.Delegates, immune *entity.ImmuneSystem, workspaces *entity.WorkspaceManager) *DelegateTool {
	return &DelegateTool{
		delegates:  delegates,
		immune:     immune,
		workspaces: workspaces,
	}
}

func (t *DelegateTool) Name() string {
	return "delegate_ticket"
}

func (t *DelegateTool) Description() string {
	return "在当前项目工作空间中执行终端命令（如 ls、cat、git、编译运行等）、文件/目录查看与深度工程代码任务。当需要查看当前目录文件、检查工程状态、执行 shell 终端或编写修改代码时，直接签发本工单执行（普通实时网页检索请优先使用 search_web 工具）。"
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
	workerDescText := "可选指定执行工单的沙箱工作者引擎，留空则自动选用主力引擎。"
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
				"description": "需要完成的具体任务目标（选用 local 引擎时传入 Shell 命令）",
			},
			"worker": workerProp,
			"model": map[string]any{
				"type":        "string",
				"description": "可选指定的目标执行模型 ID 或关键字（留空则沿用当前会话模型）",
			},
			"workspace": map[string]any{
				"type":        "string",
				"description": "可选指定的目标工作空间目录，默认当前项目目录",
			},
		},
		"required": []string{"goal"},
	}
}

type delegateArgs struct {
	Goal      string `json:"goal"`
	Worker    string `json:"worker"`
	Model     string `json:"model"`
	Workspace string `json:"workspace"`
}

func (t *DelegateTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	if t.delegates == nil {
		return nil, fmt.Errorf("delegates subsystem not configured")
	}

	event := entity.SensoryEventFromContext(ctx)
	if t.immune != nil && event != nil {
		if permErr := t.immune.VerifyDelegationPerm(event.Role); permErr != nil {
			return &core.ToolResult{Text: permErr.Error()}, nil
		}
	}

	var args delegateArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, fmt.Errorf("invalid delegate_ticket args: %w", err)
	}

	var senderID, channelID string
	if event != nil {
		senderID = event.SenderID
		channelID = event.ChannelID
	}

	ws := strings.TrimSpace(args.Workspace)
	if ws == "" && t.workspaces != nil && channelID != "" {
		ws, _ = t.workspaces.GetCurrent(channelID)
	} else if ws != "" && t.workspaces != nil {
		currWs, _ := t.workspaces.GetCurrent(channelID)
		isCurrent := currWs != "" && filepath.Clean(currWs) == filepath.Clean(ws)
		isSandbox := t.workspaces.IsSandbox(ws)
		isBookmarked := false
		_, bookmarked := t.workspaces.ListBookmarked(channelID)
		for _, b := range bookmarked {
			if filepath.Clean(b.Path) == filepath.Clean(ws) || b.ProjectName == ws {
				isBookmarked = true
				ws = b.Path
				break
			}
		}
		if !isCurrent && !isSandbox && !isBookmarked {
			return &core.ToolResult{
				Text: fmt.Sprintf("【工单排期失败】权限拒绝：工作空间 %q 未纳管。仅允许在当前工作空间、沙盒演练区或已书签的工程中执行工单。", ws),
			}, nil
		}
	}

	ticket := t.delegates.CreateTicket(senderID, channelID, args.Goal, args.Model, ws, args.Worker)
	ticket.AllowSensitive = event.IsPartnerPrivate()
	slog.Info("delegate_tool: 已签发独立工程工单", "ticketID", ticket.ID, "goal", ticket.Goal, "worker", args.Worker)

	if capErr := t.delegates.CheckCapacity(ticket); capErr != nil {
		slog.Warn("delegate_tool: 触发系统并发或工作空间保护，拒绝排期", "ticketID", ticket.ID, "err", capErr)
		return &core.ToolResult{
			Text: fmt.Sprintf("【工单排期失败】%v", capErr),
		}, nil
	}

	if event != nil && event.OnProgress != nil {
		wsName := filepath.Base(ticket.Workspace)
		if t.workspaces != nil && t.workspaces.IsSandbox(ticket.Workspace) {
			wsName = "sandbox"
		}
		timer := time.AfterFunc(3*time.Second, func() {
			if ctx.Err() == nil {
				event.OnProgress(fmt.Sprintf("正在后台处理 (%s)，请稍候...", wsName))
			}
		})
		defer timer.Stop()
	}

	output, execErr := t.delegates.ExecuteTicket(ctx, ticket)
	if execErr != nil {
		output = fmt.Sprintf("工单执行遇到异常: %v", execErr)
	}

	return &core.ToolResult{
		Text: fmt.Sprintf("【工单 #%s 执行结果】：\n%s", ticket.ID, output),
	}, nil
}
