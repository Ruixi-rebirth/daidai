package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daidai/internal/provider/llm"
)

func TestReadLinkPreservesURLAndTaskThroughCLIBoundary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	token := llm.DefaultAGYTokenPath()
	if err := os.MkdirAll(filepath.Dir(token), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte(`{"id_token_email":"test@example.com","token":{"refresh_token":"test-only"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(t.TempDir(), "prompt.txt")
	t.Setenv("DAIDAI_TEST_WEB_PROMPT", capture)
	command := filepath.Join(t.TempDir(), "fake-agy")
	script := "#!/bin/sh\nprintf '%s' \"$2\" > \"$DAIDAI_TEST_WEB_PROMPT\"\nprintf '%s' '{\"status\":\"SUCCESS\",\"response\":\"网页已读取，回答完整。\"}'\n"
	if err := os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	url := "https://example.com/" + strings.Repeat("path", 400) + "?final=preserved"
	task := strings.Repeat("总结内容并解释，", 100) + "最后请回答这个问题。"
	raw, err := json.Marshal(map[string]string{"url": url, "task": task})
	if err != nil {
		t.Fatal(err)
	}
	reader := NewReadLinkToolWithCommand(nil, command)
	result, err := reader.Execute(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	_, encoded, ok := strings.Cut(string(prompt), "用户请求 JSON：")
	if !ok {
		t.Fatal("missing structured request")
	}
	var request webRequest
	if err := json.Unmarshal([]byte(encoded), &request); err != nil {
		t.Fatal(err)
	}
	if request.URL != url || request.Task != task || request.Query != "" {
		t.Fatal("URL or task was truncated or converted into search terms")
	}
	if !strings.Contains(result.Text, "回答完整") || strings.Contains(result.Text, url) || strings.Contains(result.Text, task) {
		t.Fatalf("tool response echoes full input instead of result: %s", result.Text)
	}
}
