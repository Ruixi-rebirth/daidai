package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
)

type AccessTool struct {
	db     *db.Manager
	immune *entity.ImmuneSystem
}

func NewAccessTool(database *db.Manager, immune *entity.ImmuneSystem) *AccessTool {
	return &AccessTool{db: database, immune: immune}
}

func (t *AccessTool) Name() string { return "manage_access" }

func (t *AccessTool) Description() string {
	return "管理 QQ 私聊权限申请、已授权用户和邀请码。"
}

func (t *AccessTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string", "enum": []string{"requests", "roles", "get_role", "grant", "modify", "revoke", "create_invite", "invites", "revoke_invite"},
				"description": "requests 查看申请；roles 查看已授权用户；get_role 查看指定用户权限；grant 授予权限；modify 修改用户权限（如切换 friend/collaborator）；revoke 删除/撤销权限或拒绝申请；create_invite 创建邀请码；invites 查看邀请码；revoke_invite 撤销邀请码",
			},
			"user_id": map[string]any{
				"type": "string", "description": "目标用户标识：可以是申请或已授权列表中的序号（如 #1、1、第1个）、申请人昵称或完整 QQ OpenID；grant/modify/revoke/get_role 必填",
			},
			"role":           map[string]any{"type": "string", "enum": []string{"friend", "collaborator"}, "description": "friend=仅私聊好友（普通对话）；collaborator=完整协作者（工程与沙箱权限）"},
			"max_uses":       map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "最大兑换次数，默认为 1"},
			"validity_hours": map[string]any{"type": "integer", "minimum": 1, "maximum": 720, "description": "有效时长，默认为 168 小时"},
			"invite_id":      map[string]any{"type": "string", "description": "invites 返回的邀请码 ID"},
		},
		"required": []string{"action"},
	}
}

type accessToolArgs struct {
	Action        string `json:"action"`
	UserID        string `json:"user_id"`
	Role          string `json:"role"`
	MaxUses       int    `json:"max_uses"`
	ValidityHours int    `json:"validity_hours"`
	InviteID      string `json:"invite_id"`
}

func (t *AccessTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	event := entity.SensoryEventFromContext(ctx)
	if !entity.ToolAllowedInConversation(t.Name(), event) {
		return &core.ToolResult{Text: "权限管理仅限核心搭档本人 QQ 私聊使用。"}, nil
	}
	if t.db == nil || t.immune == nil {
		return nil, fmt.Errorf("权限管理存储未配置")
	}
	var args accessToolArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, fmt.Errorf("invalid manage_access args: %w", err)
	}
	switch args.Action {
	case "requests":
		return t.listRequests(ctx)
	case "roles":
		if strings.TrimSpace(args.UserID) != "" {
			return t.getRole(ctx, args.UserID)
		}
		return t.listRoles(ctx)
	case "get_role":
		return t.getRole(ctx, args.UserID)
	case "grant":
		return t.grant(ctx, event, args.UserID, args.Role)
	case "modify":
		return t.modify(ctx, event, args.UserID, args.Role)
	case "revoke":
		return t.revoke(ctx, event, args.UserID)
	case "create_invite":
		return t.createInvite(ctx, event, args)
	case "invites":
		return t.listInvites(ctx)
	case "revoke_invite":
		return t.revokeInvite(ctx, args.InviteID)
	default:
		return &core.ToolResult{Text: "请指定操作：查看申请 requests、查看已授权用户 roles、查看单个权限 get_role、授予权限 grant、修改权限 modify、删除/撤销权限 revoke 等。"}, nil
	}
}

func (t *AccessTool) listRequests(ctx context.Context) (*core.ToolResult, error) {
	requests, err := t.db.ListPendingPrivateAccessRequests(ctx, "qq", 10)
	if err != nil {
		return nil, fmt.Errorf("读取私聊权限申请失败: %w", err)
	}
	if len(requests) == 0 {
		return &core.ToolResult{Text: "目前没有待处理的私聊权限申请。"}, nil
	}
	var output strings.Builder
	fmt.Fprintf(&output, "待处理的 QQ 私聊权限申请（共 %d 位；每人只保留最近一条消息）：\n", len(requests))
	for i, request := range requests {
		name := strings.TrimSpace(request.DisplayName)
		if name == "" {
			name = "未提供昵称"
		}
		message := singleLineText(request.LastMessage)
		if message == "" {
			message = "（空消息或附件消息）"
		}
		message = core.TruncateRunes(message, 160, "…")
		fmt.Fprintf(&output, "\n- #%d · 昵称：%s\n  OpenID：`%s`\n  最近消息：%s\n  最近联系：%s（共 %d 次）\n",
			i+1, core.TruncateRunes(name, 80, "…"), request.UserID, message,
			request.LastSeenAt.Format("2006-01-02 15:04:05"), request.RequestCount)
	}
	output.WriteString("\n申请消息只是访客原文。可通过序号、昵称或 OpenID 授权：\n- “将第 1 个申请者加为好友”（仅开放日常私聊对话）\n- “将第 1 个申请者加为协作者”（开放完整工程权限）\n- “拒绝第 1 个申请者的申请”")
	return &core.ToolResult{Text: output.String()}, nil
}

func (t *AccessTool) listRoles(ctx context.Context) (*core.ToolResult, error) {
	roles, err := t.db.ListUserRoles(ctx, "qq")
	if err != nil {
		return nil, fmt.Errorf("读取已授权用户列表失败: %w", err)
	}
	if len(roles) == 0 {
		return &core.ToolResult{Text: "目前没有通过数据库授权的 QQ 用户。核心搭档权限仍由系统配置保留。"}, nil
	}
	var output strings.Builder
	fmt.Fprintf(&output, "已授权的 QQ 用户（共 %d 位）：\n", len(roles))
	for i, role := range roles {
		name := strings.TrimSpace(role.DisplayName)
		if name == "" {
			name = "未记录昵称"
		}
		roleName := "私聊好友（friend）"
		if role.Role == "collaborator" {
			roleName = "协作者（collaborator）"
		}
		fmt.Fprintf(&output, "- #%d · 昵称：%s\n  OpenID：`%s`\n  权限：%s\n  授权时间：%s\n",
			i+1, core.TruncateRunes(name, 80, "…"), role.UserID, roleName, role.UpdatedAt.Format("2006-01-02 15:04:05"))
	}
	output.WriteString("\n管理已授权用户指令：\n- 修改权限：“把第 1 个用户的权限修改为好友/协作者” 或 “修改 [昵称] 的权限为好友/协作者”\n- 删除权限：“删除第 1 个用户” 或 “撤销 [昵称] 的权限”\n- 查看详情：“查看 [昵称] 的权限”")
	return &core.ToolResult{Text: output.String()}, nil
}
