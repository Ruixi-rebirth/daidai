package qq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

func (c *Client) StartTokenRefresher(ctx context.Context) error {
	if err := c.refreshToken(ctx); err != nil {
		return fmt.Errorf("initial token refresh failed: %w", err)
	}

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.mu.RLock()
				exp := c.expiresAt
				c.mu.RUnlock()

				if time.Until(exp) <= 2*time.Minute {
					if err := c.refreshToken(ctx); err != nil {
						slog.Error("failed to refresh qq access token", "err", err)
					}
				}
			}
		}
	}()
	return nil
}

func (c *Client) refreshToken(ctx context.Context) error {
	return c.refreshTokenForRejectedToken(ctx, "")
}

func (c *Client) refreshTokenForRejectedToken(ctx context.Context, rejectedToken string) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.mu.RLock()
	if rejectedToken != "" && c.accessToken != rejectedToken && c.accessToken != "" {
		c.mu.RUnlock()
		return nil
	}
	if rejectedToken == "" && time.Until(c.expiresAt) > 2*time.Minute && c.accessToken != "" {
		c.mu.RUnlock()
		return nil
	}
	c.mu.RUnlock()

	reqBody, _ := json.Marshal(map[string]string{
		"appId":        c.appID,
		"clientSecret": c.secret,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := readBoundedAPIResponse(resp.Body)
		return fmt.Errorf("token request returned status %d: %s", resp.StatusCode, summarizeAPIResponse(body))
	}

	var res struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   any    `json:"expires_in"`
		Code        int    `json:"code"`
		Message     string `json:"message"`
	}
	body, err := readBoundedAPIResponse(resp.Body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return err
	}
	if strings.TrimSpace(res.AccessToken) == "" {
		if res.Code != 0 || strings.TrimSpace(res.Message) != "" {
			return fmt.Errorf("token request rejected [code=%d]: %s", res.Code, summarizeAPIResponse([]byte(res.Message)))
		}
		return fmt.Errorf("token response did not contain an access_token")
	}

	expiresSec := 7200
	switch v := res.ExpiresIn.(type) {
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			expiresSec = n
		}
	case float64:
		expiresSec = int(v)
	}

	refreshAfter := expiresSec - 120
	if refreshAfter < 30 {
		refreshAfter = expiresSec / 2
	}
	if refreshAfter < 10 {
		refreshAfter = 10
	}

	c.mu.Lock()
	c.accessToken = res.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(refreshAfter) * time.Second)
	c.mu.Unlock()

	slog.Info("qq bot access token refreshed successfully", "expiresIn", expiresSec)
	return nil
}

// Token 获取当前有效的访问令牌
func (c *Client) Token() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.accessToken
}

// UploadMedia 上传富媒体文件至 QQ 开放平台并换取 file_info 凭据
