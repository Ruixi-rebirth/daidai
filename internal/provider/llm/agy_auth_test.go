package llm

import (
	"context"
	"errors"
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
	if err := mgr.CheckAuth(); err == nil {
		t.Fatalf("expected error on checkAuth when not logged in")
	}
	if err := CheckAGYAuth(tokenFile); err == nil {
		t.Fatalf("expected error on CheckAGYAuth when not logged in")
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
	if err := mgr.CheckAuth(); err != nil {
		t.Fatalf("unexpected checkAuth err: %v", err)
	}
	if err := CheckAGYAuth(tokenFile); err != nil {
		t.Fatalf("unexpected CheckAGYAuth err: %v", err)
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

func TestLoginPTYCancellationTerminatesActiveSession(t *testing.T) {
	command := filepath.Join(t.TempDir(), "fake-auth")
	script := "#!/bin/sh\nprintf '%s\\n' 'https://accounts.google.com/o/oauth2/v2/auth?client_id=test-only'\nread code\nsleep 30\n"
	if err := os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	manager := NewAGYAuthManager(command, filepath.Join(t.TempDir(), "token"))
	defer manager.Cancel()
	startCtx, cancelStart := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelStart()
	link, err := manager.StartLogin(startCtx)
	if err != nil || !strings.HasPrefix(link, "https://accounts.google.com/") {
		t.Fatalf("login link not extracted: %s %v", link, err)
	}
	submitCtx, cancelSubmit := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelSubmit()
	_, err = manager.SubmitCode(submitCtx, "test-only-code")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("submission did not observe cancellation: %v", err)
	}
	manager.mu.Lock()
	active := manager.active
	manager.mu.Unlock()
	if active != nil {
		t.Fatal("cancelled authorization left child process/session active")
	}
}

func TestFindOAuthURLSupportsOfficialPathsOnly(t *testing.T) {
	for _, path := range []string{"/o/oauth2/auth?client_id=test", "/o/oauth2/v2/auth?client_id=test"} {
		expected := "https://accounts.google.com" + path
		if got := findAGYOAuthURL("登录链接：<" + expected + ">\n"); got != expected {
			t.Fatalf("missed valid OAuth link: %q", got)
		}
	}
	for _, input := range []string{"https://accounts.google.com.evil.test/o/oauth2/v2/auth?x=1", "https://accounts.google.com/o/oauth2/other?x=1", "https://accounts.google.com/o/oauth2/v2/auth", "https://evil.test/o/oauth2/v2/auth?x=1"} {
		if got := findAGYOAuthURL(input); got != "" {
			t.Fatalf("accepted non-authorization URL: %q", got)
		}
	}
}
