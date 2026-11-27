package tool

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"daidai/internal/core"
)

// TextToImageAPI calls an OpenAI/Zhipu-compatible POST /images/generations endpoint
// (e.g. Zhipu BigModel cogview-3-flash) for pure text-to-image fallback.
type TextToImageAPI struct {
	BaseURL    string
	APIKey     string
	Model      string
	Size       string
	HTTPClient *http.Client
}

// NewTextToImageAPI constructs a text-to-image API fallback client.
func NewTextToImageAPI(baseURL, apiKey, model string) *TextToImageAPI {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	apiKey = strings.TrimSpace(apiKey)
	model = strings.TrimSpace(model)
	if baseURL == "" || apiKey == "" || model == "" {
		return nil
	}
	return &TextToImageAPI{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   model,
		Size:    "1024x1024",
	}
}

// Generate generates a single PNG image from prompt using the configured API endpoint.
func (a *TextToImageAPI) Generate(ctx context.Context, prompt string) ([]byte, error) {
	if a == nil || a.BaseURL == "" || a.APIKey == "" || a.Model == "" {
		return nil, fmt.Errorf("text-to-image API not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	reqBody := map[string]any{
		"model":  a.Model,
		"prompt": prompt,
	}
	if strings.TrimSpace(a.Size) != "" {
		reqBody["size"] = strings.TrimSpace(a.Size)
	}
	rawReq, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("encode image API request: %w", err)
	}

	endpoint := a.BaseURL + "/images/generations"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(rawReq))
	if err != nil {
		return nil, fmt.Errorf("build image API request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.APIKey)

	client := a.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call image API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read image API response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("image API status %d: %s", resp.StatusCode, core.TruncateRunes(strings.TrimSpace(string(respBytes)), 300, "…"))
	}

	var parsed struct {
		Data []struct {
			URL     string `json:"url"`
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return nil, fmt.Errorf("decode image API response: %w", err)
	}
	if len(parsed.Data) == 0 {
		return nil, fmt.Errorf("image API returned empty data")
	}

	var rawImage []byte
	if b64 := strings.TrimSpace(parsed.Data[0].B64JSON); b64 != "" {
		rawImage, err = base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("decode image API base64: %w", err)
		}
	} else if imgURL := strings.TrimSpace(parsed.Data[0].URL); imgURL != "" {
		rawImage, err = a.downloadImage(ctx, imgURL)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("image API response missing url and b64_json")
	}

	normalized, _, err := normalizeImageData(ctx, rawImage)
	if err != nil {
		return nil, fmt.Errorf("normalize image API output: %w", err)
	}
	return normalized, nil
}

func (a *TextToImageAPI) downloadImage(ctx context.Context, imgURL string) ([]byte, error) {
	if a.HTTPClient != nil {
		// Allow injected test clients without public DNS resolution.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, imgURL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := a.HTTPClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("download image API result: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("download image API result status %d", resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, core.MaxImageBytes+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > core.MaxImageBytes {
			return nil, fmt.Errorf("image API result exceeds %d byte limit", core.MaxImageBytes)
		}
		return data, nil
	}

	if err := core.ValidateSafeExternalURL(ctx, imgURL); err != nil {
		return nil, fmt.Errorf("unsafe image API result URL: %w", err)
	}
	client := core.NewProtectedHTTPClient(30 * time.Second)
	defer client.CloseIdleConnections()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("stopped after 5 image redirects")
		}
		return core.ValidateSafeExternalURL(req.Context(), req.URL.String())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imgURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download image API result: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download image API result status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, core.MaxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > core.MaxImageBytes {
		return nil, fmt.Errorf("image API result exceeds %d byte limit", core.MaxImageBytes)
	}
	return data, nil
}
