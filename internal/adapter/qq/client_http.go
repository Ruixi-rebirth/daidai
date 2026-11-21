package qq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"daidai/internal/entity"
)

func (c *Client) doPostJSON(ctx context.Context, url string, data any) ([]byte, error) {
	return c.doJSON(ctx, http.MethodPost, url, data)
}

func (c *Client) doJSON(ctx context.Context, method, url string, data any) ([]byte, error) {
	var body []byte
	var err error
	if data != nil {
		body, err = json.Marshal(data)
		if err != nil {
			return nil, err
		}
	}

	newRequest := func(token string) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if data != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "QQBot "+token)
		return req, nil
	}
	usedToken := c.Token()
	req, err := newRequest(usedToken)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := readBoundedAPIResponse(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		slog.Warn("qq api 返回 401 未授权，尝试即时刷新 token 并重发请求...", "url", url)
		if refErr := c.refreshTokenForRejectedToken(ctx, usedToken); refErr == nil {
			retryReq, rErr := newRequest(c.Token())
			if rErr != nil {
				return nil, rErr
			}
			retryResp, doErr := c.httpClient.Do(retryReq)
			if doErr != nil {
				return nil, doErr
			}
			defer func() { _ = retryResp.Body.Close() }()
			retryBytes, readErr := readBoundedAPIResponse(retryResp.Body)
			if readErr != nil {
				return nil, readErr
			}
			if validateAPIResponse(retryResp.StatusCode, retryBytes) == nil {
				slog.Info("qq api 401 刷新 token 后重发成功", "url", url)
				return retryBytes, nil
			}
			respBytes = retryBytes
			resp.StatusCode = retryResp.StatusCode
			resp.Header = retryResp.Header
		} else {
			return nil, fmt.Errorf("refresh QQ token after 401: %w", refErr)
		}
	}

	if err := validateAPIResponse(resp.StatusCode, respBytes); err != nil {
		var rejection *entity.DeliveryRejection
		if errors.As(err, &rejection) && rejection.After > 0 {
			value := resp.Header.Get("Retry-After")
			if seconds, parseErr := strconv.ParseInt(value, 10, 32); parseErr == nil && seconds > 0 {
				rejection.After = max(rejection.After, time.Duration(seconds)*time.Second)
			} else if at, parseErr := http.ParseTime(value); parseErr == nil {
				rejection.After = max(rejection.After, time.Until(at))
			}
		}
		return respBytes, err
	}

	return respBytes, nil
}

func validateAPIResponse(status int, respBytes []byte) error {
	if status < 200 || status >= 300 {
		err := fmt.Errorf("api request failed [status=%d]: %s", status, summarizeAPIResponse(respBytes))
		if status >= 400 && status < 500 {
			delay := time.Duration(0)
			if status == 429 {
				delay = 5 * time.Minute
			}
			return &entity.DeliveryRejection{Err: err, After: delay}
		}
		return err
	}
	var apiError struct {
		ErrCode int    `json:"err_code"`
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBytes, &apiError); err == nil && (apiError.ErrCode != 0 || apiError.Code != 0) {
		code := apiError.ErrCode
		if code == 0 {
			code = apiError.Code
		}
		message := summarizeAPIResponse([]byte(apiError.Message))
		if message == "" {
			message = summarizeAPIResponse(respBytes)
		}
		return &entity.DeliveryRejection{Err: fmt.Errorf("api request rejected [code=%d]: %s", code, message)}
	}
	return nil
}

func readBoundedAPIResponse(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxAPIResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxAPIResponseBytes {
		return nil, fmt.Errorf("QQ API 响应超过 %d 字节上限", maxAPIResponseBytes)
	}
	return data, nil
}

func summarizeAPIResponse(data []byte) string {
	text := strings.TrimSpace(string(data))
	if runes := []rune(text); len(runes) > 4000 {
		return string(runes[:4000]) + "..."
	}
	return text
}
