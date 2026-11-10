package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"daidai/internal/core"
)

// OpenAIConfig 通用 OpenAI 兼容协议配置
type OpenAIConfig struct {
	BaseURL string // 例如 "https://api.deepseek.com/v1"
	APIKey  string
	Model   string // 例如 "deepseek-chat"
	Timeout time.Duration
}

// OpenAIProvider 统一 OpenAI 兼容驱动
type OpenAIProvider struct {
	mu         sync.RWMutex
	cfg        OpenAIConfig
	httpClient *http.Client
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
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
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
	p.mu.RLock()
	baseURL := strings.TrimRight(p.cfg.BaseURL, "/")
	apiKey := p.cfg.APIKey
	configuredModel := strings.TrimSpace(p.cfg.Model)
	p.mu.RUnlock()

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

	// 优先确保配置文件中显式指定的默认模型排在首位
	if configuredModel != "" {
		addModel(configuredModel)
	}

	if apiKey != "" && baseURL != "" {
		discCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(discCtx, http.MethodGet, baseURL+"/models", nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+apiKey)
			resp, err := p.httpClient.Do(req)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					var listResp struct {
						Data []struct {
							ID string `json:"id"`
						} `json:"data"`
					}
					if err := json.NewDecoder(resp.Body).Decode(&listResp); err == nil {
						for _, item := range listResp.Data {
							addModel(item.ID)
						}
						slog.Info("api: 成功从远端 /models 动态探测可用模型列表", "count", len(models))
					}
				}
			}
		}
	}

	return models
}

type deepseekBalanceResponse struct {
	IsAvailable  bool `json:"is_available"`
	BalanceInfos []struct {
		Currency        string `json:"currency"`
		TotalBalance    string `json:"total_balance"`
		GrantedBalance  string `json:"granted_balance"`
		ToppedUpBalance string `json:"topped_up_balance"`
	} `json:"balance_infos"`
}

// GetUsage 查询当前 API 服务商的账户余额与用量状态 (实现 UsageDiscoverer)
func (p *OpenAIProvider) GetUsage(ctx context.Context) (string, error) {
	p.mu.RLock()
	baseURL := p.cfg.BaseURL
	apiKey := p.cfg.APIKey
	model := p.cfg.Model
	p.mu.RUnlock()

	if strings.Contains(baseURL, "deepseek.com") {
		return p.getDeepSeekBalance(ctx, apiKey)
	}

	return fmt.Sprintf("#### API 通道实时状态\n- 服务端点：%s\n- 当前模型：%s\n- 计费说明：OpenAI 兼容协议端点，按 Token 计费", baseURL, model), nil
}

func (p *OpenAIProvider) getDeepSeekBalance(ctx context.Context, apiKey string) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "https://api.deepseek.com/user/balance", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求 DeepSeek balance 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("DeepSeek balance 响应异常 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var data deepseekBalanceResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", fmt.Errorf("解析 DeepSeek balance 失败: %w", err)
	}

	var sb strings.Builder
	sb.WriteString("#### API (DeepSeek) 账户余额与用量\n")
	statusStr := "不可用"
	if data.IsAvailable {
		statusStr = "正常可用"
	}
	sb.WriteString(fmt.Sprintf("- 账户状态：%s\n", statusStr))

	for _, b := range data.BalanceInfos {
		sb.WriteString(fmt.Sprintf("- **总可用余额**：**%s %s**\n", b.TotalBalance, b.Currency))
		sb.WriteString(fmt.Sprintf("- 充值余额：%s %s\n", b.ToppedUpBalance, b.Currency))
		sb.WriteString(fmt.Sprintf("- 赠送金余额：%s %s\n", b.GrantedBalance, b.Currency))
	}

	return strings.TrimSpace(sb.String()), nil
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
	Temperature float32               `json:"temperature,omitempty"`
	MaxTokens   int                   `json:"max_tokens,omitempty"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message struct {
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
		if opts.Temperature > 0 {
			temp = opts.Temperature
		}
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
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body error: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("api returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var chatResp openAIChatResponse
	if err := json.Unmarshal(bodyBytes, &chatResp); err != nil {
		return nil, fmt.Errorf("unmarshal response error: %w (body: %s)", err, string(bodyBytes))
	}

	if chatResp.Error != nil && chatResp.Error.Message != "" {
		return nil, fmt.Errorf("model error: %s", chatResp.Error.Message)
	}

	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("empty choices returned by model")
	}

	firstChoice := chatResp.Choices[0].Message
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
	input := strings.TrimSpace(text)
	if input == "" {
		return nil, nil
	}

	p.mu.RLock()
	currentModel := strings.TrimSpace(p.cfg.Model)
	baseURL := strings.TrimRight(p.cfg.BaseURL, "/")
	apiKey := strings.TrimSpace(p.cfg.APIKey)
	p.mu.RUnlock()

	if apiKey == "" || baseURL == "" || currentModel == "" {
		return nil, nil
	}

	payload := openAIEmbeddingRequest{
		Model:          currentModel,
		Input:          input,
		EncodingFormat: "float",
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, baseURL+"/embeddings", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding api status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var embResp openAIEmbeddingResponse
	if err := json.Unmarshal(bodyBytes, &embResp); err != nil {
		return nil, err
	}
	if embResp.Error != nil && embResp.Error.Message != "" {
		return nil, fmt.Errorf("embedding error: %s", embResp.Error.Message)
	}
	if len(embResp.Data) == 0 || len(embResp.Data[0].Embedding) == 0 {
		return nil, nil
	}
	return embResp.Data[0].Embedding, nil
}
