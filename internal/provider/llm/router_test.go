package llm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daidai/internal/core"
)

type mockCustomAgentProvider struct {
	model  string
	effort string
}

func (m *mockCustomAgentProvider) Name() string {
	return "mock-agent (" + m.model + ")"
}

func (m *mockCustomAgentProvider) Chat(ctx context.Context, messages []core.ChatMessage, opts *core.ChatOptions) (string, error) {
	return "custom-agent-response", nil
}

func (m *mockCustomAgentProvider) ChatWithTools(ctx context.Context, messages []core.ChatMessage, tools []core.ToolDefinition, opts *core.ChatOptions) (*core.ChatResponse, error) {
	return &core.ChatResponse{Content: "custom-agent-tool-response"}, nil
}

func (m *mockCustomAgentProvider) SetModel(model string) {
	m.model = model
}

func (m *mockCustomAgentProvider) SetEffort(effort string) {
	m.effort = effort
}

func testAGYModelsOutput() string {
	return "gemini-4-argon-medium     Gemini 4 Argon (Medium)\n" +
		"gemini-4-argon-high       Gemini 4 Argon (High)\n" +
		"gemini-4-argon-low        Gemini 4 Argon (Low)\n" +
		"gemini-3.8-flash-high     Gemini 3.8 Flash (High)\n" +
		"gemini-3.8-flash-medium   Gemini 3.8 Flash (Medium)\n" +
		"gemini-3.8-flash-low      Gemini 3.8 Flash (Low)\n" +
		"claude-opus-5-5-low       Claude Opus 5.5 (Low)\n" +
		"claude-opus-5-5-medium    Claude Opus 5.5 (Medium)\n" +
		"claude-opus-5-5-high      Claude Opus 5.5 (High)\n" +
		"claude-sonnet-5-5-low     Claude Sonnet 5.5 (Low)\n" +
		"claude-sonnet-5-5-medium  Claude Sonnet 5.5 (Medium)\n" +
		"claude-sonnet-5-5-high    Claude Sonnet 5.5 (High)\n" +
		"gpt-oss-120b-medium       GPT-OSS 120B (Medium)\n"
}

func TestParseCLIModelsOutput_DynamicAggregation(t *testing.T) {
	models := ParseCLIModelsOutput(testAGYModelsOutput(), "AGY")
	if len(models) != 5 {
		t.Fatalf("expected 5 aggregated models, got %d: %+v", len(models), models)
	}

	if models[0].ID != "gemini-4-argon" || models[0].Name != "Gemini 4 Argon" || len(models[0].SupportedEfforts) != 3 {
		t.Errorf("unexpected first model: %+v", models[0])
	}
	if models[0].DefaultEffort != "medium" {
		t.Errorf("expected default effort 'medium', got %q", models[0].DefaultEffort)
	}
}

