package qq

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	tokenURL   = "https://bots.qq.com/app/getAppAccessToken"
	apiBaseURL = "https://api.sgroup.qq.com"
)

// Client 负责与 QQ 开放平台 OpenAPI 进行鉴权和 REST API 通信
type Client struct {
	appID      string
	secret     string
	httpClient *http.Client

	mu          sync.RWMutex
	refreshMu   sync.Mutex
	accessToken string
	expiresAt   time.Time

	msgSeq        uint32
	onMessageSent func(msgID, content string)
}

// SetOnMessageSent 注册发送成功后的消息记录回调
func (c *Client) SetOnMessageSent(fn func(msgID, content string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onMessageSent = fn
}

func (c *Client) notifySent(respBytes []byte, content string) {
	c.mu.RLock()
	fn := c.onMessageSent
	c.mu.RUnlock()

	if fn == nil || len(respBytes) == 0 {
		return
	}
	var res struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBytes, &res); err == nil && res.ID != "" {
		slog.Info("qq: 成功登记出站消息 ID", "id", res.ID)
		fn(res.ID, content)
	} else {
		slog.Warn("qq: 发送成功但响应未解析出 id", "raw", string(respBytes))
	}
}

// NewClient 创建并初始化 QQ API 客户端实例
func NewClient(appID, secret string) *Client {
	// 动态高位初始序号，避免进程热重启后 msg_seq 与腾讯端缓存冲突 (40054005)
	initSeq := uint32(time.Now().UnixNano()%800000 + 100000)
	return &Client{
		appID:  appID,
		secret: secret,
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
		msgSeq: initSeq,
	}
}

func (c *Client) nextSeq() uint32 {
	seq := atomic.AddUint32(&c.msgSeq, 1)
	if seq == 0 || seq > 10000000 {
		atomic.StoreUint32(&c.msgSeq, 100000)
		seq = 100000
	}
	return seq
}

