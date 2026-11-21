package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"daidai/internal/core"
)

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
	currentModel := strings.TrimSpace(p.cfg.Model)
	baseURL := strings.TrimRight(p.cfg.BaseURL, "/")
	apiKey := p.cfg.APIKey
	p.mu.RUnlock()
	if currentModel == "" {
		return nil, fmt.Errorf("API chat model is not configured")
	}

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
	firstChoice := choice.Message
	return &core.ChatResponse{
		Content:      firstChoice.Content,
		ToolCalls:    firstChoice.ToolCalls,
		FinishReason: strings.ToLower(strings.TrimSpace(choice.FinishReason)),
	}, nil
}

func responseSummary(body []byte) string {
	const maxSummaryBytes = 4096
	if len(body) <= maxSummaryBytes {
		return string(body)
	}
	return string(body[:maxSummaryBytes]) + "..."
}
