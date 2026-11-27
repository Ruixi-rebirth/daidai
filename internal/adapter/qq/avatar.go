package qq

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"daidai/internal/core"
)

const qqAvatarBaseURL = "https://q.qlogo.cn/qqapp"

func isSafeQQOpenID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		isHexOrWord := (r >= '0' && r <= '9') ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			r == '_' || r == '-'
		if !isHexOrWord {
			return false
		}
	}
	return true
}

func isTrustedQLogoHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "q.qlogo.cn" || strings.HasSuffix(host, ".qlogo.cn")
}

// FetchAvatar resolves an opaque "qq-avatar:<openid>" media reference into image bytes
// without exposing the bot's AppID or avatar URL to the model or history.
func (q *QQChannel) FetchAvatar(ctx context.Context, ref string, limit int64) ([]byte, error) {
	if q == nil {
		return nil, fmt.Errorf("qq channel not configured")
	}
	openID, ok := strings.CutPrefix(strings.TrimSpace(ref), "qq-avatar:")
	openID = strings.TrimSpace(openID)
	if !ok || !isSafeQQOpenID(openID) {
		return nil, fmt.Errorf("invalid qq avatar reference")
	}
	appID := strings.TrimSpace(q.cfg.AppID)
	if !isSafeQQOpenID(appID) {
		return nil, fmt.Errorf("qq app_id is not configured")
	}
	if limit <= 0 {
		limit = core.MaxImageBytes
	}

	avatarURL := fmt.Sprintf("%s/%s/%s/640", qqAvatarBaseURL, appID, openID)
	if err := core.ValidateSafeExternalURL(ctx, avatarURL); err != nil {
		return nil, fmt.Errorf("unsafe qq avatar URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, avatarURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	client := core.NewProtectedHTTPClient(15 * time.Second)
	defer client.CloseIdleConnections()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("stopped after 5 avatar redirects")
		}
		if req.URL.Scheme != "https" || !isTrustedQLogoHost(req.URL.Hostname()) {
			return fmt.Errorf("untrusted qq avatar redirect host: %s", req.URL.Hostname())
		}
		if err := core.ValidateSafeExternalURL(req.Context(), req.URL.String()); err != nil {
			return fmt.Errorf("unsafe qq avatar redirect: %w", err)
		}
		return nil
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch qq avatar status: %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("qq avatar exceeds %d byte limit", limit)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("qq avatar response is empty")
	}
	return data, nil
}
