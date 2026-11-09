package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daidai/internal/db"
	"daidai/internal/entity"
)

func TestWorkspaceTool_CurrentListAndSwitch(t *testing.T) {
	tempBase := t.TempDir()
	dbMgr := db.NewManager(filepath.Join(tempBase, "db"))
	defer dbMgr.Close()

	projA := filepath.Join(tempBase, "project-1")
	projB := filepath.Join(tempBase, "project-2")
	sandbox := filepath.Join(tempBase, "sandbox")
	_ = os.MkdirAll(projA, 0755)
	_ = os.WriteFile(filepath.Join(projA, "go.mod"), []byte("module p1\n"), 0644)
	_ = os.MkdirAll(projB, 0755)
	_ = os.WriteFile(filepath.Join(projB, "Cargo.toml"), []byte("[package]\nname = \"p2\"\n"), 0644)
	_ = os.MkdirAll(sandbox, 0755)

	immune := entity.NewImmuneSystem("partner_101")
	wm := entity.NewWorkspaceManager(sandbox, dbMgr)
	_, _ = wm.Switch("chan_qq_1", projA)
	wsTool := NewWorkspaceTool(wm, immune)

	partnerEv := &entity.SensoryEvent{
		ChannelID: "chan_qq_1",
		SenderID:  "partner_101",
		Role:      entity.RolePartner,
	}
	partnerCtx := entity.WithSensoryEvent(context.Background(), partnerEv)

	guestEv := &entity.SensoryEvent{
		ChannelID: "chan_qq_1",
		SenderID:  "guest_999",
		Role:      entity.RoleGuest,
	}
	guestCtx := entity.WithSensoryEvent(context.Background(), guestEv)

	// 1. 查询当前
	resCurr, err := wsTool.Execute(partnerCtx, json.RawMessage(`{"action":"current"}`))
	if err != nil {
		t.Fatalf("execute current failed: %v", err)
	}
	if !strings.Contains(resCurr.Text, "project-1") {
		t.Errorf("expected current to be project-1, got: %s", resCurr.Text)
	}

	// 2. 访客尝试切换应被权限拦截
	resGuest, _ := wsTool.Execute(guestCtx, json.RawMessage(`{"action":"switch","target":"project-2"}`))
	if !strings.Contains(resGuest.Text, "权限不足") {
		t.Errorf("expected guest to be blocked, got: %s", resGuest.Text)
	}

	// 3. 搭档切换并激活 project-2
	resSwitch, err := wsTool.Execute(partnerCtx, json.RawMessage(`{"action":"switch","target":"`+projB+`"}`))
	if err != nil {
		t.Fatalf("execute switch failed: %v", err)
	}
	if !strings.Contains(resSwitch.Text, "project-2") {
		t.Errorf("expected switched to project-2, got: %s", resSwitch.Text)
	}

	// 4. 列出纳管书签清单
	resList, err := wsTool.Execute(partnerCtx, json.RawMessage(`{"action":"list"}`))
	if err != nil {
		t.Fatalf("execute list failed: %v", err)
	}
	if !strings.Contains(resList.Text, "project-1") || !strings.Contains(resList.Text, "project-2") {
		t.Errorf("expected list to contain both projects, got: %s", resList.Text)
	}

	// 5. 注销 project-1 书签
	resRm, err := wsTool.Execute(partnerCtx, json.RawMessage(`{"action":"remove","target":"project-1"}`))
	if err != nil {
		t.Fatalf("execute remove failed: %v", err)
	}
	if !strings.Contains(resRm.Text, "已成功注销") {
		t.Errorf("expected remove success, got: %s", resRm.Text)
	}

	resListAfter, _ := wsTool.Execute(partnerCtx, json.RawMessage(`{"action":"list"}`))
	if strings.Contains(resListAfter.Text, "project-1") {
		t.Errorf("expected project-1 to be removed from list, got: %s", resListAfter.Text)
	}

	// 6. 测试退出工程回到沙盒
	resExit, err := wsTool.Execute(partnerCtx, json.RawMessage(`{"action":"exit"}`))
	if err != nil {
		t.Fatalf("execute exit failed: %v", err)
	}
	if !strings.Contains(resExit.Text, "沙盒演练态") {
		t.Errorf("expected exit to return to sandbox, got: %s", resExit.Text)
	}

	// 7. 测试清空沙盒
	_ = os.WriteFile(filepath.Join(sandbox, "scratch.txt"), []byte("temp"), 0644)
	resClean, err := wsTool.Execute(partnerCtx, json.RawMessage(`{"action":"clean"}`))
	if err != nil {
		t.Fatalf("execute clean failed: %v", err)
	}
	if !strings.Contains(resClean.Text, "已清空") {
		t.Errorf("expected clean to succeed, got: %s", resClean.Text)
	}
	if _, err := os.Stat(filepath.Join(sandbox, "scratch.txt")); !os.IsNotExist(err) {
		t.Errorf("expected scratch.txt to be removed")
	}
}
