package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"daidai/internal/core"
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
	script := `#!/bin/sh
printf '%s' "$2" > "$DAIDAI_TEST_WEB_PROMPT"
printf '%s' '{"status":"SUCCESS","response":"{\"read\":true,\"answer\":\"网页已读取，回答完整。\"}"}'
`
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

func TestExtractHTMLContentSkipsHiddenErrorPlaceholders(t *testing.T) {
	const visible = "Pinned repository: this visible project description should remain in the page output."
	source := `<main><p>` + visible + `</p><div hidden data-show-on-error><p>There was an error while loading.</p></div></main>`
	text, err := extractHTMLContent([]byte(source))
	if err != nil || !strings.Contains(text, visible) || strings.Contains(text, "error while loading") {
		t.Fatalf("extracted wrong page content: %q, %v", text, err)
	}
}

func TestReadPageReturnsAGYWhenSuccessful(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	directCalled := false
	result, err := readPageWithFallback(ctx, time.Second, func(ctx context.Context) (*core.ToolResult, error) {
		return &core.ToolResult{Text: "AGY 正文"}, nil
	}, func(context.Context) (*core.ToolResult, error) {
		directCalled = true
		return &core.ToolResult{Text: "Go 正文"}, nil
	})
	if err != nil || result == nil || result.Text != "AGY 正文" {
		t.Fatalf("expected AGY result, got: %v, %v", result, err)
	}
	if directCalled {
		t.Fatal("direct fallback was called even though AGY succeeded")
	}
}

func TestReadPageFallsBackOnAGYTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := readPageWithFallback(ctx, 10*time.Millisecond, func(ctx context.Context) (*core.ToolResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, func(context.Context) (*core.ToolResult, error) {
		return &core.ToolResult{Text: "Go 兜底正文"}, nil
	})
	if err != nil || result == nil || result.Text != "Go 兜底正文" {
		t.Fatalf("expected fallback result after AGY timeout, got: %v, %v", result, err)
	}
}

func TestReadPageFallsBackImmediatelyOnAGYFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := readPageWithFallback(ctx, time.Hour, func(context.Context) (*core.ToolResult, error) {
		return nil, errors.New("AGY could not access the page")
	}, func(context.Context) (*core.ToolResult, error) {
		return &core.ToolResult{Text: "Go 正文"}, nil
	})
	if err != nil || result == nil || result.Text != "Go 正文" {
		t.Fatalf("fallback did not start after AGY failed: %v, %v", result, err)
	}
}
