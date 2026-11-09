package tool

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"daidai/internal/provider/llm"
)

func TestModelSwitchTool_Flow(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "active_mind.json")

	api := llm.NewOpenAIProvider(llm.OpenAIConfig{
		BaseURL: "https://api.example.com",
		APIKey:  "",
		Model:   "deepseek-chat",
	})
	agy := llm.NewAGYMindProvider(llm.AGYMindConfig{
		Model:  "gemini-3.8-flash",
		Effort: "medium",
	})
	router := llm.NewMindRouter(llm.MindRouterConfig{
		StatePath:       statePath,
		FallbackChannel: "API",
	})

	router.RegisterChannel(llm.ChannelRegistration{
		Channel:  "API",
		Provider: api,
	})
	router.RegisterChannel(llm.ChannelRegistration{
		Channel:  "AGY",
		Provider: agy,
		Models: llm.ParseCLIModelsOutput(
			"gemini-3.8-flash-low   Gemini 3.8 Flash (Low)\n"+
				"claude-opus-5-5-high   Claude Opus 5.5 (High)\n"+
				"claude-sonnet-5-5-medium Claude Sonnet 5.5 (Medium)\n",
			"AGY",
		),
	})

	switchTool := NewModelSwitchTool(router, nil)

	// 1. 查询当前心智
	currRes, err := switchTool.Execute(context.Background(), json.RawMessage(`{"action":"current"}`))
	if err != nil {
		t.Fatalf("failed to query current: %v", err)
	}
	if !strings.Contains(currRes.Text, "deepseek-chat") || !strings.Contains(currRes.Text, "API 通道") {
		t.Fatalf("current output unexpected: %s", currRes.Text)
	}

	// 2. 列出模型池
	listRes, err := switchTool.Execute(context.Background(), json.RawMessage(`{"action":"list"}`))
	if err != nil {
		t.Fatalf("failed to list models: %v", err)
	}
	if !strings.Contains(listRes.Text, "API 通道") || !strings.Contains(listRes.Text, "AGY 通道") {
		t.Fatalf("list output unexpected: %s", listRes.Text)
	}

	// 3. 歧义查询 claude
	ambRes, err := switchTool.Execute(context.Background(), json.RawMessage(`{"action":"switch","model":"claude"}`))
	if err != nil {
		t.Fatalf("failed to handle ambiguity: %v", err)
	}
	if !strings.Contains(ambRes.Text, "二义性") {
		t.Fatalf("expected ambiguity notice, got: %s", ambRes.Text)
	}

	// 4. 精确切换至 AGY claude-sonnet-5-5 思考强度 medium
	okRes, err := switchTool.Execute(context.Background(), json.RawMessage(`{"action":"switch","model":"claude-sonnet","effort":"medium"}`))
	if err != nil {
		t.Fatalf("failed to switch: %v", err)
	}
	if !strings.Contains(okRes.Text, "切换成功") || !strings.Contains(okRes.Text, "Claude Sonnet") {
		t.Fatalf("switch output unexpected: %s", okRes.Text)
	}

	// 5. 再次查询当前，验证 effort 正确呈现
	currRes2, _ := switchTool.Execute(context.Background(), json.RawMessage(`{}`))
	if !strings.Contains(currRes2.Text, "Claude Sonnet") || !strings.Contains(currRes2.Text, "思考强度：medium") {
		t.Fatalf("current output after switch unexpected: %s", currRes2.Text)
	}
}
