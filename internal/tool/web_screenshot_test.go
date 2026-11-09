package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestScreenshotTool_Validation(t *testing.T) {
	tool := NewScreenshotTool()
	ctx := context.Background()

	// 1. 空参数
	res, err := tool.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Text, "不能为空") {
		t.Errorf("expected empty URL warning, got: %s", res.Text)
	}

	// 2. SSRF 拦截
	badTargets := []string{
		"http://127.0.0.1:2375",
		"http://localhost",
		"http://10.100.1.1",
		"http://172.16.0.1",
	}

	for _, target := range badTargets {
		payload, _ := json.Marshal(map[string]string{"url": target})
		res, err := tool.Execute(ctx, payload)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", target, err)
		}
		if !strings.Contains(res.Text, "未通过") && !strings.Contains(res.Text, "forbidden") {
			t.Errorf("expected SSRF block for %q, got: %s", target, res.Text)
		}
	}
}

func TestScreenshotTool_Metadata(t *testing.T) {
	tool := NewScreenshotTool()
	if tool.Name() != "screenshot_web" {
		t.Errorf("expected name 'screenshot_web', got %s", tool.Name())
	}
	params := tool.Parameters()
	req, ok := params["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "url" {
		t.Errorf("expected required field ['url'], got %v", params["required"])
	}
}
