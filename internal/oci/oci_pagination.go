package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

func listOCIResources[T any](ctx context.Context, client *OCIClient, endpoint string) ([]T, error) {
	pageURL := endpoint
	seenPages := make(map[string]struct{})
	var all []T
	var totalBytes int64
	for page := 0; page < 1000; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resp, body, err := client.Do(ctx, http.MethodGet, pageURL, nil)
		if err != nil {
			return nil, fmt.Errorf("OCI list request failed: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("OCI list request returned HTTP %d: %s", resp.StatusCode, summarizeOCIResponse(body))
		}
		totalBytes += int64(len(body))
		if totalBytes > maxOCIPaginatedBytes {
			return nil, fmt.Errorf("OCI list response exceeded %d byte aggregate limit", maxOCIPaginatedBytes)
		}
		var items []T
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, fmt.Errorf("decode OCI list response: %w", err)
		}
		all = append(all, items...)

		next := strings.TrimSpace(resp.Header.Get("opc-next-page"))
		if next == "" {
			return all, nil
		}
		if _, exists := seenPages[next]; exists {
			return nil, fmt.Errorf("OCI list response repeated a pagination token")
		}
		seenPages[next] = struct{}{}
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return nil, err
		}
		query := parsed.Query()
		query.Set("page", next)
		parsed.RawQuery = query.Encode()
		pageURL = parsed.String()
	}
	return nil, fmt.Errorf("OCI list exceeded 1000 pages")
}
