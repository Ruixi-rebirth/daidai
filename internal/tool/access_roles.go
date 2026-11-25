package tool

import (
	"context"
	"fmt"
	"strings"

	"daidai/internal/core"
	"daidai/internal/entity"
)

func (t *AccessTool) getRole(ctx context.Context, rawUserID string) (*core.ToolResult, error) {
	rawUserID = strings.TrimSpace(rawUserID)
	if rawUserID == "" {
		return &core.ToolResult{Text: "请指定要查看的用户序号（如 #1）、昵称或完整 OpenID。"}, nil
	}
	target, err := t.resolveTarget(ctx, rawUserID, true)
	if err != nil {
		return &core.ToolResult{Text: err.Error()}, nil
	}
	role, err := t.db.GetUserRole(ctx, "qq", target.UserID)
	if err != nil {
		return nil, fmt.Errorf("读取用户权限失败: %w", err)
	}
	if role == nil {
		return &core.ToolResult{Text: fmt.Sprintf("用户 %s 目前没有已生效的数据库授权记录（未授权访客）。", target.displayDesc())}, nil
	}
	roleDesc := "私聊好友（friend，仅开放日常私聊对话）"
	if role.Role == "collaborator" {
		roleDesc = "协作者（collaborator，开放私聊、动态表情与群画图免限额）"
	}
	return &core.ToolResult{
		Text: fmt.Sprintf("用户 %s 的当前权限信息：\n- 权限角色：%s\n- 授权时间：%s\n- 授权操作人：`%s`",
			target.displayDesc(), roleDesc, role.UpdatedAt.Format("2006-01-02 15:04:05"), role.GrantedBy),
	}, nil
}

func (t *AccessTool) resolveMutableTarget(ctx context.Context, rawUserID string, preferAuthorized bool) (*resolvedTarget, error) {
	target, err := t.resolveTarget(ctx, rawUserID, preferAuthorized)
	if err != nil {
		return nil, err
	}
	if !validAccessUserID(target.UserID) {
		return nil, fmt.Errorf("未能识别目标用户的有效 OpenID。")
	}
	if t.immune.ResolveRole(target.UserID) == entity.RolePartner {
		return nil, fmt.Errorf("核心搭档权限不能通过权限管理操作。")
	}
	return target, nil
}

func (t *AccessTool) grant(ctx context.Context, event *entity.SensoryEvent, rawUserID, role string) (*core.ToolResult, error) {
	rawUserID = strings.TrimSpace(rawUserID)
	if rawUserID == "" {
		return &core.ToolResult{Text: "请指定要授权的申请者序号（如 #1）、昵称或完整 OpenID。"}, nil
	}
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		role = "friend"
	}
	if role != "friend" && role != "collaborator" {
		return &core.ToolResult{Text: "请指定授权角色：friend（私聊好友）或 collaborator（协作者）。"}, nil
	}

	target, err := t.resolveMutableTarget(ctx, rawUserID, false)
	if err != nil {
		return &core.ToolResult{Text: err.Error()}, nil
	}

	effectiveRole, err := t.db.GrantUserRole(ctx, "qq", target.UserID, role, event.SenderID)
	if err != nil {
		return nil, fmt.Errorf("保存权限失败: %w", err)
	}

	if effectiveRole == "collaborator" {
		t.immune.AddCollaborator(target.UserID)
		t.immune.AddPrivateUser(target.UserID)
		if role != effectiveRole {
			return &core.ToolResult{Text: fmt.Sprintf("%s 已拥有协作者权限，本次添加好友保留现有协作者权限。若要降级，请明确要求修改权限为好友。", target.displayDesc())}, nil
		}
		return &core.ToolResult{Text: fmt.Sprintf("已将 %s 加为 QQ 协作者，已开放私聊对话、动态表情与群画图免限额，即时生效。", target.displayDesc())}, nil
	}
	t.immune.AddPrivateUser(target.UserID)
	return &core.ToolResult{Text: fmt.Sprintf("已将 %s 加为 QQ 好友，已开放日常私聊对话权限，即时生效。", target.displayDesc())}, nil
}

func (t *AccessTool) modify(ctx context.Context, event *entity.SensoryEvent, rawUserID, role string) (*core.ToolResult, error) {
	rawUserID = strings.TrimSpace(rawUserID)
	if rawUserID == "" {
		return &core.ToolResult{Text: "请指定要修改权限的用户序号（如 #1）、昵称或完整 OpenID。"}, nil
	}
	role = strings.ToLower(strings.TrimSpace(role))
	if role != "friend" && role != "collaborator" {
		return &core.ToolResult{Text: "请指定目标角色：friend（私聊好友）或 collaborator（协作者）。"}, nil
	}

	target, err := t.resolveMutableTarget(ctx, rawUserID, true)
	if err != nil {
		return &core.ToolResult{Text: err.Error()}, nil
	}

	if err := t.db.SetUserRole(ctx, "qq", target.UserID, role, event.SenderID); err != nil {
		return nil, fmt.Errorf("修改用户权限失败: %w", err)
	}

	if role == "collaborator" {
		t.immune.AddCollaborator(target.UserID)
		t.immune.AddPrivateUser(target.UserID)
		return &core.ToolResult{Text: fmt.Sprintf("已将 %s 的权限修改为 协作者（collaborator），已开放私聊对话、动态表情与群画图免限额，即时生效。", target.displayDesc())}, nil
	}
	t.immune.RemoveCollaborator(target.UserID)
	t.immune.AddPrivateUser(target.UserID)
	return &core.ToolResult{Text: fmt.Sprintf("已将 %s 的权限修改为 私聊好友（friend），仅保留日常私聊对话权限，即时生效。", target.displayDesc())}, nil
}

func (t *AccessTool) revoke(ctx context.Context, event *entity.SensoryEvent, rawUserID string) (*core.ToolResult, error) {
	rawUserID = strings.TrimSpace(rawUserID)
	if rawUserID == "" {
		return &core.ToolResult{Text: "请指定要删除或撤销的用户序号（如 #1）、昵称或完整 OpenID。"}, nil
	}

	target, err := t.resolveMutableTarget(ctx, rawUserID, false)
	if err != nil {
		return &core.ToolResult{Text: err.Error()}, nil
	}

	if err := t.db.RevokeCollaborator(ctx, "qq", target.UserID, event.SenderID); err != nil {
		return nil, fmt.Errorf("删除用户权限失败: %w", err)
	}
	t.immune.RemoveCollaborator(target.UserID)
	t.immune.RemovePrivateUser(target.UserID)

	if target.IsPending {
		return &core.ToolResult{Text: fmt.Sprintf("已拒绝 %s 的 QQ 私聊权限申请。", target.displayDesc())}, nil
	}
	return &core.ToolResult{Text: fmt.Sprintf("已删除 %s 的 QQ 访问权限，该用户已恢复为未授权访客。", target.displayDesc())}, nil
}
