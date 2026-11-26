package llm

import (
	"context"
	"errors"
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
	if !strings.Contains(roPrompt, "只读推理") || strings.Contains(roPrompt, "RoleGuest") {
		t.Fatalf("expected a read-only scope without changing the user's role, got:\n%s", roPrompt)
	}
}

type mockFailingProvider struct {
	called   bool
	response *core.ChatResponse
}

func (m *mockFailingProvider) Name() string { return "failing" }
func (m *mockFailingProvider) Chat(ctx context.Context, messages []core.ChatMessage, opts *core.ChatOptions) (string, error) {
	return "", context.Canceled
}
func (m *mockFailingProvider) ChatWithTools(ctx context.Context, messages []core.ChatMessage, tools []core.ToolDefinition, opts *core.ChatOptions) (*core.ChatResponse, error) {
	m.called = true
	if m.response != nil {
		return m.response, nil
	}
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

func TestMindRouterPreservesEmptyOutputLimitResponse(t *testing.T) {
	router := NewMindRouter(MindRouterConfig{StatePath: filepath.Join(t.TempDir(), "mind.json"), FallbackChannel: "API"})
	active := &mockFailingProvider{response: &core.ChatResponse{FinishReason: "length"}}
	fallback := &mockFailingProvider{}
	router.RegisterChannel(ChannelRegistration{Channel: "AGY", Provider: active, Models: []ModelInfo{{ID: "agy-model"}}})
	router.RegisterChannel(ChannelRegistration{Channel: "API", Provider: fallback, Models: []ModelInfo{{ID: "api-model"}}})
	response, err := router.ChatWithTools(context.Background(), []core.ChatMessage{{Role: core.RoleUser, Content: "继续"}}, nil, nil)
	if err != nil || response != active.response || fallback.called {
		t.Fatalf("output limit was lost in routing: response=%+v fallback=%v error=%v", response, fallback.called, err)
	}
}

func TestModelStatusShowsConfiguredFallback(t *testing.T) {
	router := NewMindRouter(MindRouterConfig{StatePath: filepath.Join(t.TempDir(), "mind.json"), FallbackChannel: "API"})
	agy := &mockCustomAgentProvider{model: "gemini-test"}
	router.RegisterChannel(ChannelRegistration{Channel: "AGY", Provider: agy, Models: []ModelInfo{{ID: "gemini-test", Name: "Gemini Test", Channel: "AGY"}}})
	api := NewOpenAIProvider(OpenAIConfig{Model: "deepseek-chat"})
	router.RegisterChannel(ChannelRegistration{Channel: "API", Provider: api, Models: []ModelInfo{{ID: "deepseek-chat", Channel: "API"}, {ID: "deepseek-reasoner", Channel: "API"}}})
	status := router.FormatCurrentStatus()
	for _, want := range []string{"首选聊天模型", "gemini-test", "备用与轻量分类模型", "deepseek-chat"} {
		if !strings.Contains(status, want) {
			t.Fatalf("missing %q in %s", want, status)
		}
	}
	model, usage := router.ChannelModelUsage("API")
	if model != "deepseek-chat" || usage != "备用兜底 / 轻量门控" {
		t.Fatalf("wrong fallback: %s %s", model, usage)
	}
	// Display the live provider configuration, not the first discovered model.
	api.SetModel("deepseek-reasoner")
	if status := router.FormatCurrentStatus(); !strings.Contains(status, "deepseek-reasoner") || strings.Contains(status, "deepseek-chat") {
		t.Fatal(status)
	}
	result, err := router.SwitchMind(SwitchRequest{Target: "deepseek-chat", Channel: "API"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "success" {
		t.Fatalf("switch failed: %+v", result)
	}
	if status := router.FormatCurrentStatus(); strings.Contains(status, "备用与轻量分类模型") {
		t.Fatal("preferred API duplicated as fallback", status)
	}
}

func TestMindRouter_ReadOnlyPrefersActiveAGYWithFallbackAndFastRouteUsesAPI(t *testing.T) {
	router := NewMindRouter(MindRouterConfig{StatePath: filepath.Join(t.TempDir(), "mind.json"), FallbackChannel: "API"})
	agy := &mockFailingProvider{response: &core.ChatResponse{Content: "agy-sandbox-reply"}}
	api := &mockFailingProvider{response: &core.ChatResponse{Content: "api-fallback-reply"}}
	router.RegisterChannel(ChannelRegistration{Channel: "AGY", Provider: agy, Models: []ModelInfo{{ID: "gemini-4-hydrogen", Channel: "AGY"}}})
	router.RegisterChannel(ChannelRegistration{Channel: "API", Provider: api, Models: []ModelInfo{{ID: "deepseek-chat", Channel: "API"}}})
	if _, err := router.SwitchMind(SwitchRequest{Target: "gemini-4-hydrogen", Channel: "AGY"}); err != nil {
		t.Fatal(err)
	}

	// 1. Ordinary ReadOnly chat (non-partner / group) hits active AGY first.
	resp, err := router.ChatWithTools(context.Background(), []core.ChatMessage{{Role: core.RoleUser, Content: "676767"}}, nil, &core.ChatOptions{ReadOnly: true})
	if err != nil || resp.Content != "agy-sandbox-reply" || !agy.called || api.called {
		t.Fatalf("expected ReadOnly chat to hit AGY primary first, got resp=%+v agy=%v api=%v err=%v", resp, agy.called, api.called, err)
	}

	// 2. FastRoute classifier hits low-latency API directly without spawning AGY.
	agy.called, api.called = false, false
	fastResp, err := router.ChatWithTools(context.Background(), []core.ChatMessage{{Role: core.RoleUser, Content: "classify"}}, nil, &core.ChatOptions{ReadOnly: true, FastRoute: true})
	if err != nil || fastResp.Content != "api-fallback-reply" || agy.called || !api.called {
		t.Fatalf("expected FastRoute to hit API directly, got resp=%+v agy=%v api=%v err=%v", fastResp, agy.called, api.called, err)
	}

	// 3. When AGY fails on ReadOnly chat (e.g. quota or missing bwrap), MindRouter falls back to API.
	agy.called, api.called = false, false
	agy.response = &core.ChatResponse{} // empty response triggers provider failure
	fbResp, err := router.ChatWithTools(context.Background(), []core.ChatMessage{{Role: core.RoleUser, Content: "hello"}}, nil, &core.ChatOptions{ReadOnly: true})
	if err != nil || fbResp.Content != "api-fallback-reply" || !agy.called || !api.called {
		t.Fatalf("expected ReadOnly AGY failure to fall back to API, got resp=%+v agy=%v api=%v err=%v", fbResp, agy.called, api.called, err)
	}
}

func TestSplitModelAndEffort_Parenthesized(t *testing.T) {
	cases := []struct {
		raw        string
		wantModel  string
		wantEffort string
	}{
		{"Gemini 4 Hydrogen (High)", "Gemini 4 Hydrogen", "high"},
		{"Gemini 4 Hydrogen (XHigh)", "Gemini 4 Hydrogen", "xhigh"},
		{"gemini-4-hydrogen-low", "gemini-4-hydrogen", "low"},
		{"gemini-4-hydrogen medium", "gemini-4-hydrogen", "medium"},
		{"Claude Sonnet 4.6 (Thinking)", "Claude Sonnet 4.6 (Thinking)", ""},
	}
	for _, tc := range cases {
		gotModel, gotEffort := SplitModelAndEffort(tc.raw)
		if gotModel != tc.wantModel || gotEffort != tc.wantEffort {
			t.Fatalf("SplitModelAndEffort(%q) = (%q, %q), want (%q, %q)",
				tc.raw, gotModel, gotEffort, tc.wantModel, tc.wantEffort)
		}
	}
}

type recordingProvider struct {
	lastOpts *core.ChatOptions
	fail     bool
	reply    string
}

func (p *recordingProvider) Name() string { return "recording" }
func (p *recordingProvider) Chat(ctx context.Context, msgs []core.ChatMessage, opts *core.ChatOptions) (string, error) {
	resp, err := p.ChatWithTools(ctx, msgs, nil, opts)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}
func (p *recordingProvider) ChatWithTools(_ context.Context, _ []core.ChatMessage, _ []core.ToolDefinition, opts *core.ChatOptions) (*core.ChatResponse, error) {
	if opts != nil {
		copied := *opts
		p.lastOpts = &copied
	} else {
		p.lastOpts = nil
	}
	if p.fail {
		return nil, context.DeadlineExceeded
	}
	return &core.ChatResponse{Content: p.reply}, nil
}
func (*recordingProvider) SetModel(string)  {}
func (*recordingProvider) SetEffort(string) {}

func TestMindRouter_DualTrackPartnerAndPublicIsolation(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "active_mind.json")
	router := NewMindRouter(MindRouterConfig{StatePath: statePath, FallbackChannel: "API"})

	agy := &recordingProvider{reply: "agy-ok"}
	api := &recordingProvider{reply: "api-ok"}
	agyModels := ParseCLIModelsOutput(testAGYModelsOutput(), "AGY")
	apiModels := []ModelInfo{{ID: "deepseek-chat", Name: "deepseek-chat"}}

	router.RegisterChannel(ChannelRegistration{Channel: "API", Provider: api, Models: apiModels})
	router.RegisterChannel(ChannelRegistration{Channel: "AGY", Provider: agy, Models: agyModels})

	// 1. Default public mind automatically prefers lightweight AGY flash model.
	if pub := router.PublicMind(); pub.Channel != "AGY" || pub.Model != "gemini-3.8-flash" || pub.Effort != "medium" {
		t.Fatalf("expected default public mind AGY/gemini-3.8-flash/medium, got %+v", pub)
	}

	// 2. Partner switches private mind to Claude Opus High; public mind stays Gemini 3.8 Flash Medium.
	res, err := router.SwitchMind(SwitchRequest{Target: "claude-opus", Effort: "high"})
	if err != nil || res.Status != "success" {
		t.Fatalf("partner switch failed: %+v, %v", res, err)
	}
	if curr := router.CurrentMind(); curr.Model != "claude-opus-5-5" || curr.Effort != "high" {
		t.Fatalf("unexpected partner mind: %+v", curr)
	}
	if pub := router.PublicMind(); pub.Model != "gemini-3.8-flash" || pub.Effort != "medium" {
		t.Fatalf("partner switch leaked into public mind: %+v", pub)
	}

	// 3. Per-request routing passes isolated (Model, Effort) pairs without cross-talk.
	if _, err := router.ChatWithTools(context.Background(), []core.ChatMessage{{Role: core.RoleUser, Content: "partner work"}}, nil, &core.ChatOptions{Temperature: 0.7}); err != nil {
		t.Fatal(err)
	}
	if agy.lastOpts == nil || agy.lastOpts.Model != "claude-opus-5-5" || agy.lastOpts.Effort != "high" {
		t.Fatalf("partner call got wrong model/effort: %+v", agy.lastOpts)
	}

	if _, err := router.ChatWithTools(context.Background(), []core.ChatMessage{{Role: core.RoleUser, Content: "group banter"}}, nil, &core.ChatOptions{Temperature: 0.7, ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if agy.lastOpts == nil || agy.lastOpts.Model != "gemini-3.8-flash" || agy.lastOpts.Effort != "medium" {
		t.Fatalf("public call got wrong model/effort: %+v", agy.lastOpts)
	}

	// 4. Fallback to API strips AGY model ID so API uses its own configured model.
	agy.fail = true
	if fbResp, err := router.ChatWithTools(context.Background(), []core.ChatMessage{{Role: core.RoleUser, Content: "group banter"}}, nil, &core.ChatOptions{Temperature: 0.7, ReadOnly: true}); err != nil || fbResp.Content != "api-ok" {
		t.Fatalf("expected API fallback on AGY failure, got resp=%+v err=%v", fbResp, err)
	}
	if api.lastOpts != nil && api.lastOpts.Model != "" {
		t.Fatalf("fallback leaked primary channel model ID into API options: %+v", api.lastOpts)
	}
	agy.fail = false

	// 5. Explicit public switch caps high effort to medium and persists across restart without touching partner mind.
	pubRes, err := router.SwitchMind(SwitchRequest{Target: "gemini-4-argon", Effort: "high", Scope: MindScopePublic})
	if err != nil || pubRes.Status != "success" || pubRes.Effort != "medium" {
		t.Fatalf("expected public switch to cap effort at medium, got %+v, %v", pubRes, err)
	}
	if curr := router.CurrentMind(); curr.Model != "claude-opus-5-5" || curr.Effort != "high" {
		t.Fatalf("public switch modified partner mind: %+v", curr)
	}

	restored := NewMindRouter(MindRouterConfig{StatePath: statePath, FallbackChannel: "API"})
	restored.RegisterChannel(ChannelRegistration{Channel: "API", Provider: api, Models: apiModels})
	restored.RegisterChannel(ChannelRegistration{Channel: "AGY", Provider: agy, Models: agyModels})
	if curr := restored.CurrentMind(); curr.Model != "claude-opus-5-5" || curr.Effort != "high" {
		t.Fatalf("restored partner mind mismatch: %+v", curr)
	}
	if pub := restored.PublicMind(); pub.Model != "gemini-4-argon" || pub.Effort != "medium" {
		t.Fatalf("restored public mind mismatch: %+v", pub)
	}
}

type cooldownTestProvider struct {
	calls int
	err   error
	reply string
}

func (p *cooldownTestProvider) Name() string { return "cooldown-test" }
func (p *cooldownTestProvider) Chat(ctx context.Context, msgs []core.ChatMessage, opts *core.ChatOptions) (string, error) {
	resp, err := p.ChatWithTools(ctx, msgs, nil, opts)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}
func (p *cooldownTestProvider) ChatWithTools(_ context.Context, _ []core.ChatMessage, _ []core.ToolDefinition, _ *core.ChatOptions) (*core.ChatResponse, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return &core.ChatResponse{Content: p.reply}, nil
}
func (*cooldownTestProvider) SetModel(string)  {}
func (*cooldownTestProvider) SetEffort(string) {}

func TestMindRouter_CooldownSkipsFailingPrimaryUntilReset(t *testing.T) {
	router := NewMindRouter(MindRouterConfig{
		StatePath:       filepath.Join(t.TempDir(), "active_mind.json"),
		FallbackChannel: "API",
	})
	agy := &cooldownTestProvider{err: errors.New("AGY 执行异常: RESOURCE_EXHAUSTED (code 429): Individual quota reached. Resets in 10m0s.")}
	api := &cooldownTestProvider{reply: "api-fallback-ok"}
	router.RegisterChannel(ChannelRegistration{
		Channel:  "API",
		Provider: api,
		Models:   []ModelInfo{{ID: "deepseek-chat", Channel: "API"}},
	})
	router.RegisterChannel(ChannelRegistration{
		Channel:  "AGY",
		Provider: agy,
		Models:   []ModelInfo{{ID: "gemini-4-argon", Channel: "AGY"}, {ID: "gemini-3.8-flash", Channel: "AGY"}},
	})
	if _, err := router.SwitchMind(SwitchRequest{Target: "gemini-4-argon", Channel: "AGY"}); err != nil {
		t.Fatal(err)
	}

	// 1. First Partner (!ReadOnly) call hits AGY 429 -> falls back cleanly AND activates cooldown.
	resp, err := router.ChatWithTools(context.Background(), []core.ChatMessage{{Role: core.RoleUser, Content: "hi"}}, nil, &core.ChatOptions{})
	if err != nil || resp.Content != "api-fallback-ok" || agy.calls != 1 || api.calls != 1 {
		t.Fatalf("unexpected first fallback result: resp=%+v err=%v agy=%d api=%d", resp, err, agy.calls, api.calls)
	}

	// Status should display remaining cooldown.
	if status := router.FormatStatusFor(true); !strings.Contains(status, "冷却状态") {
		t.Fatalf("expected cooldown notice in status, got:\n%s", status)
	}

	// 2. Second immediate call skips cooling primary AGY without spawning it.
	resp2, err := router.ChatWithTools(context.Background(), []core.ChatMessage{{Role: core.RoleUser, Content: "hi again"}}, nil, &core.ChatOptions{})
	if err != nil || resp2.Content != "api-fallback-ok" || agy.calls != 1 || api.calls != 2 {
		t.Fatalf("expected cooldown short-circuit without calling AGY, got resp=%+v err=%v agy=%d api=%d", resp2, err, agy.calls, api.calls)
	}

	// 3. Explicit manual switch clears cooldown and probes AGY again.
	agy.err = nil
	agy.reply = "agy-back-ok"
	if res, err := router.SwitchMind(SwitchRequest{Target: "gemini-4-argon", Channel: "AGY"}); err != nil || res.Status != "success" {
		t.Fatalf("switch failed: %+v, %v", res, err)
	}
	resp3, err := router.ChatWithTools(context.Background(), []core.ChatMessage{{Role: core.RoleUser, Content: "after switch"}}, nil, &core.ChatOptions{})
	if err != nil || resp3.Content != "agy-back-ok" || agy.calls != 2 {
		t.Fatalf("expected switch to clear cooldown and use AGY, got resp=%+v err=%v agy=%d", resp3, err, agy.calls)
	}
}
