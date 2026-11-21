package tool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
)

func (t *AccessTool) createInvite(ctx context.Context, event *entity.SensoryEvent, args accessToolArgs) (*core.ToolResult, error) {
	if args.Role != "friend" && args.Role != "collaborator" {
		return &core.ToolResult{Text: "请指定邀请码权限：friend（仅私聊访客）或 collaborator（完整协作者）。"}, nil
	}
	uses := args.MaxUses
	if uses == 0 {
		uses = 1
	}
	hours := args.ValidityHours
	if hours == 0 {
		hours = 168
	}
	if uses < 1 || uses > 100 || hours < 1 || hours > 720 {
		return &core.ToolResult{Text: "使用次数范围为 1–100，有效期范围为 1–720 小时。"}, nil
	}
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	for attempt := 0; attempt < 5; attempt++ {
		var raw [20]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, fmt.Errorf("生成安全邀请码失败: %w", err)
		}
		var code strings.Builder
		code.WriteString("DAIDAI-")
		for _, b := range raw[:12] {
			code.WriteByte(alphabet[int(b)%len(alphabet)])
		}
		idBytes := make([]byte, 12)
		if _, err := rand.Read(idBytes); err != nil {
			return nil, err
		}
		id := hex.EncodeToString(idBytes)
		expires := time.Now().Add(time.Duration(hours) * time.Hour)
		err := t.db.CreatePrivateAccessInvite(ctx, "qq", id, db.HashAccessInviteCode(code.String()), args.Role, event.SenderID, expires, uses)
		if err == nil {
			return &core.ToolResult{Text: fmt.Sprintf("已创建邀请码：`%s`\n权限：%s\n最多兑换：%d 次\n有效期至：%s\n请将邀请码原样发给对方；出于安全考虑，之后无法再次查看完整邀请码。", code.String(), args.Role, uses, expires.Format("2006-01-02 15:04:05"))}, nil
		}
		if !errors.Is(err, db.ErrAccessInviteConflict) {
			return nil, fmt.Errorf("创建邀请码失败: %w", err)
		}
	}
	return nil, fmt.Errorf("生成唯一邀请码失败，请重试")
}

func (t *AccessTool) listInvites(ctx context.Context) (*core.ToolResult, error) {
	invites, err := t.db.ListPrivateAccessInvites(ctx, "qq", 20)
	if err != nil {
		return nil, err
	}
	if len(invites) == 0 {
		return &core.ToolResult{Text: "目前没有邀请码记录。"}, nil
	}
	var out strings.Builder
	out.WriteString("最近的邀请码（不会显示完整邀请码）：\n")
	for _, invite := range invites {
		state := "可用"
		if invite.RevokedAt != nil {
			state = "已撤销"
		} else if time.Now().After(invite.ExpiresAt) {
			state = "已过期"
		} else if invite.UsedCount >= invite.MaxUses {
			state = "已用完"
		}
		fmt.Fprintf(&out, "- ID `%s` · 权限 %s · %s · 已用 %d/%d · 到期 %s\n", invite.ID, invite.Role, state, invite.UsedCount, invite.MaxUses, invite.ExpiresAt.Format("2006-01-02 15:04"))
	}
	return &core.ToolResult{Text: out.String()}, nil
}

func (t *AccessTool) revokeInvite(ctx context.Context, id string) (*core.ToolResult, error) {
	if strings.TrimSpace(id) == "" {
		return &core.ToolResult{Text: "请提供 invites 列表中的完整邀请码 ID。"}, nil
	}
	if err := t.db.RevokePrivateAccessInvite(ctx, "qq", id); err != nil {
		return &core.ToolResult{Text: "未找到可撤销的邀请码 ID。"}, nil
	}
	return &core.ToolResult{Text: "邀请码已撤销，后续无法兑换。"}, nil
}
