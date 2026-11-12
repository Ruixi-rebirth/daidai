package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"daidai/internal/core"
	"daidai/internal/entity"
)

// WorkspaceTool 动态工作空间管理工具 (实现 core.Tool，直接复用 WorkspaceManager.ExecuteAction)
type WorkspaceTool struct {
	workspaces *entity.WorkspaceManager
	immune     *entity.ImmuneSystem
	delegates  *entity.Delegates
}

// NewWorkspaceTool 实例化工作空间管理工具
func NewWorkspaceTool(workspaces *entity.WorkspaceManager, immune *entity.ImmuneSystem, delegates ...*entity.Delegates) *WorkspaceTool {
	var scheduler *entity.Delegates
	if len(delegates) > 0 {
		scheduler = delegates[0]
	}
	return &WorkspaceTool{
		workspaces: workspaces,
		immune:     immune,
		delegates:  scheduler,
	}
}

func (t *WorkspaceTool) Name() string {
	return "switch_workspace"
}

func (t *WorkspaceTool) Description() string {
	return "查询当前会话绑定的项目工作空间目录、列出已纳管的工作空间书签列表，或切换/注销工作空间目录。"
}

func (t *WorkspaceTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"current", "list", "switch", "remove", "clean", "exit"},
				"description": "执行动作：'current' 查看当前工作空间与模式，'list' 列出纳管工程，'switch' 切入生产工程，'remove' 注销书签，'clean' 清理专用默认工作区，'exit' 退出工程切回默认工作区",
			},
			"target": map[string]any{
				"type":        "string",
				"description": "目标路径或工程名称（当 action 为 switch 或 remove 时必填）",
			},
		},
		"required": []string{"action"},
	}
}

type workspaceArgs struct {
	Action string `json:"action"`
	Target string `json:"target"`
}

func (t *WorkspaceTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	if t.workspaces == nil {
		return &core.ToolResult{Text: "工作空间中枢未配置"}, nil
	}

	var args workspaceArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, fmt.Errorf("invalid switch_workspace args: %w", err)
	}

	var channelID string
	role := entity.RoleGuest
	event := entity.SensoryEventFromContext(ctx)
	if event != nil {
		channelID = event.ChannelID
		role = event.Role
	}

	result := &core.ToolResult{
		Text: t.workspaces.ExecuteAction(channelID, role, t.immune, args.Action, args.Target, t.delegates),
	}
	if event == nil || !event.IsPartnerPrivate() {
		result.Text = core.RedactSensitiveIPs(result.Text)
	}
	return result, nil
}