func TestMindRouter_SwitchAndAmbiguity(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "active_mind.json")

	api := NewOpenAIProvider(OpenAIConfig{
		BaseURL: "https://api.example.com",
		APIKey:  "",
		Model:   "deepseek-chat",
	})
	agy := NewAGYMindProvider(AGYMindConfig{
		Model:  "gemini-3.8-flash",
		Effort: "medium",
	})

	router := NewMindRouter(MindRouterConfig{
		StatePath:       statePath,
		FallbackChannel: "API",
	})

	apiModels := []ModelInfo{
		{ID: "deepseek-chat", Name: "deepseek-chat"},
		{ID: "deepseek-reasoner", Name: "deepseek-reasoner"},
	}
	agyModels := ParseCLIModelsOutput(testAGYModelsOutput(), "AGY")

	router.RegisterChannel(ChannelRegistration{
		Channel:  "API",
		Provider: api,
		Models:   apiModels,
	})
	router.RegisterChannel(ChannelRegistration{
		Channel:  "AGY",
		Provider: agy,
		Models:   agyModels,
	})

	// 1. 验证默认初始化状态
	curr := router.CurrentMind()
	if curr.Channel != "API" || curr.Model != "deepseek-chat" {
		t.Fatalf("unexpected initial state: %+v", curr)
	}

	// 2. 切换至 API Reasoner
	res, err := router.SwitchMind(SwitchRequest{Target: "deepseek-reasoner"})
	if err != nil || res.Status != "success" {
		t.Fatalf("expected success switching to deepseek-reasoner, got: %+v, err: %v", res, err)
	}
	if router.CurrentMind().Model != "deepseek-reasoner" || router.CurrentMind().Channel != "API" {
		t.Fatalf("current mind mismatch: %+v", router.CurrentMind())
	}

	// 3. 歧义检测：搜索 "claude" 命中 opus 与 sonnet
	res, err = router.SwitchMind(SwitchRequest{Target: "claude"})
	if err != nil {
		t.Fatalf("unexpected error on ambiguous query: %v", err)
	}
	if res.Status != "ambiguous" || len(res.Candidates) < 2 {
		t.Fatalf("expected ambiguous status with >= 2 candidates, got: %+v", res)
	}

	// 4. 精确切换至 AGY claude-opus-5-5，指定思考强度 high
	res, err = router.SwitchMind(SwitchRequest{Target: "claude-opus", Effort: "high"})
	if err != nil || res.Status != "success" {
		t.Fatalf("expected success switching to opus, got: %+v, err: %v", res, err)
	}
	if router.CurrentMind().Channel != "AGY" || router.CurrentMind().Model != "claude-opus-5-5" || router.CurrentMind().Effort != "high" {
		t.Fatalf("unexpected state after switching to opus: %+v", router.CurrentMind())
	}

	// 5. 复合字符串解析：从 "gemini-4-argon-low" 自动解析出模型与 low 思考强度
	res, err = router.SwitchMind(SwitchRequest{Target: "gemini-4-argon-low"})
	if err != nil || res.Status != "success" {
		t.Fatalf("expected success with suffix effort: %+v", res)
	}
	if router.CurrentMind().Model != "gemini-4-argon" || router.CurrentMind().Effort != "low" {
		t.Fatalf("unexpected parsed state: %+v", router.CurrentMind())
	}

	// 6. 单独调整思考强度（验证合法档位成功、非法档位拦截）
	res, err = router.SwitchMind(SwitchRequest{Effort: "high"})
	if err != nil || res.Status != "success" {
		t.Fatalf("expected success adjusting effort: %+v", res)
	}
	if router.CurrentMind().Effort != "high" {
		t.Fatalf("expected effort to be high, got: %s", router.CurrentMind().Effort)
	}
	invalidRes, _ := router.SwitchMind(SwitchRequest{Effort: "max"})
	if invalidRes.Status != "error" {
		t.Fatalf("expected error when setting unsupported effort 'max' on gemini-4-argon, got: %+v", invalidRes)
	}

	// 7. 持久化验证：重新实例化 Router 应恢复状态
	routerRestored := NewMindRouter(MindRouterConfig{
		StatePath:       statePath,
		FallbackChannel: "API",
	})
	routerRestored.RegisterChannel(ChannelRegistration{
		Channel:  "API",
		Provider: api,
		Models:   apiModels,
	})
	routerRestored.RegisterChannel(ChannelRegistration{
		Channel:  "AGY",
		Provider: agy,
		Models:   agyModels,
	})

	if routerRestored.CurrentMind().Model != "gemini-4-argon" || routerRestored.CurrentMind().Effort != "high" {
		t.Fatalf("failed to restore persisted mind: %+v", routerRestored.CurrentMind())
	}

	content, rErr := os.ReadFile(statePath)
	if rErr != nil || len(content) == 0 {
		t.Fatalf("state file not written properly: %v", rErr)
	}
}

