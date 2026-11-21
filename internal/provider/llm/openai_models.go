package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"daidai/internal/core"
)

// DiscoverModels 通过标准 OpenAI 协议 GET /models 端点动态探测当前 API 服务商支持的模型列表
func (p *OpenAIProvider) DiscoverModels(ctx context.Context) []ModelInfo {
	models, err := p.DiscoverModelsWithError(ctx)
	if err != nil {
		slog.Warn("api: 模型池探测失败", "err", err)
	}
	return models
}

func (p *OpenAIProvider) DiscoverModelsWithError(ctx context.Context) ([]ModelInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.RLock()
	baseURL := strings.TrimRight(p.cfg.BaseURL, "/")
	apiKey := p.cfg.APIKey
	configuredModel := p.configuredModel
	p.mu.RUnlock()

	fallbackModels := make([]ModelInfo, 0, 1)
	if configuredModel != "" {
		fallbackModels = append(fallbackModels, ModelInfo{
			ID:      configuredModel,
			Channel: "API",
			Name:    configuredModel,
		})
	}
	if baseURL == "" {
		return fallbackModels, fmt.Errorf("API model discovery has no base URL")
	}

	var models []ModelInfo
	seen := make(map[string]struct{})

	addModel := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		lower := strings.ToLower(id)
		if _, exists := seen[lower]; exists {
			return
		}
		seen[lower] = struct{}{}
		models = append(models, ModelInfo{
			ID:      id,
			Channel: "API",
			Name:    id,
		})
	}

	if baseURL != "" {
		discCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(discCtx, http.MethodGet, baseURL+"/models", nil)
		if err != nil {
			return fallbackModels, fmt.Errorf("create /models request: %w", err)
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, err := p.httpClient.Do(req)
		if err != nil {
			return fallbackModels, fmt.Errorf("request /models: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return fallbackModels, fmt.Errorf("/models returned HTTP %d", resp.StatusCode)
		}
		body, err := core.ReadBounded(resp.Body, maxOpenAIResponseBytes)
		if err != nil {
			return fallbackModels, fmt.Errorf("read /models response: %w", err)
		}
		var listResp struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &listResp); err != nil {
			return fallbackModels, fmt.Errorf("decode /models response: %w", err)
		}
		for _, item := range listResp.Data {
			addModel(item.ID)
		}
		if len(models) == 0 {
			return fallbackModels, fmt.Errorf("/models returned no recognizable models")
		}
		for index := range models {
			if strings.EqualFold(models[index].ID, configuredModel) {
				if index > 0 {
					configured := models[index]
					copy(models[1:index+1], models[:index])
					models[0] = configured
				}
				break
			}
		}
		slog.Info("api: 成功从远端 /models 动态探测可用模型列表", "count", len(models))
	}

	return models, nil
}
