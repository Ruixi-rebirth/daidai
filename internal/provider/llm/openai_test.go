package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"daidai/internal/core"
)

func TestUnconfiguredChatModelDoesNotSubmitMessages(t *testing.T) {
	var called atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"unexpected response"}}]}`)
	}))
	defer server.Close()
	for _, model := range []string{"", "   "} {
		provider := NewOpenAIProvider(OpenAIConfig{BaseURL: server.URL, Model: model})
		if _, err := provider.Chat(context.Background(), []core.ChatMessage{{Role: core.RoleUser, Content: "private conversation"}}, nil); err == nil {
			t.Fatalf("unconfigured model %q was submitted", model)
		}
	}
	if called.Load() {
		t.Fatal("private messages reached an unconfigured chat provider")
	}
}

func TestOpenAIRequestRespondsToCallerCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	provider := NewOpenAIProvider(OpenAIConfig{BaseURL: server.URL, APIKey: "fixture", Model: "fixture"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := provider.Chat(ctx, []core.ChatMessage{{Role: core.RoleUser, Content: "hello"}}, nil)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not reach fixture server")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("caller cancellation was not preserved: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("long request did not respond to cancellation")
	}
}

func TestOpenAIChatRequest_Serialization(t *testing.T) {
	req := openAIChatRequest{
		Model: "deepseek-chat",
		Messages: []openAIMessage{
			{Role: "user", Content: "你好"},
		},
		Tools: []core.ToolDefinition{
			{
				Type: "function",
				Function: core.ToolFunctionDetail{
					Name:        "render_card",
					Description: "渲染卡片长图",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"content": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal openAIChatRequest: %v", err)
	}

	raw := string(data)
	if !strings.Contains(raw, `"tools":`) {
		t.Errorf("expected json to contain 'tools' field")
	}
	if !strings.Contains(raw, `"render_card"`) {
		t.Errorf("expected json to contain tool name 'render_card'")
	}
}