func TestMindRouter_FutureExternalAgentExtension(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "active_mind_ext.json")

	router := NewMindRouter(MindRouterConfig{
		StatePath:       statePath,
		FallbackChannel: "API",
	})

	api := NewOpenAIProvider(OpenAIConfig{
		BaseURL: "https://api.example.com",
		APIKey:  "",
		Model:   "deepseek-chat",
	})
	router.RegisterChannel(ChannelRegistration{
		Channel:  "API",
		Provider: api,
	})

	customAgent := &mockCustomAgentProvider{model: "claude-3-7-sonnet"}
	router.RegisterChannel(ChannelRegistration{
		Channel:  "CLAUDE_CODE",
		Provider: customAgent,
		Models: []ModelInfo{
			{
				ID:   "claude-3-7-sonnet",
				Name: "Claude 3.7 Sonnet (Agent)",
			},
		},
	})

	channels := router.Channels()
	hasClaudeCode := false
	for _, ch := range channels {
		if ch == "CLAUDE_CODE" {
			hasClaudeCode = true
		}
	}
	if !hasClaudeCode {
		t.Fatalf("expected CLAUDE_CODE channel to be registered, got: %v", channels)
	}

	res, err := router.SwitchMind(SwitchRequest{Target: "claude-3-7-sonnet"})
	if err != nil || res.Status != "success" {
		t.Fatalf("failed to switch to third-party agent: %+v, err: %v", res, err)
	}
	if router.CurrentMind().Channel != "CLAUDE_CODE" || router.CurrentMind().Model != "claude-3-7-sonnet" {
		t.Fatalf("current mind mismatch for third-party agent: %+v", router.CurrentMind())
	}

	reply, chatErr := router.Chat(context.Background(), []core.ChatMessage{{Role: core.RoleUser, Content: "hi"}}, nil)
	if chatErr != nil || reply != "custom-agent-tool-response" {
		t.Fatalf("unexpected reply from third-party agent: %s, err: %v", reply, chatErr)
	}

	router.RegisterChannel(ChannelRegistration{
		Channel:  "API",
		Provider: api,
		Models: []ModelInfo{
			{
				ID:   "claude-3-5-sonnet-api",
				Name: "Claude 3.5 Sonnet (API Key)",
			},
		},
	})

	ambRes, _ := router.SwitchMind(SwitchRequest{Target: "sonnet"})
	if ambRes.Status != "ambiguous" {
		t.Fatalf("expected cross-channel ambiguity, got: %+v", ambRes)
	}
	if !strings.Contains(ambRes.Message, "CLAUDE_CODE 通道") || !strings.Contains(ambRes.Message, "API 通道") {
		t.Fatalf("ambiguity message should mention conflicting channels: %s", ambRes.Message)
	}
}

func TestAGYMindProvider_AssemblePromptPreservesHistory(t *testing.T) {
	agy := NewAGYMindProvider(AGYMindConfig{
		Model:  "gemini-3.8-flash",
		Effort: "low",
	})

	msgs := []core.ChatMessage{
		{Role: core.RoleSystem, Content: "你是呆呆"},
		{Role: core.RoleUser, Content: "我昨天买了一把机械键盘"},
		{Role: core.RoleAssistant, Content: "什么轴体的呀？"},
		{Role: core.RoleUser, Content: "红轴的，手感很轻"},
	}

	prompt := agy.assemblePrompt(msgs, nil, false)
	if !strings.Contains(prompt, "用户: 我昨天买了一把机械键盘") {
		t.Fatalf("expected historical user message in prompt, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "呆呆: 什么轴体的呀？") {
		t.Fatalf("expected historical assistant message in prompt, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "=== 用户当前发言 ===\n红轴的，手感很轻") {
		t.Fatalf("expected latest user message in current turn section, got:\n%s", prompt)
	}

	roPrompt := agy.assemblePrompt(msgs, nil, true)
	if !strings.Contains(roPrompt, "严格只读隔离模式") {
		t.Fatalf("expected read-only sandbox notice in guest prompt, got:\n%s", roPrompt)
	}
}

type mockFailingProvider struct {
	called bool
}

func (m *mockFailingProvider) Name() string { return "failing" }
func (m *mockFailingProvider) Chat(ctx context.Context, messages []core.ChatMessage, opts *core.ChatOptions) (string, error) {
	return "", context.Canceled
}
func (m *mockFailingProvider) ChatWithTools(ctx context.Context, messages []core.ChatMessage, tools []core.ToolDefinition, opts *core.ChatOptions) (*core.ChatResponse, error) {
	m.called = true
	return nil, context.Canceled
}

func TestMindRouter_CanceledContextDoesNotFallback(t *testing.T) {
	router := NewMindRouter(MindRouterConfig{
		StatePath:       t.TempDir() + "/mind.json",
		FallbackChannel: "API",
	})

	activeMock := &mockFailingProvider{}
	fallbackMock := &mockFailingProvider{}

	router.RegisterChannel(ChannelRegistration{
		Channel:  "AGY",
		Provider: activeMock,
		Models:   []ModelInfo{{ID: "agy-model"}},
	})
	router.RegisterChannel(ChannelRegistration{
		Channel:  "API",
		Provider: fallbackMock,
		Models:   []ModelInfo{{ID: "api-model"}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 主动取消 context

	_, err := router.ChatWithTools(ctx, []core.ChatMessage{{Role: core.RoleUser, Content: "hi"}}, nil, nil)
	if err == nil {
		t.Fatalf("expected error from canceled context")
	}
	if fallbackMock.called {
		t.Fatalf("expected fallback provider NOT to be called when context is canceled")
	}
}
