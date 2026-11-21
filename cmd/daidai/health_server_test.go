package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"daidai/internal/config"
	"daidai/internal/provider/llm"
)

func TestHealthServerDuringShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router := llm.NewMindRouter(llm.MindRouterConfig{
		StatePath:       filepath.Join(t.TempDir(), "mind.json"),
		FallbackChannel: "API",
	})
	router.RegisterChannel(llm.ChannelRegistration{
		Channel:  "API",
		Provider: llm.NewOpenAIProvider(llm.OpenAIConfig{Model: "fixture"}),
		Models:   []llm.ModelInfo{{ID: "fixture"}},
	})
	handler := newHealthServer(ctx, &config.Config{}, nil, nil, router, nil, nil).Handler
	probe := func(path string, status int, bodyFragment string) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != status || !strings.Contains(response.Body.String(), bodyFragment) {
			t.Fatalf("%s returned %d %q, want status %d and body containing %q", path, response.Code, response.Body.String(), status, bodyFragment)
		}
	}
	probe("/readyz", http.StatusOK, `"ready":true`)
	cancel()
	probe("/readyz", http.StatusServiceUnavailable, `"lifecycle":"stopping"`)
	probe("/healthz", http.StatusOK, "ok")
}