// StartTokenRefresher 启动后台周期性检查并刷新 AccessToken 的协程
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

				if time.Now().After(exp) {
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
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.mu.RLock()
	if time.Until(c.expiresAt) > 2*time.Minute && c.accessToken != "" {
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("token request returned status %d: %s", resp.StatusCode, string(body))
	}

	var res struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   any    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return err
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
func (c *Client) UploadMedia(ctx context.Context, targetType, targetID string, fileType int, data []byte) (string, error) {
	url := fmt.Sprintf("%s/v2/%s/%s/files", apiBaseURL, targetType, targetID)
	payload := map[string]any{
		"file_type":    fileType,
		"file_data":    base64.StdEncoding.EncodeToString(data),
		"srv_send_msg": false,
	}

	respBytes, err := c.doPostJSON(ctx, url, payload)
	if err != nil {
		return "", fmt.Errorf("upload media failed: %w", err)
	}

	var res struct {
		FileInfo string `json:"file_info"`
		FileUUID string `json:"file_uuid"`
	}
	if err := json.Unmarshal(respBytes, &res); err != nil {
		return "", fmt.Errorf("failed to parse media upload response: %w", err)
	}
	if res.FileInfo == "" {
		return "", fmt.Errorf("upload media returned empty file_info: %s", string(respBytes))
	}

	return res.FileInfo, nil
}

// SendGroupMessage 发送 QQ 群聊消息 (优先采用 msg_type: 2 Markdown 格式，失败则平滑降级为 msg_type: 0 纯文本)
func (c *Client) SendGroupMessage(ctx context.Context, groupOpenID string, content string, msgID string) error {
	return c.sendMessage(ctx, "groups", groupOpenID, content, msgID)
}

// SendC2CMessage 发送 QQ 私聊消息 (优先采用 msg_type: 2 Markdown 格式，失败则平滑降级为 msg_type: 0 纯文本)
func (c *Client) SendC2CMessage(ctx context.Context, userOpenID string, content string, msgID string) error {
	return c.sendMessage(ctx, "users", userOpenID, content, msgID)
}

func (c *Client) sendMessage(ctx context.Context, targetType, targetID, content, msgID string) error {
	url := fmt.Sprintf("%s/v2/%s/%s/messages", apiBaseURL, targetType, targetID)

	mdPayload := map[string]any{
		"msg_type": 2,
		"markdown": map[string]string{
			"content": content,
		},
		"msg_seq": c.nextSeq(),
	}
	if msgID != "" {
		mdPayload["msg_id"] = msgID
	}

	resp, err := c.postJSONWithFallback(ctx, url, mdPayload, "msg_id")
	if err == nil {
		c.notifySent(resp, content)
		return nil
	}

	slog.Warn("qq api markdown 消息发送失败，自动降级为普通文本格式", "targetType", targetType, "err", err)

	textPayload := map[string]any{
		"content":  content,
		"msg_type": 0,
		"msg_seq":  c.nextSeq(),
	}
	if msgID != "" {
		textPayload["msg_id"] = msgID
	}

	resp, err = c.postJSONWithFallback(ctx, url, textPayload, "msg_id")
	if err == nil {
		c.notifySent(resp, content)
	}
	return err
}

// SendGroupMediaMessage 发送 QQ 群聊富媒体消息
func (c *Client) SendGroupMediaMessage(ctx context.Context, groupOpenID string, content string, imageData []byte, msgID string) error {
	return c.sendMediaMessage(ctx, "groups", groupOpenID, content, imageData, msgID)
}

// SendC2CMediaMessage 发送 QQ 私聊富媒体消息
func (c *Client) SendC2CMediaMessage(ctx context.Context, userOpenID string, content string, imageData []byte, msgID string) error {
	return c.sendMediaMessage(ctx, "users", userOpenID, content, imageData, msgID)
}

func (c *Client) sendMediaMessage(ctx context.Context, targetType, targetID, content string, imageData []byte, msgID string) error {
	fileInfo, err := c.UploadMedia(ctx, targetType, targetID, 1, imageData)
	if err != nil {
		return fmt.Errorf("%s image upload error: %w", targetType, err)
	}

	url := fmt.Sprintf("%s/v2/%s/%s/messages", apiBaseURL, targetType, targetID)
	payload := map[string]any{
		"msg_type": 7,
		"media": map[string]string{
			"file_info": fileInfo,
		},
		"msg_seq": c.nextSeq(),
	}
	if content != "" {
		payload["content"] = content
	}
	if msgID != "" {
		payload["msg_id"] = msgID
	}

	resp, err := c.postJSONWithFallback(ctx, url, payload, "msg_id")
	if err == nil {
		c.notifySent(resp, content)
	}
	return err
}

func (c *Client) postJSONWithFallback(ctx context.Context, url string, payload map[string]any, fallbackKey string) ([]byte, error) {
	respBytes, err := c.doPostJSON(ctx, url, payload)
	if err != nil && fallbackKey != "" {
		if _, hasKey := payload[fallbackKey]; hasKey {
			slog.Warn("qq api 被动消息发送失败，尝试降级为主动消息重发", "url", url, "err", err)
			fallbackPayload := make(map[string]any, len(payload))
			for k, v := range payload {
				if k != fallbackKey {
					fallbackPayload[k] = v
				}
			}
			fallbackPayload["msg_seq"] = c.nextSeq()
			var retryErr error
			respBytes, retryErr = c.doPostJSON(ctx, url, fallbackPayload)
			if retryErr == nil {
				slog.Info("qq api 降级主动消息重发成功", "url", url)
				return respBytes, nil
			}
		}
	}
	return respBytes, err
}

func (c *Client) doPostJSON(ctx context.Context, url string, data any) ([]byte, error) {
	body, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "QQBot "+c.Token())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		slog.Warn("qq api 返回 401 未授权，尝试即时刷新 token 并重发请求...", "url", url)
		if refErr := c.refreshToken(ctx); refErr == nil {
			retryReq, rErr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
			if rErr == nil {
				retryReq.Header.Set("Content-Type", "application/json")
				retryReq.Header.Set("Authorization", "QQBot "+c.Token())
				retryResp, doErr := c.httpClient.Do(retryReq)
				if doErr == nil {
					defer retryResp.Body.Close()
					retryBytes, _ := io.ReadAll(retryResp.Body)
					if retryResp.StatusCode >= 200 && retryResp.StatusCode < 300 {
						slog.Info("qq api 401 刷新 token 后重发成功", "url", url)
						return retryBytes, nil
					}
					respBytes = retryBytes
					resp.StatusCode = retryResp.StatusCode
				}
			}
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return respBytes, fmt.Errorf("api request failed [status=%d]: %s", resp.StatusCode, string(respBytes))
	}

	return respBytes, nil
}
