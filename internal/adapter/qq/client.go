package qq

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"daidai/internal/core"
)

const (
	tokenURL            = "https://api.bot.qq.com/app/getAppAccessToken"
	apiBaseURL          = "https://api.bot.qq.com"
	maxAPIResponseBytes = 1 << 20
	maxQQMediaBytes     = core.MaxImageBytes
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
	onMessageSent func(msgID, refIndex, content, targetType, targetID string)
}

// SetOnMessageSent 注册发送成功后的消息记录回调
func (c *Client) SetOnMessageSent(fn func(msgID, refIndex, content, targetType, targetID string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onMessageSent = fn
}

func (c *Client) notifySent(respBytes []byte, content, targetType, targetID string) {
	c.mu.RLock()
	fn := c.onMessageSent
	c.mu.RUnlock()

	if fn == nil || len(respBytes) == 0 {
		return
	}
	var res struct {
		ID      string `json:"id"`
		ExtInfo struct {
			RefIndex string `json:"ref_idx"`
		} `json:"ext_info"`
	}
	if err := json.Unmarshal(respBytes, &res); err == nil && res.ID != "" {
		slog.Info("qq: 成功登记出站消息 ID", "id", res.ID, "hasRefIndex", res.ExtInfo.RefIndex != "")
		fn(res.ID, res.ExtInfo.RefIndex, content, targetType, targetID)
	} else {
		slog.Warn("qq: 发送成功但响应未包含可解析的消息 ID")
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
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		msgSeq: initSeq,
	}
}

func (c *Client) nextSeq() uint32 {
	for {
		current := atomic.LoadUint32(&c.msgSeq)
		next := current + 1
		if next == 0 || next > 10000000 {
			next = 100000
		}
		if atomic.CompareAndSwapUint32(&c.msgSeq, current, next) {
			return next
		}
	}
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
func (c *Client) UploadMedia(ctx context.Context, targetType, targetID string, fileType int, data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("upload media failed: image data is empty")
	}
	if len(data) > maxQQMediaBytes {
		return "", fmt.Errorf("upload media failed: image exceeds %d byte limit", maxQQMediaBytes)
	}
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
		return "", fmt.Errorf("upload media returned empty file_info: %s", summarizeAPIResponse(respBytes))
	}

	return res.FileInfo, nil
}

// UploadMarkdownImage uploads PNG data to QQ's material library and returns
// the platform-scoped Markdown reference and material ID for later cleanup.
func (c *Client) UploadMarkdownImage(ctx context.Context, data []byte) (string, string, error) {
	const maxMarkdownImageBytes = 10_000_000
	if len(data) == 0 || len(data) > maxMarkdownImageBytes {
		return "", "", fmt.Errorf("markdown image size must be between 1 and %d bytes", maxMarkdownImageBytes)
	}
	if !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return "", "", fmt.Errorf("markdown image must be PNG")
	}
	md5Sum := md5.Sum(data)
	sha1Sum := sha1.Sum(data)
	sha256Sum := sha256.Sum256(data)
	fileName := "reply-" + hex.EncodeToString(sha256Sum[:8]) + ".png"
	prepared, err := c.doPostJSON(ctx, apiBaseURL+"/v2/media-images/prepare", map[string]any{
		"file_name": fileName, "file_size": len(data), "content_type": "image/png",
		"md5": hex.EncodeToString(md5Sum[:]), "sha1": hex.EncodeToString(sha1Sum[:]),
	})
	if err != nil {
		return "", "", fmt.Errorf("prepare QQ Markdown image upload: %w", err)
	}
	var upload struct {
		UploadID     string `json:"upload_id"`
		PresignedURL string `json:"presigned_url"`
	}
	if err := json.Unmarshal(prepared, &upload); err != nil {
		return "", "", fmt.Errorf("parse QQ Markdown image upload: %w", err)
	}
	if upload.UploadID == "" || upload.PresignedURL == "" {
		return "", "", fmt.Errorf("QQ Markdown image upload response is missing upload_id or presigned_url")
	}
	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, upload.PresignedURL, bytes.NewReader(data))
	if err != nil {
		return "", "", err
	}
	putReq.Header.Set("Content-Type", "image/png")
	putResp, err := c.httpClient.Do(putReq)
	if err != nil {
		return "", "", fmt.Errorf("upload QQ Markdown image bytes: %w", err)
	}
	_, readErr := readBoundedAPIResponse(putResp.Body)
	_ = putResp.Body.Close()
	if readErr != nil {
		return "", "", readErr
	}
	if putResp.StatusCode < 200 || putResp.StatusCode >= 300 {
		return "", "", fmt.Errorf("QQ Markdown image storage returned status %d", putResp.StatusCode)
	}
	etag := strings.Trim(putResp.Header.Get("ETag"), "\" ")
	if etag == "" {
		return "", "", fmt.Errorf("QQ Markdown image storage response did not include ETag")
	}
	completed, err := c.doPostJSON(ctx, apiBaseURL+"/v2/media-images/complete", map[string]string{
		"upload_id": upload.UploadID, "md5": etag,
	})
	if err != nil {
		return "", "", fmt.Errorf("complete QQ Markdown image upload: %w", err)
	}
	var material struct {
		ImageID string `json:"image_id"`
		URL     string `json:"url"`
	}
	if err := json.Unmarshal(completed, &material); err != nil {
		return "", "", fmt.Errorf("parse completed QQ Markdown image: %w", err)
	}
	if material.ImageID == "" || !strings.HasPrefix(material.URL, "qqbot-media://image/v1/") {
		return "", "", fmt.Errorf("QQ returned an invalid Markdown image material")
	}
	return material.ImageID, material.URL, nil
}

