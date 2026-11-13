package tool

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"daidai/internal/db"
	"daidai/internal/entity"
)

func TestParseApplicantIndex(t *testing.T) {
	cases := []struct {
		input    string
		expected int
	}{
		{"1", 1},
		{"2", 2},
		{"#1", 1},
		{"# 2", 2},
		{"1号", 1},
		{"2号", 2},
		{"第1个", 1},
		{"第 1 个", 1},
		{"第1位", 1},
		{"第一个", 1},
		{"第二个", 2},
		{"第1个申请者", 1},
		{"第一个申请者", 1},
		{"第2位申请者", 2},
		{"第1个用户", 1},
		{"第一个用户", 1},
		{"第2位用户", 2},
		{"两", 2},
		{"vinkon", 0},
		{"abc123", 0},
		{"", 0},
	}
	for _, c := range cases {
		if got := parseApplicantIndex(c.input); got != c.expected {
			t.Errorf("parseApplicantIndex(%q) = %d, expected %d", c.input, got, c.expected)
		}
	}
}

func TestAccessTool_FullLifecycle(t *testing.T) {
	tempBase := t.TempDir()
	dbMgr := db.NewManager(filepath.Join(tempBase, "db"))
	defer dbMgr.Close()

	partnerID := "partner_qq_001"
	immune := entity.NewImmuneSystem(partnerID)
	accessTool := NewAccessTool(dbMgr, immune)

	partnerEv := &entity.SensoryEvent{
		Platform:  "qq",
		ChannelID: partnerID,
		SenderID:  partnerID,
		Role:      entity.RolePartner,
	}
	partnerCtx := entity.WithSensoryEvent(context.Background(), partnerEv)

	// 1. 无申请时查看申请
	res, err := accessTool.Execute(partnerCtx, json.RawMessage(`{"action":"requests"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Text, "没有待处理") {
		t.Fatalf("expected empty requests message, got: %s", res.Text)
	}

	// 2. 模拟录入 2 个访客申请
	ctx := context.Background()
	_, _ = dbMgr.RecordPrivateAccessRequest(ctx, "qq", "user_openid_111", "vinkon", "你好呆呆")
	_, _ = dbMgr.RecordPrivateAccessRequest(ctx, "qq", "user_openid_222", "alice", "申请使用呆呆")

	// 3. 查看申请列表，应显示 2 个申请者并带 #1, #2 序号
	res, err = accessTool.Execute(partnerCtx, json.RawMessage(`{"action":"requests"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Text, "#1 · 昵称：alice") && !strings.Contains(res.Text, "#1 · 昵称：vinkon") {
		t.Fatalf("expected applicant #1 in list, got: %s", res.Text)
	}
	if !strings.Contains(res.Text, "加为好友") || !strings.Contains(res.Text, "加为协作者") {
		t.Fatalf("expected friend and collaborator guidance in footer, got: %s", res.Text)
	}

	// 4. 将第 1 个申请者加为好友 (默认 friend)
	res, err = accessTool.Execute(partnerCtx, json.RawMessage(`{"action":"grant","user_id":"第1个申请者"}`))
	if err != nil {
		t.Fatalf("grant error: %v", err)
	}
	if !strings.Contains(res.Text, "加为 QQ 好友") {
		t.Fatalf("expected grant friend confirmation, got: %s", res.Text)
	}

	// 5. 校验权限生效：已开通私聊访客权限，但不是协作者
	requests, _ := dbMgr.ListPendingPrivateAccessRequests(ctx, "qq", 10)
	// 第 1 个被批准后只剩 1 个待处理
	if len(requests) != 1 {
		t.Fatalf("expected 1 remaining pending request, got %d", len(requests))
	}
	remainingID := requests[0].UserID

	// 6. 将剩余的申请者通过昵称加为协作者
	res, err = accessTool.Execute(partnerCtx, json.RawMessage(`{"action":"grant","user_id":"`+requests[0].DisplayName+`","role":"collaborator"}`))
	if err != nil {
		t.Fatalf("grant collaborator error: %v", err)
	}
	if !strings.Contains(res.Text, "加为 QQ 协作者") {
		t.Fatalf("expected grant collaborator confirmation, got: %s", res.Text)
	}
	if immune.ResolveRole(remainingID) != entity.RoleCollaborator {
		t.Fatalf("expected %s to have collaborator role", remainingID)
	}

	// 7. 查看已授权用户列表 roles
	res, err = accessTool.Execute(partnerCtx, json.RawMessage(`{"action":"roles"}`))
	if err != nil {
		t.Fatalf("roles error: %v", err)
	}
	if !strings.Contains(res.Text, "已授权的 QQ 用户（共 2 位）") {
		t.Fatalf("expected 2 authorized users, got: %s", res.Text)
	}
	if !strings.Contains(res.Text, "私聊好友（friend）") || !strings.Contains(res.Text, "协作者（collaborator）") {
		t.Fatalf("expected both friend and collaborator in roles, got: %s", res.Text)
	}

	// 8. 查看单个用户权限 get_role
	res, err = accessTool.Execute(partnerCtx, json.RawMessage(`{"action":"get_role","user_id":"`+remainingID+`"}`))
	if err != nil {
		t.Fatalf("get_role error: %v", err)
	}
	if !strings.Contains(res.Text, "collaborator") {
		t.Fatalf("expected collaborator info in get_role, got: %s", res.Text)
	}

	// 9. 修改权限 modify：将协作者降为好友
	res, err = accessTool.Execute(partnerCtx, json.RawMessage(`{"action":"modify","user_id":"`+remainingID+`","role":"friend"}`))
	if err != nil {
		t.Fatalf("modify error: %v", err)
	}
	if !strings.Contains(res.Text, "修改为 私聊好友") {
		t.Fatalf("expected demote to friend confirmation, got: %s", res.Text)
	}
	if immune.ResolveRole(remainingID) == entity.RoleCollaborator {
		t.Fatalf("expected remainingID to be demoted from collaborator")
	}
	if !immune.HasPrivateAccess(remainingID) {
		t.Fatalf("expected remainingID to still have private access as friend")
	}

	// 10. 删除/撤销权限 revoke
	res, err = accessTool.Execute(partnerCtx, json.RawMessage(`{"action":"revoke","user_id":"`+remainingID+`"}`))
	if err != nil {
		t.Fatalf("revoke error: %v", err)
	}
	if !strings.Contains(res.Text, "已删除") {
		t.Fatalf("expected delete user confirmation, got: %s", res.Text)
	}
	if immune.HasPrivateAccess(remainingID) {
		t.Fatalf("expected remainingID to have private access revoked")
	}

	// 11. 非核心搭档权限隔离
	guestEv := &entity.SensoryEvent{
		Platform:  "qq",
		ChannelID: "guest_123",
		SenderID:  "guest_123",
		Role:      entity.RoleGuest,
	}
	guestCtx := entity.WithSensoryEvent(context.Background(), guestEv)
	res, err = accessTool.Execute(guestCtx, json.RawMessage(`{"action":"roles"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Text, "仅限核心搭档") {
		t.Fatalf("expected denial for non-partner, got: %s", res.Text)
	}
}
