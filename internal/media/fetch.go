package media

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"daidai/internal/core"
)

// FetchImage resolves an opaque platform media ref via sources, or downloads a
// trusted external image URL with SSRF and size protections.
func FetchImage(ctx context.Context, sources *Sources, refOrURL string, timeout time.Duration) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if data, handled, err := sources.Fetch(ctx, refOrURL, core.MaxImageBytes); handled {
		return data, err
	}
	if err := core.ValidateTrustedMediaURL(refOrURL); err != nil {
		return nil, err
	}
	if err := core.ValidateSafeExternalURL(ctx, refOrURL); err != nil {
		return nil, fmt.Errorf("unsafe image URL: %w", err)
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, refOrURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	client := core.NewProtectedHTTPClient(timeout)
	defer client.CloseIdleConnections()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("stopped after 5 image redirects")
		}
		if err := core.ValidateTrustedMediaURL(req.URL.String()); err != nil {
			return fmt.Errorf("untrusted image redirect: %w", err)
		}
		if err := core.ValidateSafeExternalURL(req.Context(), req.URL.String()); err != nil {
			return fmt.Errorf("unsafe image redirect: %w", err)
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch image status: %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, core.MaxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > core.MaxImageBytes {
		return nil, fmt.Errorf("image exceeded %d byte limit", core.MaxImageBytes)
	}
	return data, nil
}
