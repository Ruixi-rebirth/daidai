package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Do 发送已签名的 OCI REST API 请求
func (c *OCIClient) Do(ctx context.Context, method, urlStr string, bodyData any) (*http.Response, []byte, error) {
	var bodyBytes []byte
	var bodyReader io.Reader

	if bodyData != nil {
		var err error
		bodyBytes, err = json.Marshal(bodyData)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal request body error: %w", err)
		}
		bodyReader = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, method, urlStr, bodyReader)
	if err != nil {
		return nil, nil, fmt.Errorf("new request error: %w", err)
	}

	if err := c.SignRequest(req, bodyBytes); err != nil {
		return nil, nil, fmt.Errorf("sign request error: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("http request [%s %s] failed: %w", method, urlStr, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := readLimitedBody(resp.Body, maxOCIResponseBytes)
	if err != nil {
		return resp, nil, fmt.Errorf("read response body error: %w", err)
	}

	return resp, respBytes, nil
}

func readLimitedBody(body io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("response body exceeded %d byte limit", maxBytes)
	}
	return data, nil
}

func summarizeOCIResponse(body []byte) string {
	const maxSummaryRunes = 4000
	text := strings.TrimSpace(string(body))
	runes := []rune(text)
	if len(runes) > maxSummaryRunes {
		return string(runes[:maxSummaryRunes]) + "…(已截断)"
	}
	return text
}
