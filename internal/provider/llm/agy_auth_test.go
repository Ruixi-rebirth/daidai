package llm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAGYAuthManager_Status(t *testing.T) {
	tmpDir := t.TempDir()
	tokenFile := filepath.Join(tmpDir, "antigravity-oauth-token")

	mgr := NewAGYAuthManager("echo", tokenFile)

	// 1. 未生成凭据文件
	s1 := mgr.GetStatus()
	if s1.IsLoggedIn {
		t.Fatalf("expected not logged in, got: %+v", s1)
	}

	// 2. 模拟写入有效凭据
	mockData := `{"token":{"access_token":"ya29.test","expiry":"2026-12-01T10:00:00Z"},"id_token_email":"user@example.com"}`
	if err := os.WriteFile(tokenFile, []byte(mockData), 0600); err != nil {
		t.Fatalf("failed to write mock token: %v", err)
	}

	s2 := mgr.GetStatus()
	if !s2.IsLoggedIn || s2.Email != "user@example.com" {
		t.Fatalf("expected logged in with email, got: %+v", s2)
	}
	expectedTime, _ := time.Parse(time.RFC3339, "2026-12-01T10:00:00Z")
	if !s2.Expiry.Equal(expectedTime) {
		t.Fatalf("expected expiry %v, got %v", expectedTime, s2.Expiry)
	}

	// 3. 模拟文件内容损坏
	_ = os.WriteFile(tokenFile, []byte("invalid json"), 0600)
	s3 := mgr.GetStatus()
	if s3.IsLoggedIn {
		t.Fatalf("expected not logged in on corrupted file, got: %+v", s3)
	}
}

func TestAGYAuthManager_CancelIdempotent(t *testing.T) {
	mgr := NewAGYAuthManager("echo", "")
	mgr.Cancel()
	mgr.Cancel()
}

func TestFormatAuthStatus(t *testing.T) {
	tmpDir := t.TempDir()
	tokenFile := filepath.Join(tmpDir, "antigravity-oauth-token")
	mgr := NewAGYAuthManager("echo", tokenFile)

	// 登录前格式化
	st1 := mgr.GetStatus()
	if st1.IsLoggedIn {
		t.Fatalf("unexpected login state")
	}

	// 模拟已登录
	mockData := `{"token":{"access_token":"ya29.test","expiry":"2026-12-01T10:00:00Z"},"id_token_email":"testuser@gmail.com"}`
	_ = os.WriteFile(tokenFile, []byte(mockData), 0600)
	st2 := mgr.GetStatus()
	if !st2.IsLoggedIn || !strings.Contains(st2.Email, "testuser") {
		t.Fatalf("unexpected logged in state: %+v", st2)
	}

	// 验证注销
	if err := mgr.Logout(); err != nil {
		t.Fatalf("logout failed: %v", err)
	}
	st3 := mgr.GetStatus()
	if st3.IsLoggedIn {
		t.Fatalf("expected logged out after Logout(), got logged in: %+v", st3)
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Fatalf("expected token file to be deleted")
	}
}
