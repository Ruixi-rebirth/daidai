package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daidai/internal/entity"
	"daidai/internal/provider/llm"
)

func TestAGYWorker_MultiWorkspaceSessionIsolation(t *testing.T) {
	tmpDir := t.TempDir()
	dirA := filepath.Join(tmpDir, "project_a")
	dirB := filepath.Join(tmpDir, "project_b")
	if err := os.MkdirAll(dirA, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirB, 0755); err != nil {
		t.Fatal(err)
	}

	worker := NewAGYWorker(dirA, nil)

	channelID := "chan_qq_group_1"

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
	if err := worker.ResetSession(channelID, dirA); err != nil {
		t.Fatalf("ResetSession failed: %v", err)
	}
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
	tmpDir := t.TempDir()
	projDir := filepath.Join(tmpDir, "my_project")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	realProjDir, err := filepath.EvalSymlinks(projDir)
	if err != nil {
		t.Fatal(err)
	}

	worker := NewAGYWorker(projDir, nil)

	// 1. 空目录回退到 defaultDir (有效项目子目录)
	wd, err := worker.resolveWorkDir("")
	if err != nil {
		t.Fatalf("resolveWorkDir empty failed: %v", err)
	}
	if wd != filepath.Clean(realProjDir) {
		t.Errorf("expected defaultDir %q, got %q", realProjDir, wd)
	}

	// 2. 整个用户家目录必须被拦截拒绝 (防误操作与越权)
	home, _ := os.UserHomeDir()
	homeWorker := NewAGYWorker(home, nil)
	_, errHome := homeWorker.resolveWorkDir("")
	if errHome == nil || !strings.Contains(errHome.Error(), "不能是整个用户主目录") {
		t.Fatalf("expected error rejecting home dir as workspace, got: %v", errHome)
	}

	// 3. 文件系统根目录必须被拦截拒绝
	rootWorker := NewAGYWorker("/", nil)
	_, errRoot := rootWorker.resolveWorkDir("")
	if errRoot == nil || !strings.Contains(errRoot.Error(), "不能是文件系统根目录") {
		t.Fatalf("expected error rejecting root dir as workspace, got: %v", errRoot)
	}

	// 4. 不存在的目录返回明确错误
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
