package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"daidai/internal/core"
)

const maxOpenAIResponseBytes = 16 << 20

// OpenAIConfig 通用 OpenAI 兼容协议配置
type OpenAIConfig struct {
	BaseURL string // 例如 "https://api.deepseek.com/v1"
	APIKey  string
	Model   string // 例如 "deepseek-chat"
	Timeout time.Duration
}

// OpenAIProvider 统一 OpenAI 兼容驱动
type OpenAIProvider struct {
	mu              sync.RWMutex
	cfg             OpenAIConfig
	configuredModel string
	httpClient      *http.Client
}

// NewOpenAIProvider 实例化 OpenAI 协议驱动
func NewOpenAIProvider(cfg OpenAIConfig) *OpenAIProvider {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.openai.com/v1"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	return &OpenAIProvider{
		cfg:             cfg,
		configuredModel: strings.TrimSpace(cfg.Model),
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
			// Model APIs should not redirect requests, prompts, or credentials to
			// another host or to an internal service.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// SetModel 动态切换当前模型
func (p *OpenAIProvider) SetModel(model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg.Model = model
}

// GetModel 获取当前模型标识
func (p *OpenAIProvider) GetModel() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg.Model
}

func (p *OpenAIProvider) Name() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return "API (" + p.cfg.Model + ")"
}

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

type openAIMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []core.ToolCall `json:"tool_calls,omitempty"`
}

type openAIChatRequest struct {
	Model       string                `json:"model"`
	Messages    []openAIMessage       `json:"messages"`
	Tools       []core.ToolDefinition `json:"tools,omitempty"`
	Temperature float32               `json:"temperature"`
	MaxTokens   int                   `json:"max_tokens,omitempty"`
}

type openAIChatResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role      string          `json:"role"`
			Content   string          `json:"content"`
			ToolCalls []core.ToolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Chat 同步对话生成 (委托给 ChatWithTools)
func (p *OpenAIProvider) Chat(ctx context.Context, messages []core.ChatMessage, opts *core.ChatOptions) (string, error) {
	resp, err := p.ChatWithTools(ctx, messages, nil, opts)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// ChatWithTools 带工具调用的同步对话生成
func (p *OpenAIProvider) ChatWithTools(ctx context.Context, messages []core.ChatMessage, tools []core.ToolDefinition, opts *core.ChatOptions) (*core.ChatResponse, error) {
	reqMsgs := make([]openAIMessage, 0, len(messages))
	for _, m := range messages {
		reqMsgs = append(reqMsgs, openAIMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
			ToolCalls:  m.ToolCalls,
		})
	}

	temp := float32(0.7)
	maxTokens := 2048
	if opts != nil {
		// A non-nil options value explicitly carries its temperature, including
		// zero for deterministic classification and routing requests.
		temp = opts.Temperature
		if opts.MaxTokens > 0 {
			maxTokens = opts.MaxTokens
		}
	}

	p.mu.RLock()
	currentModel := p.cfg.Model
	baseURL := strings.TrimRight(p.cfg.BaseURL, "/")
	apiKey := p.cfg.APIKey
	p.mu.RUnlock()

	var reqTools []core.ToolDefinition
	if len(tools) > 0 {
		reqTools = tools
	}

	payload := openAIChatRequest{
		Model:       currentModel,
		Messages:    reqMsgs,
		Tools:       reqTools,
		Temperature: temp,
		MaxTokens:   maxTokens,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request error: %w", err)
	}

	url := baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create request error: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request error: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	bodyBytes, err := core.ReadBounded(resp.Body, maxOpenAIResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("read response body error: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("api returned status %d: %s", resp.StatusCode, responseSummary(bodyBytes))
	}

	var chatResp openAIChatResponse
	if err := json.Unmarshal(bodyBytes, &chatResp); err != nil {
		return nil, fmt.Errorf("unmarshal response error: %w (body: %s)", err, responseSummary(bodyBytes))
	}

	if chatResp.Error != nil && chatResp.Error.Message != "" {
		return nil, fmt.Errorf("model error: %s", chatResp.Error.Message)
	}

	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("empty choices returned by model")
	}

	choice := chatResp.Choices[0]
	switch strings.ToLower(strings.TrimSpace(choice.FinishReason)) {
	case "length", "max_tokens", "max_output_tokens":
		return nil, fmt.Errorf("model response was truncated because it reached the output token limit")
	}
	firstChoice := choice.Message
	return &core.ChatResponse{
		Content:   firstChoice.Content,
		ToolCalls: firstChoice.ToolCalls,
	}, nil
}

type openAIEmbeddingRequest struct {
	Model          string `json:"model"`
	Input          string `json:"input"`
	EncodingFormat string `json:"encoding_format,omitempty"`
}

type openAIEmbeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Embed 调用标准 OpenAI 兼容 /embeddings 端点生成稠密语义向量 (如 BAAI/bge-m3)
func (p *OpenAIProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	vec, _, err := p.EmbedIdentified(ctx, text)
	return vec, err
}

func (p *OpenAIProvider) EmbeddingIdentity() string {
	p.mu.RLock()
	baseURL := strings.TrimRight(p.cfg.BaseURL, "/")
	model := strings.TrimSpace(p.cfg.Model)
	p.mu.RUnlock()
	if baseURL == "" || model == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(baseURL + "\x00" + model))
	return hex.EncodeToString(sum[:])
}

func (p *OpenAIProvider) EmbedIdentified(ctx context.Context, text string) ([]float32, string, error) {
	input := strings.TrimSpace(text)
	if input == "" {
		return nil, "", nil
	}

	p.mu.RLock()
	currentModel := strings.TrimSpace(p.cfg.Model)
	baseURL := strings.TrimRight(p.cfg.BaseURL, "/")
	apiKey := strings.TrimSpace(p.cfg.APIKey)
	p.mu.RUnlock()
	identity := ""
	if baseURL != "" && currentModel != "" {
		sum := sha256.Sum256([]byte(baseURL + "\x00" + currentModel))
		identity = hex.EncodeToString(sum[:])
	}

	if apiKey == "" || baseURL == "" || currentModel == "" {
		return nil, "", nil
	}

	payload := openAIEmbeddingRequest{
		Model:          currentModel,
		Input:          input,
		EncodingFormat: "float",
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}

	reqCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, baseURL+"/embeddings", bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	bodyBytes, err := core.ReadBounded(resp.Body, maxOpenAIResponseBytes)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("embedding api status %d: %s", resp.StatusCode, responseSummary(bodyBytes))
	}

	var embResp openAIEmbeddingResponse
	if err := json.Unmarshal(bodyBytes, &embResp); err != nil {
		return nil, "", err
	}
	if embResp.Error != nil && embResp.Error.Message != "" {
		return nil, "", fmt.Errorf("embedding error: %s", embResp.Error.Message)
	}
	if len(embResp.Data) == 0 || len(embResp.Data[0].Embedding) == 0 {
		return nil, "", nil
	}
	return embResp.Data[0].Embedding, identity, nil
}

func responseSummary(body []byte) string {
	const maxSummaryBytes = 4096
	if len(body) <= maxSummaryBytes {
		return string(body)
	}
	return string(body[:maxSummaryBytes]) + "..."
}
