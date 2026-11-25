package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type ociUsageItem struct {
	Service          string    `json:"service"`
	SkuName          string    `json:"skuName"`
	ComputedAmount   float64   `json:"computedAmount"`
	Currency         string    `json:"currency"`
	TimeUsageStarted time.Time `json:"timeUsageStarted"`
	TimeUsageEnded   time.Time `json:"timeUsageEnded"`
}

type ociUsageResponse struct {
	Items []ociUsageItem `json:"items"`
}

type ociUsagePeriod struct {
	Items         []ociUsageItem
	NowUTC        time.Time
	MonthStart    time.Time
	TomorrowStart time.Time
	Currency      string
}

func fetchOCIUsagePeriod(ctx context.Context, client *OCIClient, groupBy []string) (*ociUsagePeriod, error) {
	if client == nil {
		return nil, fmt.Errorf("OCI client 未初始化")
	}
	nowUTC := time.Now().UTC()
	monthStart := time.Date(nowUTC.Year(), nowUTC.Month(), 1, 0, 0, 0, 0, time.UTC)
	tomorrowStart := time.Date(nowUTC.Year(), nowUTC.Month(), nowUTC.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
	request := map[string]any{
		"tenantId":         client.tenancyOCID,
		"timeUsageStarted": monthStart.Format("2006-01-02T15:04:05.000Z"),
		"timeUsageEnded":   tomorrowStart.Format("2006-01-02T15:04:05.000Z"),
		"granularity":      "DAILY",
		"queryType":        "COST",
		"groupBy":          groupBy,
	}
	items, err := fetchOCIUsageItems(ctx, client, request)
	if err != nil {
		return nil, err
	}
	period := &ociUsagePeriod{
		Items:         items,
		NowUTC:        nowUTC,
		MonthStart:    monthStart,
		TomorrowStart: tomorrowStart,
	}
	for _, item := range items {
		started := item.TimeUsageStarted.UTC()
		if item.TimeUsageStarted.IsZero() || started.Before(monthStart) || !started.Before(tomorrowStart) {
			return nil, fmt.Errorf("OCI usage API returned a record outside the requested UTC range")
		}
		itemCurrency := strings.TrimSpace(item.Currency)
		if itemCurrency == "" && item.ComputedAmount != 0 {
			return nil, fmt.Errorf("OCI usage item has a nonzero amount without a currency")
		}
		if itemCurrency != "" {
			if period.Currency != "" && period.Currency != itemCurrency {
				return nil, fmt.Errorf("OCI usage API returned mixed currencies %q and %q", period.Currency, itemCurrency)
			}
			period.Currency = itemCurrency
		}
	}
	if period.Currency == "" {
		period.Currency = "未知"
	}
	return period, nil
}

func fetchOCIUsageItems(ctx context.Context, client *OCIClient, request map[string]any) ([]ociUsageItem, error) {
	endpoint := fmt.Sprintf("https://usageapi.%s.oci.oraclecloud.com/20200107/usage", client.region)
	pageToken := ""
	seenPages := make(map[string]struct{})
	var bytesRead int64
	var items []ociUsageItem

	for page := 0; page < 1000; page++ {
		pageURL := endpoint
		if pageToken != "" {
			parsed, err := url.Parse(endpoint)
			if err != nil {
				return nil, err
			}
			query := parsed.Query()
			query.Set("page", pageToken)
			parsed.RawQuery = query.Encode()
			pageURL = parsed.String()
		}

		resp, body, err := client.Do(ctx, http.MethodPost, pageURL, request)
		if err != nil {
			return nil, fmt.Errorf("request OCI usage API error: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("OCI usage API returned code %d: %s", resp.StatusCode, summarizeOCIResponse(body))
		}
		bytesRead += int64(len(body))
		if bytesRead > maxOCIPaginatedBytes {
			return nil, fmt.Errorf("OCI usage response exceeded %d byte aggregate limit", maxOCIPaginatedBytes)
		}

		var result ociUsageResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("unmarshal usage response: %w", err)
		}
		items = append(items, result.Items...)

		pageToken = strings.TrimSpace(resp.Header.Get("opc-next-page"))
		if pageToken == "" {
			return items, nil
		}
		if _, exists := seenPages[pageToken]; exists {
			return nil, fmt.Errorf("OCI usage API returned a repeated pagination token")
		}
		seenPages[pageToken] = struct{}{}
		if page == 999 {
			return nil, fmt.Errorf("OCI usage API exceeded 1000 pages")
		}
	}
	return items, nil
}
