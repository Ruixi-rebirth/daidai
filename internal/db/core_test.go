package db

import (
	"context"
	"testing"
	"time"
)

func TestManager_MessagesAndFactMutations(t *testing.T) {
	mgr := NewManager(t.TempDir())
	defer mgr.Close()

	ctx := context.Background()

	// 1. 验证消息写入与正序读取（零前缀）
	_, err := mgr.LogMessage(ctx, "qq", "group", "900100", "10001", "小明", "第一条群消息", false, "", "")
	if err != nil {
		t.Fatalf("LogMessage failed: %v", err)
	}
	_, err = mgr.LogMessage(ctx, "qq", "group", "900100", "daidai", "呆呆", "收到啦", true, "10001", "")
	if err != nil {
		t.Fatalf("LogMessage bot failed: %v", err)
	}

	msgs, err := mgr.RecentMessages(ctx, "qq", "group", "900100", 10)
	if err != nil {
		t.Fatalf("RecentMessages failed: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Text != "第一条群消息" || msgs[0].IsBot || !msgs[0].IsGroup {
		t.Errorf("unexpected first message: %+v", msgs[0])
	}
	if msgs[1].Text != "收到啦" || !msgs[1].IsBot {
		t.Errorf("unexpected second message: %+v", msgs[1])
	}

	// 验证重置会话切断工作记忆 (RecentMessages 不再读取游标前内容，但底层日志保留)
	time.Sleep(5 * time.Millisecond)
	err = mgr.ResetSessionHistory("qq", "900100")
	if err != nil {
		t.Fatalf("ResetSessionHistory failed: %v", err)
	}
	msgsReset, err := mgr.RecentMessages(ctx, "qq", "group", "900100", 10)
	if err != nil || len(msgsReset) != 0 {
		t.Fatalf("expected 0 messages after reset, got %d (err: %v)", len(msgsReset), err)
	}

	// 重置后新发的消息能够正常读取
	time.Sleep(5 * time.Millisecond)
	_, _ = mgr.LogMessage(ctx, "qq", "group", "900100", "10001", "小明", "重开后的第一句话", false, "", "")
	msgsNew, err := mgr.RecentMessages(ctx, "qq", "group", "900100", 10)
	if err != nil || len(msgsNew) != 1 || msgsNew[0].Text != "重开后的第一句话" {
		t.Fatalf("expected new message after reset, got %+v (err: %v)", msgsNew, err)
	}

	// 2. 验证事实 ADD、重复 ADD 幂等去重、UPDATE 覆盖与 DELETE 废弃
	err = mgr.ApplyFactMutations(ctx, "qq", "10001", []FactMutation{
		{Action: "ADD", Fact: "住在上海", Quote: "我现在住在上海"},
		{Action: "ADD", Fact: "住在上海", Quote: "我现在住在上海"}, // 重复事实应自动去重
		{Action: "ADD", Fact: "养了一只狸花猫", Quote: "我家有只狸花猫"},
	})
	if err != nil {
		t.Fatalf("ApplyFactMutations ADD failed: %v", err)
	}

	facts, err := mgr.ListActiveFacts(ctx, "qq", "10001")
	if err != nil || len(facts) != 2 {
		t.Fatalf("expected 2 deduplicated active facts, got %d (err: %v)", len(facts), err)
	}
	origFirstID := facts[0].ID

	// UPDATE 第 1 条事实（搬家去杭州，保持原 ID 稳定），DELETE 第 2 条事实
	err = mgr.ApplyFactMutations(ctx, "qq", "10001", []FactMutation{
		{Action: "UPDATE", FactID: facts[0].ID, Fact: "搬到了杭州", Quote: "我上个月搬到杭州了"},
		{Action: "DELETE", FactID: facts[1].ID, Quote: "猫送回老家给爸妈养了"},
	})
	if err != nil {
		t.Fatalf("ApplyFactMutations UPDATE/DELETE failed: %v", err)
	}

	activeAfter, err := mgr.ListActiveFacts(ctx, "qq", "10001")
	if err != nil || len(activeAfter) != 1 {
		t.Fatalf("expected 1 active fact after UPDATE+DELETE, got %d (err: %v)", len(activeAfter), err)
	}
	if activeAfter[0].ID != origFirstID || activeAfter[0].Content != "搬到了杭州" {
		t.Errorf("expected updated fact ID=%d '搬到了杭州', got ID=%d %q", origFirstID, activeAfter[0].ID, activeAfter[0].Content)
	}

	// 3. 验证动态工作空间持久化与活跃切换
	err = mgr.SetActiveWorkspace("qq", "group_100", "/home/ruixi/projectA", "projectA")
	if err != nil {
		t.Fatalf("SetActiveWorkspace A failed: %v", err)
	}

	wsCurr, err := mgr.GetActiveWorkspace("qq", "group_100")
	if err != nil || wsCurr == nil {
		t.Fatalf("GetActiveWorkspace A failed: %v", err)
	}
	if wsCurr.Path != "/home/ruixi/projectA" || wsCurr.ProjectName != "projectA" || !wsCurr.IsCurrent {
		t.Errorf("unexpected active workspace A: %+v", wsCurr)
	}

	// 切换到 projectB
	err = mgr.SetActiveWorkspace("qq", "group_100", "/home/ruixi/projectB", "projectB")
	if err != nil {
		t.Fatalf("SetActiveWorkspace B failed: %v", err)
	}

	wsB, err := mgr.GetActiveWorkspace("qq", "group_100")
	if err != nil || wsB == nil {
		t.Fatalf("GetActiveWorkspace B failed: %v", err)
	}
	if wsB.Path != "/home/ruixi/projectB" || !wsB.IsCurrent {
		t.Errorf("expected active workspace to be projectB, got: %+v", wsB)
	}

	// 验证列表
	list, err := mgr.ListChannelWorkspaces("qq", "group_100")
	if err != nil || len(list) != 2 {
		t.Fatalf("expected 2 workspaces in history, got %d (err: %v)", len(list), err)
	}
	if !list[0].IsCurrent || list[0].Path != "/home/ruixi/projectB" {
		t.Errorf("expected first workspace in list to be current projectB, got: %+v", list[0])
	}
	if list[1].IsCurrent || list[1].Path != "/home/ruixi/projectA" {
		t.Errorf("expected second workspace in list to be inactive projectA, got: %+v", list[1])
	}
}

func TestPrivateFactSourceScope(t *testing.T) {
	m := NewManager(t.TempDir())
	defer m.Close()
	ctx := context.Background()
	for _, tc := range []struct {
		name, user, chat, conv string
		valid                  bool
	}{
		{"own private", "owner", "private", "owner", true},
		{"other user", "other", "private", "other", false},
		{"group source", "owner", "group", "group1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := m.LogMessage(ctx, "qq", tc.chat, tc.conv, tc.user, "", "我喜欢阅读", false, "", "")
			if err != nil {
				t.Fatal(err)
			}
			err = m.ApplyFactMutations(ctx, "qq", "owner", []FactMutation{{Action: "ADD", Fact: tc.name, SourceMessageID: id, ScopeType: "global"}})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}
