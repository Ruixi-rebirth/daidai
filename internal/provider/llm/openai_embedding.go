package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"daidai/internal/core"
)

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
