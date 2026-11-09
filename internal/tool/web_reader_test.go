package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestReadLinkTool_Validation(t *testing.T) {
	tool := NewReadLinkTool()
	ctx := context.Background()

	// 1. 测试空参数
	res, err := tool.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error on empty url: %v", err)
	}
	if !strings.Contains(res.Text, "不能为空") {
		t.Errorf("expected empty warning, got: %s", res.Text)
	}

	// 2. 测试回环与私有地址 (SSRF 阻断)
	ssrfTargets := []string{
		"http://127.0.0.1:8080",
		"http://localhost:3000",
		"http://192.168.1.1/admin",
		"http://10.0.0.5/api",
		"http://169.254.169.254/latest/meta-data/",
	}

	for _, target := range ssrfTargets {
		payload, _ := json.Marshal(map[string]string{"url": target})
		res, err := tool.Execute(ctx, payload)
		if err != nil {
			t.Fatalf("unexpected execution error on SSRF test %q: %v", target, err)
		}
		if !strings.Contains(res.Text, "未通过") && !strings.Contains(res.Text, "forbidden") {
			t.Errorf("expected SSRF block for %q, got: %s", target, res.Text)
		}
	}
}

func TestExtractHTMLTitleAndText(t *testing.T) {
	rawHTML := `<!DOCTYPE html>
<html>
<head>
    <title>测试网页标题 - Test Page</title>
    <style>body { color: red; } .nav { display: none; }</style>
    <script>console.log("secret script execution");</script>
</head>
<body>
    <header><h1>Header Nav Title</h1></header>
    <nav><a href="/">Home</a><a href="/about">About</a></nav>
    <div class="content">
        <p>这是第一段有用的正文介绍内容。</p>
        <p>这是第二段详细说明，包含 &quot;HTML实体转义&quot; 与数据展示。</p>
    </div>
    <footer><p>Copyright 2026 Test</p></footer>
</body>
</html>`

	title, text := extractHTMLTitleAndText(rawHTML)

	if title != "测试网页标题 - Test Page" {
		t.Errorf("expected title '测试网页标题 - Test Page', got %q", title)
	}

	if strings.Contains(text, "secret script execution") {
		t.Errorf("text should not contain script content")
	}
	if strings.Contains(text, "color: red") {
		t.Errorf("text should not contain style content")
	}
	if !strings.Contains(text, "第一段有用的正文介绍内容") {
		t.Errorf("text missing main content: %q", text)
	}
	if !strings.Contains(text, `"HTML实体转义"`) {
		t.Errorf("html entity should be unescaped, got: %q", text)
	}
}
