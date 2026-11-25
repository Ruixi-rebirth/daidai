package llm

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAGYCredentialGuardDetectsRawAndBase64Secrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tokenPath := DefaultAGYTokenPath()
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0700); err != nil {
		t.Fatal(err)
	}
	rawToken := `{"id_token_email":"owner-secret@example.com","token":{"access_token":"ya29.super_secret_access_token","refresh_token":"1//super_secret_refresh_token"}}`
	if err := os.WriteFile(tokenPath, []byte(rawToken), 0600); err != nil {
		t.Fatal(err)
	}

	_, guard, err := LoadAGYCredentialGuard()
	if err != nil {
		t.Fatalf("LoadAGYCredentialGuard failed: %v", err)
	}
	if !guard.ContainsSecret("leaked: 1//super_secret_refresh_token") {
		t.Fatal("expected raw refresh token to be caught")
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("ya29.super_secret_access_token"))
	if !guard.ContainsSecret("b64=" + encoded) {
		t.Fatal("expected base64 access token to be caught")
	}
	if !guard.ContainsSecret("email: owner-secret@example.com") {
		t.Fatal("expected owner OAuth email to be caught")
	}
	if guard.ContainsSecret("普通闲聊回复 676767 笑死") {
		t.Fatal("ordinary reply must not trigger credential guard")
	}
}

func TestIsolatedAGYCommandHidesHostFilesAndSecrets(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed in test environment")
	}

	hostHome := t.TempDir()
	t.Setenv("HOME", hostHome)
	t.Setenv("SUPER_SECRET_HOST_ENV", "top-secret-env-value")

	// Create sensitive host files in $HOME that must NEVER be visible inside bwrap.
	hostSecretFile := filepath.Join(hostHome, "private_ssh_key.pem")
	if err := os.WriteFile(hostSecretFile, []byte("PRIVATE_KEY_MATERIAL"), 0600); err != nil {
		t.Fatal(err)
	}
	tokenPath := DefaultAGYTokenPath()
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(`{"id_token_email":"owner@example.com","token":{"refresh_token":"1//refresh_secret_value"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Also create a fake history file next to the token to ensure sibling files in ~/.gemini/antigravity-cli are hidden.
	historyFile := filepath.Join(filepath.Dir(tokenPath), "history.jsonl")
	if err := os.WriteFile(historyFile, []byte("PRIVATE_CLI_HISTORY"), 0600); err != nil {
		t.Fatal(err)
	}

	// Probe script simulating a compromised/injected CLI trying to read host secrets & env.
	probeBin := filepath.Join(t.TempDir(), "probe-cli")
	script := "#!/bin/sh\n" +
		"echo \"HOME=$HOME\"\n" +
		"echo \"ENV=$SUPER_SECRET_HOST_ENV\"\n" +
		"if [ -e \"" + hostSecretFile + "\" ]; then echo \"LEAK_HOST_HOME\"; fi\n" +
		"if [ -e \"/run/secrets\" ]; then echo \"LEAK_RUN_SECRETS\"; fi\n" +
		"if [ -e \"/var/lib/daidai\" ]; then echo \"LEAK_VAR_DAIDAI\"; fi\n" +
		"if [ -e \"$HOME/.gemini/antigravity-cli/history.jsonl\" ]; then echo \"LEAK_CLI_HISTORY\"; fi\n" +
		"if [ -f \"$HOME/.gemini/antigravity-cli/antigravity-oauth-token\" ]; then echo \"TOKEN_READY\"; fi\n"
	if err := os.WriteFile(probeBin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}

	cmd, cleanup, _, err := IsolatedAGYCommand(context.Background(), probeBin, nil, "")
	if err != nil {
		t.Fatalf("IsolatedAGYCommand setup failed: %v", err)
	}
	defer cleanup()

	outBytes, runErr := cmd.CombinedOutput()
	out := string(outBytes)
	if runErr != nil {
		t.Fatalf("sandboxed probe failed (%v): %s", runErr, out)
	}
	if !strings.Contains(out, "HOME=/sandbox/home") {
		t.Fatalf("expected isolated HOME=/sandbox/home, got:\n%s", out)
	}
	if !strings.Contains(out, "ENV=\n") {
		t.Fatalf("expected host environment variables to be cleared, got:\n%s", out)
	}
	for _, leak := range []string{"LEAK_HOST_HOME", "LEAK_RUN_SECRETS", "LEAK_VAR_DAIDAI", "LEAK_CLI_HISTORY"} {
		if strings.Contains(out, leak) {
			t.Fatalf("sandbox leaked host resource %s:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "TOKEN_READY") {
		t.Fatalf("expected isolated token copy to be mounted for CLI auth, got:\n%s", out)
	}
}
