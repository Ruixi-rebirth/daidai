package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"daidai/internal/entity"
	"daidai/internal/provider/llm"
)

func TestAGYWorker_MultiWorkspaceSessionIsolation(t *testing.T) {
	worker := NewAGYWorker("/default/dir", nil)

	channelID := "chan_qq_group_1"
	dirA := "/workspace/project_a"
	dirB := "/workspace/project_b"

	// 获取项目 A 的会话并模拟生成 ConversationID
	sessA := worker.GetSession(channelID, dirA)
	sessA.Lock()
	sessA.ConversationID = "conv_project_a_123"
	sessA.Unlock()

	// 切换到项目 B 执行
	sessB := worker.GetSession(channelID, dirB)
	sessB.Lock()
	if sessB.ConversationID != "" {
		t.Fatalf("expected project B to have empty initial ConversationID, got: %q", sessB.ConversationID)
	}
	sessB.ConversationID = "conv_project_b_456"
	sessB.Unlock()

	// 切回项目 A，验证项目 A 的会话仍完好无损地被保留
	sessAReload := worker.GetSession(channelID, dirA)
	sessAReload.Lock()

	if sessAReload.ConversationID != "conv_project_a_123" {
		t.Errorf("expected project A ConversationID to be preserved, got: %q", sessAReload.ConversationID)
	}
	sessAReload.Unlock()

	// 验证重置会话：重置 project A 的会话，project B 保持不受影响
	worker.ResetSession(channelID, dirA)
	sessAReset := worker.GetSession(channelID, dirA)
	sessAReset.Lock()
	if sessAReset.ConversationID != "" {
		t.Errorf("expected project A ConversationID to be reset to empty, got: %q", sessAReset.ConversationID)
	}
	sessAReset.Unlock()

	sessBCheck := worker.GetSession(channelID, dirB)
	sessBCheck.Lock()
	if sessBCheck.ConversationID != "conv_project_b_456" {
		t.Errorf("expected project B ConversationID to remain untouched, got: %q", sessBCheck.ConversationID)
	}
	sessBCheck.Unlock()
}

func TestAGYWorker_ResolveWorkDir(t *testing.T) {
	home, _ := os.UserHomeDir()
	worker := NewAGYWorker(home, nil)

	// 1. 空目录回退到 defaultDir
	wd, err := worker.resolveWorkDir("")
	if err != nil {
		t.Fatalf("resolveWorkDir empty failed: %v", err)
	}
	if wd != filepath.Clean(home) {
		t.Errorf("expected defaultDir %q, got %q", home, wd)
	}

	// 2. ~ 符号解析为用户家目录
	wdHome, errHome := worker.resolveWorkDir("~")
	if errHome != nil {
		t.Fatalf("resolveWorkDir ~ failed: %v", errHome)
	}
	if wdHome != filepath.Clean(home) {
		t.Errorf("expected home dir %q, got %q", home, wdHome)
	}

	// 3. 不存在的目录返回明确错误
	_, errInvalid := worker.resolveWorkDir("/nonexistent_path_for_test_123456")
	if errInvalid == nil {
		t.Fatalf("expected error for nonexistent workspace, got nil")
	}
}

func TestAGYWorker_NotLoggedIn_FailFast(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	worker := NewAGYWorker(tmpHome, nil)
	ticket := &entity.Ticket{
		Goal: "test task",
	}

	_, err := worker.ExecuteTicket(context.Background(), ticket)
	if !errors.Is(err, llm.ErrAGYNotLoggedIn) {
		t.Fatalf("expected ErrAGYNotLoggedIn, got %v", err)
	}
}
