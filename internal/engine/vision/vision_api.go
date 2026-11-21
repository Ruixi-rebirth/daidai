package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"daidai/internal/core"
	"daidai/internal/media"
)

// describeWithAPI 调用外部标准 OpenAI 兼容视觉端点
func (e *Engine) describeWithAPI(ctx context.Context, imageURLs []string, userText string) (string, []int, error) {

	var imageParts []any
	var processedIndexes []int
	for index, rawURL := range imageURLs {
		imgBytes, err := e.fetchImage(ctx, rawURL)
		if err != nil || len(imgBytes) == 0 {
			if err != nil {
				slog.Warn("vision: 下载输入图片失败，已标记为未分析")
			}
			continue
		}
		imgBytes, _, err = media.NormalizeImageData(ctx, imgBytes)
		if err != nil {
			slog.Warn("vision: 无法解码输入图片", "err", err)
			continue
		}

		b64 := base64.StdEncoding.EncodeToString(imgBytes)
		dataURL := fmt.Sprintf("data:image/png;base64,%s", b64)

		imageParts = append(imageParts, map[string]any{
			"type": "image_url",
			"image_url": map[string]string{
				"url": dataURL,
			},
		})
		processedIndexes = append(processedIndexes, index)
	}

	if len(imageParts) == 0 {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		return "", nil, fmt.Errorf("no images successfully fetched")
	}
	prompt := visionUserContext(userText, core.MaxUserMessageRunes)
	contentParts := make([]any, 0, len(imageParts)+1)
	contentParts = append(contentParts, map[string]any{"type": "text", "text": prompt})
	contentParts = append(contentParts, imageParts...)

	payload := map[string]any{
		"model": e.cfg.Model,
		"messages": []map[string]any{
			{
				"role":    "system",
				"content": systemPrompt,
			},
			{
				"role":    "user",
				"content": contentParts,
			},
		},
		"max_tokens": 300,
	}

	reqBody, err := json.Marshal(payload)
	if err != nil {
		return "", nil, err
	}

	url := strings.TrimRight(e.cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := core.ReadBounded(resp.Body, maxVisionAPIResponseBytes)
	if err != nil {
		return "", nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return "", nil, &visionAPIError{status: resp.StatusCode, body: summarizeResponse(respBytes)}
	}

	var res struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBytes, &res); err != nil {
		return "", nil, err
	}
	if len(res.Choices) == 0 {
		return "", nil, fmt.Errorf("empty choices from vision model")
	}
	output := strings.TrimSpace(res.Choices[0].Message.Content)
	if output == "" {
		return "", nil, fmt.Errorf("empty output from vision model")
	}
	if core.OutputLimitReached(res.Choices[0].FinishReason) {
		output = "[视觉模型输出已截断，以下仅为部分观察]\n" + output
	}
	return output, processedIndexes, nil
}
