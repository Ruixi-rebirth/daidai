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

// DelegateTool 工作区工程任务委派工具 (实现 core.Tool)
type DelegateTool struct {
	delegates  *entity.Delegates
	immune     *entity.ImmuneSystem
	workspaces *entity.WorkspaceManager
}

// NewDelegateTool 实例化工程任务委派工具
func NewDelegateTool(delegates *entity.Delegates, immune *entity.ImmuneSystem) *DelegateTool {
	return NewDelegateToolWithWorkspace(delegates, immune, nil)
}

// NewDelegateToolWithWorkspace 支持工作空间感知实例化工程任务委派工具
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
	return "在当前或已纳管的工作空间中执行工程任务。每张工单最多运行 5 分钟；预计超过单次时限的目标必须拆成可验证阶段，逐阶段委派。AGY 会续接同一会话和工作空间的上下文；阶段未完成整体目标时，应根据阶段结果继续委派，不能把单阶段结果报告为整体完成。若本轮工具调用额度用尽，向用户说明已完成阶段、剩余阶段和明确续跑步骤。AGY 工作者会按配置直接访问该工作空间并可能修改其中的文件；这不是操作系统级隔离沙箱。当需要检查或修改项目、执行配置好的工作者任务时使用（普通网页检索请优先使用 search_web 工具）。"
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
				"description": "需要完成的具体目标或当前阶段目标；预计超过 5 分钟时，只描述一个可独立验证的阶段，并注明阶段完成标准（选用 local 引擎时传入 Shell 命令）",
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
	if event == nil || t.immune == nil {
		return &core.ToolResult{Text: "权限校验不可用，拒绝签发工程工单"}, nil
	}
	if permErr := t.immune.VerifyDelegationPerm(event.Role); permErr != nil {
		return &core.ToolResult{Text: permErr.Error()}, nil
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

	var senderID, channelID string
	if event != nil {
		senderID = event.SenderID
		channelID = event.ChannelID
	}

	ws := strings.TrimSpace(args.Workspace)
	if ws != "" && t.workspaces == nil {
		return &core.ToolResult{Text: "无法校验目标工作区，已拒绝执行显式路径工单。请挂载工作区管理器，或省略 workspace 使用 Worker 默认目录。"}, nil
	}
	if ws == "" && t.workspaces != nil && channelID != "" {
		var err error
		ws, _, err = t.workspaces.GetCurrentChecked(channelID)
		if err != nil {
			return &core.ToolResult{Text: fmt.Sprintf("无法确认当前工作区，已拒绝签发工单：%v", err)}, nil
		}
	} else if ws != "" && t.workspaces != nil {
		currWs, _, err := t.workspaces.GetCurrentChecked(channelID)
		if err != nil {
			return &core.ToolResult{Text: fmt.Sprintf("无法确认当前工作区，已拒绝校验目标：%v", err)}, nil
		}
		bookmarks := []entity.WorkspaceBookmark(nil)
		if channelID != "" {
			_, listedBookmarks, listErr := t.workspaces.ListBookmarkedChecked(channelID)
			if listErr != nil {
				return &core.ToolResult{Text: fmt.Sprintf("无法校验目标工作空间权限：%v", listErr)}, nil
			}
			bookmarks = listedBookmarks
		}

		// Resolve the same well-known workspace aliases and bookmarked names that
		// switch_workspace accepts before enforcing the ticket workspace allowlist.
		switch {
		case strings.EqualFold(ws, "sandbox"), strings.EqualFold(ws, "default"),
			strings.EqualFold(ws, "exit"), strings.EqualFold(ws, "~"):
			ws = t.workspaces.SandboxDir()
		case strings.EqualFold(ws, "current"), ws == ".":
			ws = currWs
		default:
			for _, bookmark := range bookmarks {
				if strings.EqualFold(bookmark.ProjectName, ws) ||
					filepath.Clean(bookmark.Path) == filepath.Clean(ws) {
					ws = bookmark.Path
					break
				}
			}
		}

		isCurrent := currWs != "" && filepath.Clean(currWs) == filepath.Clean(ws)
		isSandbox := t.workspaces.IsSandbox(ws)
		isBookmarked := false
		for _, b := range bookmarks {
			if filepath.Clean(b.Path) == filepath.Clean(ws) {
				isBookmarked = true
				ws = b.Path
				break
			}
		}
		if !isCurrent && !isSandbox && !isBookmarked {
			return &core.ToolResult{
				Text: fmt.Sprintf("【工单排期失败】权限拒绝：工作空间 %q 未纳管。仅允许在当前工作空间、默认工作区或已书签的工程中执行工单。", ws),
			}, nil
		}
	}

	allowSensitive := event.IsPartnerPrivate()
	ticket := t.delegates.CreateTicket(senderID, channelID, goal, args.Model, ws, args.Worker, allowSensitive)
	slog.Info("delegate_tool: 已签发独立工程工单", "ticketID", ticket.ID, "goalBytes", len(ticket.Goal), "worker", args.Worker)

	var progressTimer *time.Timer
	var progressDone chan struct{}
	if event.OnProgress != nil {
		wsName := filepath.Base(ticket.Workspace)
		if t.workspaces != nil && t.workspaces.IsSandbox(ticket.Workspace) {
			wsName = "sandbox"
		}
		progressDone = make(chan struct{})
		progressTimer = time.AfterFunc(3*time.Second, func() {
			defer close(progressDone)
			if ctx.Err() == nil {
				event.OnProgress(fmt.Sprintf("正在后台处理 (%s)，请稍候...", wsName))
			}
		})
	}

	output, execErr := t.delegates.ExecuteTicket(ctx, ticket)
	if progressTimer != nil && !progressTimer.Stop() {
		// Stop 返回 false 表示回调已开始或已结束；等它退出，避免最终结果先于过期进度发出。
		<-progressDone
	}
	if execErr != nil {
		errText := execErr.Error()
		if !allowSensitive {
			errText = core.RedactSensitiveIPs(errText)
		}
		slog.Warn("delegate_tool: 工单未能完成", "ticketID", ticket.ID, "err", errText)
		output = fmt.Sprintf("工单执行遇到异常: %s", errText)
	}

	return &core.ToolResult{
		Text: fmt.Sprintf("【工单 #%s 执行结果】：\n%s", ticket.ID, output),
	}, nil
}
