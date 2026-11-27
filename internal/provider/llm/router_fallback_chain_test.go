package llm

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"daidai/internal/core"
)

type chainStubProvider struct {
	name  string
	reply string
	err   error
	calls int
}

func (p *chainStubProvider) Name() string {
	if p.name != "" {
		return p.name
	}
	return "stub"
}

func (p *chainStubProvider) Chat(ctx context.Context, messages []core.ChatMessage, opts *core.ChatOptions) (string, error) {
	resp, err := p.ChatWithTools(ctx, messages, nil, opts)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

func (p *chainStubProvider) ChatWithTools(_ context.Context, _ []core.ChatMessage, _ []core.ToolDefinition, _ *core.ChatOptions) (*core.ChatResponse, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return &core.ChatResponse{Content: p.reply}, nil
}

func TestMindRouter_OrderedFallbackChain_AGYToGLMToAPI(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "active_mind.json")
	router := NewMindRouter(MindRouterConfig{
		StatePath:        statePath,
		FallbackChannels: []string{"GLM", "API"},
	})

	agy := &chainStubProvider{err: errors.New("429 RESOURCE_EXHAUSTED")}
	glm := &chainStubProvider{reply: "from-glm"}
	api := &chainStubProvider{reply: "from-deepseek"}

	router.RegisterChannel(ChannelRegistration{
		Channel:  "AGY",
		Provider: agy,
		Models:   []ModelInfo{{ID: "gemini-4", Channel: "AGY", Name: "Gemini 4"}},
	})
	router.RegisterChannel(ChannelRegistration{
		Channel:  "GLM",
		Provider: glm,
		Models:   []ModelInfo{{ID: "glm-4.7-flash", Channel: "GLM", Name: "GLM-4.7-Flash"}},
	})
	router.RegisterChannel(ChannelRegistration{
		Channel:  "API",
		Provider: api,
		Models:   []ModelInfo{{ID: "deepseek-chat", Channel: "API", Name: "DeepSeek"}},
	})

	// 1. AGY fails -> GLM catches it before DeepSeek.
	got, err := router.Chat(context.Background(), []core.ChatMessage{{Role: core.RoleUser, Content: "hi"}}, nil)
	if err != nil || got != "from-glm" {
		t.Fatalf("expected GLM first fallback, got (%q, %v)", got, err)
	}
	if api.calls != 0 {
		t.Fatalf("DeepSeek API should not be called when GLM succeeds, calls=%d", api.calls)
	}

	// 2. GLM also fails -> falls through to DeepSeek API.
	glm.err = errors.New("429 concurrency limit")
	got, err = router.Chat(context.Background(), []core.ChatMessage{{Role: core.RoleUser, Content: "hi again"}}, nil)
	if err != nil || got != "from-deepseek" {
		t.Fatalf("expected DeepSeek final fallback, got (%q, %v)", got, err)
	}

	// 3. FastRoute tries GLM first when healthy, and falls back to DeepSeek when GLM fails.
	glm.err = nil
	got, err = router.Chat(context.Background(), []core.ChatMessage{{Role: core.RoleUser, Content: "classify"}}, &core.ChatOptions{FastRoute: true})
	if err != nil || got != "from-glm" {
		t.Fatalf("expected FastRoute to hit GLM first, got (%q, %v)", got, err)
	}
}