// DeleteMarkdownImage releases a temporary material after its message is sent.
func (c *Client) DeleteMarkdownImage(ctx context.Context, imageID string) error {
	if imageID == "" {
		return nil
	}
	_, err := c.doJSON(ctx, http.MethodDelete, apiBaseURL+"/v2/media-images/"+url.PathEscape(imageID)+"/delete", nil)
	return err
}

// SendGroupMessage 发送 QQ 群聊 Markdown 消息。
func (c *Client) SendGroupMessage(ctx context.Context, groupOpenID string, content string, msgID string) error {
	return c.sendMessage(ctx, "groups", groupOpenID, content, msgID)
}

// SendC2CMessage 发送 QQ 私聊 Markdown 消息。
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

	resp, err := c.doPostJSON(ctx, url, mdPayload)
	if err == nil {
		c.notifySent(resp, content, targetType, targetID)
		return nil
	}
	return fmt.Errorf("send QQ markdown message: %w", err)
}

// SendGroupMediaMessage 发送 QQ 群聊富媒体消息
func (c *Client) SendGroupMediaMessage(ctx context.Context, groupOpenID string, content string, imageData []byte, msgID string) error {
	return c.sendMediaMessage(ctx, "groups", groupOpenID, content, imageData, msgID, content)
}

// SendC2CMediaMessage 发送 QQ 私聊富媒体消息
func (c *Client) SendC2CMediaMessage(ctx context.Context, userOpenID string, content string, imageData []byte, msgID string) error {
	return c.sendMediaMessage(ctx, "users", userOpenID, content, imageData, msgID, content)
}

// SendGroupMediaMessageWithContext associates the full text representation with
// the image message so a later quote can recover the card's content.
func (c *Client) SendGroupMediaMessageWithContext(ctx context.Context, groupOpenID, content string, imageData []byte, msgID, quoteContext string) error {
	return c.sendMediaMessage(ctx, "groups", groupOpenID, content, imageData, msgID, quoteContext)
}

// SendC2CMediaMessageWithContext associates the full text representation with
// the image message so a later quote can recover the card's content.
func (c *Client) SendC2CMediaMessageWithContext(ctx context.Context, userOpenID, content string, imageData []byte, msgID, quoteContext string) error {
	return c.sendMediaMessage(ctx, "users", userOpenID, content, imageData, msgID, quoteContext)
}

func (c *Client) sendMediaMessage(ctx context.Context, targetType, targetID, content string, imageData []byte, msgID, quoteContext string) error {
	fileInfo, err := c.UploadMedia(ctx, targetType, targetID, 1, imageData)
	if err != nil {
		return fmt.Errorf("%s image upload error: %w", targetType, err)
	}

	url := fmt.Sprintf("%s/v2/%s/%s/messages", apiBaseURL, targetType, targetID)
	payload := map[string]any{
		"msg_type": 7,
		// QQ v2 在富媒体消息中仍要求 content 字段，即使消息没有文字。
		"content": content,
		"media": map[string]string{
			"file_info": fileInfo,
		},
		"msg_seq": c.nextSeq(),
	}
	if content == "" {
		payload["content"] = " "
	}
	if msgID != "" {
		payload["msg_id"] = msgID
	}

	resp, err := c.doPostJSON(ctx, url, payload)
	if err == nil {
		cacheContent := strings.TrimSpace(quoteContext)
		if cacheContent == "" {
			cacheContent = content
		}
		c.notifySent(resp, cacheContent, targetType, targetID)
	}
	return err
}

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
		} else {
			return nil, fmt.Errorf("refresh QQ token after 401: %w", refErr)
		}
	}

	if err := validateAPIResponse(resp.StatusCode, respBytes); err != nil {
		return respBytes, err
	}

	return respBytes, nil
}

func validateAPIResponse(status int, respBytes []byte) error {
	if status < 200 || status >= 300 {
		return fmt.Errorf("api request failed [status=%d]: %s", status, summarizeAPIResponse(respBytes))
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
		return fmt.Errorf("api request rejected [code=%d]: %s", code, message)
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
