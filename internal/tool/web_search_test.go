package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
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
