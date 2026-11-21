package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"daidai/internal/provider/llm"
)

func TestWebSearchTool_NotLoggedIn_FailFast(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	searchTool := NewWebSearchTool(nil)

	rawArgs, _ := json.Marshal(map[string]any{
		"query": "最新科技新闻",
	})

	res, err := searchTool.Execute(context.Background(), rawArgs)
	if err != nil {
		t.Fatalf("unexpected execution error: %v", err)
	}

	if !strings.Contains(res.Text, "尚未登录") || !strings.Contains(res.Text, "/agy login") {
		t.Fatalf("expected fail-fast login error message, got: %s", res.Text)
	}
}

func TestWebSearchResultDistinguishesPartialAndFailedOutput(t *testing.T) {
	const body = "网页片段"
	partial := webSearchResultText(llm.AGYResult{Status: "SUCCESS", FinishedReason: "max_tokens", Response: body})
	if !strings.Contains(partial, "已截断") || !strings.Contains(partial, body) {
		t.Fatalf("partial result lost: %q", partial)
	}
	failed := webSearchResultText(llm.AGYResult{Status: "SUCCESS", Error: "lookup failed", Response: body})
	if strings.Contains(failed, body) || !strings.Contains(failed, "未返回有效结果") {
		t.Fatalf("failed result presented as evidence: %q", failed)
	}
}
