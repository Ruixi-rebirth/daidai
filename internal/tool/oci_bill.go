package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/engine/render"
	"daidai/internal/entity"
)

// OCIBillTool OCI 账单与防扣费安全巡检工具
type OCIBillTool struct {
	client *OCIClient
	loc    *time.Location
}

func NewOCIBillTool(client *OCIClient, timezone string) *OCIBillTool {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.FixedZone("SGT", 8*3600)
	}
	return &OCIBillTool{
		client: client,
		loc:    loc,
	}
}

func (t *OCIBillTool) Name() string {
	return "oci_bill"
}

func (t *OCIBillTool) Description() string {
	return "查询 Oracle Cloud (OCI) 本月 tenancy 账单，以及配置区域内的防扣费资源巡检（按 UTC 日统计费用、引导卷与块存储占用、闲置预留 IP）。当搭档询问甲骨文账单、扣费情况或云资源巡检时调用。"
}

func (t *OCIBillTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"render_card": map[string]any{
				"type":        "boolean",
				"description": "根据报告内容和用户偏好决定是否直接渲染账单卡片：用户明确要求图片，或账单概览/费用对比更适合图表式浏览时设为 true；简单结果或用户要纯文字时设为 false。未指定时返回文字，由对话继续判断是否调用 render_card",
			},
		},
	}
}

type ociBillArgs struct {
	RenderCard *bool `json:"render_card"`
}

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

type ociAvailabilityDomain struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

type ociCompartmentItem struct {
	ID             string `json:"id"`
	LifecycleState string `json:"lifecycleState"`
}

const maxOCITenancyCompartments = 1000

type ociVolumeItem struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName"`
	SizeInGBs      int64  `json:"sizeInGBs"`
	LifecycleState string `json:"lifecycleState"`
}

type ociPublicIPItem struct {
	ID             string `json:"id"`
	IPAddress      string `json:"ipAddress"`
	DisplayName    string `json:"displayName"`
	Lifetime       string `json:"lifetime"`
	PrivateIPID    string `json:"privateIpId"`
	LifecycleState string `json:"lifecycleState"`
	TimeCreated    string `json:"timeCreated"`
}

// OCIBillReport 汇总报告结构
type OCIBillReport struct {
	Date          string
	MonthTotal    float64
	TodayCost     float64
	YesterdayCost float64
	Currency      string
	ByService     map[string]float64
	NonzeroSKUs   []string
	StorageGB     int64
	VolumeCount   int
	UnboundIPs    []string
	BoundIPs      []string
	HasRisk       bool
	Markdown      string
}

type OCICostSummary struct {
	Date       string
	TodayCost  float64
	MonthTotal float64
	Currency   string
}

func SummarizeIPList(ips []string, limit int) string {
	if len(ips) == 0 {
		return "无"
	}
	if limit <= 0 || limit > len(ips) {
		limit = len(ips)
	}
	visible := strings.Join(ips[:limit], ", ")
	if remaining := len(ips) - limit; remaining > 0 {
		visible += fmt.Sprintf("，另有 %d 个未列出", remaining)
	}
	return visible
}

// FetchOCICostSummary checks billing data without scanning every compartment's volumes and IPs.
func FetchOCICostSummary(ctx context.Context, client *OCIClient) (*OCICostSummary, error) {
	if client == nil {
		return nil, fmt.Errorf("OCI client 未初始化")
	}
	nowUTC := time.Now().UTC()
	monthStart := time.Date(nowUTC.Year(), nowUTC.Month(), 1, 0, 0, 0, 0, time.UTC)
	tomorrowStart := time.Date(nowUTC.Year(), nowUTC.Month(), nowUTC.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
	endpoint := fmt.Sprintf("https://usageapi.%s.oci.oraclecloud.com/20200107/usage", client.region)
	body := map[string]any{
		"tenantId":         client.tenancyOCID,
		"timeUsageStarted": monthStart.Format("2006-01-02T15:04:05.000Z"),
		"timeUsageEnded":   tomorrowStart.Format("2006-01-02T15:04:05.000Z"),
		"granularity":      "DAILY",
		"queryType":        "COST",
		"groupBy":          []string{"service"},
	}
	var (
		pageToken string
		seen      = make(map[string]struct{})
		bytesRead int64
		total     float64
		today     float64
		currency  string
	)
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
		resp, data, err := client.Do(ctx, http.MethodPost, pageURL, body)
		if err != nil {
			return nil, fmt.Errorf("request OCI usage API error: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("OCI usage API returned code %d: %s", resp.StatusCode, summarizeOCIResponse(data))
		}
		bytesRead += int64(len(data))
		if bytesRead > maxOCIPaginatedBytes {
			return nil, fmt.Errorf("OCI usage response exceeded %d byte aggregate limit", maxOCIPaginatedBytes)
		}
		var result ociUsageResponse
		if err := json.Unmarshal(data, &result); err != nil {
			return nil, fmt.Errorf("unmarshal usage response: %w", err)
		}
		for _, item := range result.Items {
			started := item.TimeUsageStarted.UTC()
			if item.TimeUsageStarted.IsZero() || started.Before(monthStart) || !started.Before(tomorrowStart) {
				return nil, fmt.Errorf("OCI usage API returned a record outside the requested UTC range")
			}
			itemCurrency := strings.TrimSpace(item.Currency)
			if itemCurrency == "" && item.ComputedAmount != 0 {
				return nil, fmt.Errorf("OCI usage item has a nonzero amount without a currency")
			}
			if itemCurrency != "" {
				if currency != "" && currency != itemCurrency {
					return nil, fmt.Errorf("OCI usage API returned mixed currencies %q and %q", currency, itemCurrency)
				}
				currency = itemCurrency
			}
			total += item.ComputedAmount
			if started.Format("2006-01-02") == nowUTC.Format("2006-01-02") {
				today += item.ComputedAmount
			}
		}
		pageToken = strings.TrimSpace(resp.Header.Get("opc-next-page"))
		if pageToken == "" {
			break
		}
		if _, exists := seen[pageToken]; exists {
			return nil, fmt.Errorf("OCI usage API returned a repeated pagination token")
		}
		seen[pageToken] = struct{}{}
		if page == 999 {
			return nil, fmt.Errorf("OCI usage API exceeded 1000 pages")
		}
	}
	if currency == "" {
		currency = "未知"
	}
	return &OCICostSummary{Date: nowUTC.Format("2006-01-02"), TodayCost: today, MonthTotal: total, Currency: currency}, nil
}

// FetchBillReport 从 OCI API 查询生成完整账单报告
func FetchBillReport(ctx context.Context, client *OCIClient, loc *time.Location) (*OCIBillReport, error) {
	if client == nil {
		return nil, fmt.Errorf("OCI client 未初始化")
	}
	if loc == nil {
		loc = time.FixedZone("SGT", 8*3600)
	}

	now := time.Now()
	nowUTC := now.UTC()
	monthStart := time.Date(nowUTC.Year(), nowUTC.Month(), 1, 0, 0, 0, 0, time.UTC)
	tomorrowStart := time.Date(nowUTC.Year(), nowUTC.Month(), nowUTC.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)

	// 1. 请求 Usage API 汇总
	usageURL := fmt.Sprintf("https://usageapi.%s.oci.oraclecloud.com/20200107/usage", client.region)
	usageReqBody := map[string]any{
		"tenantId":         client.tenancyOCID,
		"timeUsageStarted": monthStart.Format("2006-01-02T15:04:05.000Z"),
		"timeUsageEnded":   tomorrowStart.Format("2006-01-02T15:04:05.000Z"),
		"granularity":      "DAILY",
		"queryType":        "COST",
		"groupBy":          []string{"service", "skuName"},
	}

	var pageToken string
	seenUsagePages := make(map[string]struct{})
	var usageItems []ociUsageItem
	var usageBytes int64
	for page := 0; page < 1000; page++ {
		pageURL := usageURL
		if pageToken != "" {
			parsed, err := url.Parse(usageURL)
			if err != nil {
				return nil, err
			}
			query := parsed.Query()
			query.Set("page", pageToken)
			parsed.RawQuery = query.Encode()
			pageURL = parsed.String()
		}
		resp, respBytes, err := client.Do(ctx, http.MethodPost, pageURL, usageReqBody)
		if err != nil {
			return nil, fmt.Errorf("request OCI usage API error: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("OCI usage API returned code %d: %s", resp.StatusCode, summarizeOCIResponse(respBytes))
		}
		usageBytes += int64(len(respBytes))
		if usageBytes > maxOCIPaginatedBytes {
			return nil, fmt.Errorf("OCI usage response exceeded %d byte aggregate limit", maxOCIPaginatedBytes)
		}
		var uResp ociUsageResponse
		if err := json.Unmarshal(respBytes, &uResp); err != nil {
			return nil, fmt.Errorf("unmarshal usage response: %w", err)
		}
		usageItems = append(usageItems, uResp.Items...)
		pageToken = strings.TrimSpace(resp.Header.Get("opc-next-page"))
		if pageToken == "" {
			break
		}
		if _, exists := seenUsagePages[pageToken]; exists {
			return nil, fmt.Errorf("OCI usage API returned a repeated pagination token")
		}
		seenUsagePages[pageToken] = struct{}{}
		if page == 999 {
			return nil, fmt.Errorf("OCI usage API exceeded 1000 pages")
		}
	}

	byService := make(map[string]float64)
	byDay := make(map[string]float64)
	var nonzeroSKUs []string
	currency := ""
	var monthTotal float64

	for _, item := range usageItems {
		amt := item.ComputedAmount
		itemCurrency := strings.TrimSpace(item.Currency)
		if itemCurrency == "" {
			if amt != 0 {
				return nil, fmt.Errorf("OCI usage item has a nonzero amount without a currency")
			}
		} else if currency == "" {
			currency = itemCurrency
		} else if currency != itemCurrency {
			return nil, fmt.Errorf("OCI usage API returned mixed currencies %q and %q; refusing to combine them", currency, itemCurrency)
		}
		started := item.TimeUsageStarted.UTC()
		if item.TimeUsageStarted.IsZero() || started.Before(monthStart) || !started.Before(tomorrowStart) {
			return nil, fmt.Errorf("OCI usage API returned a record outside the requested UTC range")
		}
		svc := item.Service
		if svc == "" {
			svc = "Other"
		}
		dayStr := started.Format("2006-01-02")
		byService[svc] += amt
		byDay[dayStr] += amt
		monthTotal += amt

		if amt > 0.00001 {
			sku := item.SkuName
			if sku == "" {
				sku = svc
			}
			nonzeroSKUs = append(nonzeroSKUs, fmt.Sprintf("%s %s: %.4f %s", dayStr, sku, amt, itemCurrency))
		}
	}
	if currency == "" {
		currency = "未知"
	}

	todayStr := nowUTC.Format("2006-01-02")
	yesterdayStr := nowUTC.Add(-24 * time.Hour).Format("2006-01-02")
	todayCost := byDay[todayStr]
	yesterdayCost := byDay[yesterdayStr]

	// 2. 存储容量巡检 (Boot Volumes & Block Volumes)
	var totalGB int64
	var volCount int

	adURL := fmt.Sprintf("https://identity.%s.oraclecloud.com/20160918/availabilityDomains?compartmentId=%s",
		client.region, url.QueryEscape(client.tenancyOCID))
	ads, err := listOCIResources[ociAvailabilityDomain](ctx, client, adURL)
	if err != nil {
		return nil, fmt.Errorf("查询可用域失败，无法完成存储巡检: %w", err)
	}
	compartments, err := listTenancyCompartmentIDs(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("查询 tenancy compartments 失败，无法完成资源巡检: %w", err)
	}
	for _, compartmentID := range compartments {
		for _, ad := range ads {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			bvURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/bootVolumes?availabilityDomain=%s&compartmentId=%s",
				client.region, url.QueryEscape(ad.Name), url.QueryEscape(compartmentID))
			bvs, err := listOCIResources[ociVolumeItem](ctx, client, bvURL)
			if err != nil {
				return nil, fmt.Errorf("查询启动卷失败 (compartment %s, availability domain %s): %w", compartmentID, ad.Name, err)
			}
			for _, bv := range bvs {
				if bv.LifecycleState != "TERMINATED" && bv.LifecycleState != "TERMINATING" {
					totalGB += bv.SizeInGBs
					volCount++
				}
			}
		}

		volURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/volumes?compartmentId=%s", client.region, url.QueryEscape(compartmentID))
		vols, err := listOCIResources[ociVolumeItem](ctx, client, volURL)
		if err != nil {
			return nil, fmt.Errorf("查询块存储卷失败 (compartment %s): %w", compartmentID, err)
		}
		for _, v := range vols {
			if v.LifecycleState != "TERMINATED" && v.LifecycleState != "TERMINATING" {
				totalGB += v.SizeInGBs
				volCount++
			}
		}
	}

	// 3. 预留公网 IP 巡检
	var unboundIPs []string
	var boundIPs []string
	for _, compartmentID := range compartments {
		pipURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps?scope=REGION&compartmentId=%s&lifetime=RESERVED",
			client.region, url.QueryEscape(compartmentID))
		ips, err := listOCIResources[ociPublicIPItem](ctx, client, pipURL)
		if err != nil {
			return nil, fmt.Errorf("查询预留公网 IP 失败 (compartment %s)，无法完成风险巡检: %w", compartmentID, err)
		}
		for _, ip := range ips {
			if ip.PrivateIPID == "" {
				unboundIPs = append(unboundIPs, ip.IPAddress)
			} else {
				boundIPs = append(boundIPs, ip.IPAddress)
			}
		}
	}

	localDate := now.In(loc).Format("2006-01-02")
	todayUTCDate := nowUTC.Format("2006-01-02")
	yesterdayUTCDate := nowUTC.Add(-24 * time.Hour).Format("2006-01-02")
	hasRisk := todayCost > 0.0001 || totalGB > 200 || len(unboundIPs) > 0

	var lines []string
	lines = append(lines, fmt.Sprintf("### OCI 账单日报 (%s)", localDate))
	lines = append(lines, fmt.Sprintf("- **统计范围**: 账单为 tenancy 本月汇总；存储与预留 IP 巡检区域为 `%s`", client.region))
	lines = append(lines, fmt.Sprintf("- **本月累计**: `%.4f %s`", monthTotal, currency))
	lines = append(lines, fmt.Sprintf("- **费用日按 UTC 统计**: `%s` `%.4f %s` (前一 UTC 日 `%s`: `%.4f %s`)",
		todayUTCDate, todayCost, currency, yesterdayUTCDate, yesterdayCost, currency))
	lines = append(lines, "")
	lines = append(lines, "**服务明细 (本月)**")

	if len(byService) > 0 {
		var svcs []string
		for s := range byService {
			svcs = append(svcs, s)
		}
		sort.Strings(svcs)
		for _, s := range svcs {
			lines = append(lines, fmt.Sprintf("- %s: `%.4f %s`", s, byService[s], currency))
		}
	} else {
		lines = append(lines, "- 暂无账单记录")
	}

	if len(nonzeroSKUs) > 0 {
		lines = append(lines, "")
		lines = append(lines, "**非零扣费记录摘录（最多展示 5 条）**")
		startIdx := 0
		if len(nonzeroSKUs) > 5 {
			startIdx = len(nonzeroSKUs) - 5
		}
		for _, skuLine := range nonzeroSKUs[startIdx:] {
			lines = append(lines, fmt.Sprintf("- %s", strings.TrimSpace(skuLine)))
		}
	}

	lines = append(lines, "")
	lines = append(lines, "**防扣费巡检**")
	storageWarn := ""
	if totalGB > 200 {
		storageWarn = " **[超免费额度!]**"
	}
	lines = append(lines, fmt.Sprintf("- **存储占用**: `%d/200 GB` (%d 个卷)%s", totalGB, volCount, storageWarn))

	if len(unboundIPs) > 0 {
		lines = append(lines, fmt.Sprintf("- **闲置预留IP**: `%d` 个 (`%s`) — 建议释放以防产生计费", len(unboundIPs), SummarizeIPList(unboundIPs, 20)))
	} else {
		boundStr := SummarizeIPList(boundIPs, 20)
		lines = append(lines, fmt.Sprintf("- **闲置预留IP**: `0` 个 (在用: `%s`)", boundStr))
	}

	if hasRisk {
		lines = append(lines, "- **结论**: **存在潜在计费项，请留意！**")
	} else {
		lines = append(lines, "- **结论**: **正常 (今日 0 扣费)**")
	}

	return &OCIBillReport{
		Date:          localDate,
		MonthTotal:    monthTotal,
		TodayCost:     todayCost,
		YesterdayCost: yesterdayCost,
		Currency:      currency,
		ByService:     byService,
		NonzeroSKUs:   nonzeroSKUs,
		StorageGB:     totalGB,
		VolumeCount:   volCount,
		UnboundIPs:    unboundIPs,
		BoundIPs:      boundIPs,
		HasRisk:       hasRisk,
		Markdown:      strings.Join(lines, "\n"),
	}, nil
}

func listTenancyCompartmentIDs(ctx context.Context, client *OCIClient) ([]string, error) {
	endpoint := fmt.Sprintf("https://identity.%s.oci.oraclecloud.com/20160918/compartments?compartmentId=%s&compartmentIdInSubtree=true&accessLevel=ANY",
		client.region, url.QueryEscape(client.tenancyOCID))
	items, err := listOCIResources[ociCompartmentItem](ctx, client, endpoint)
	if err != nil {
		return nil, err
	}
	if len(items) > maxOCITenancyCompartments {
		return nil, fmt.Errorf("tenancy has more than %d compartments; refusing an incomplete resource scan", maxOCITenancyCompartments)
	}

	ids := []string{client.tenancyOCID}
	seen := map[string]struct{}{client.tenancyOCID: {}}
	for _, item := range items {
		if item.ID == "" || item.LifecycleState == "DELETED" {
			continue
		}
		if _, exists := seen[item.ID]; exists {
			continue
		}
		seen[item.ID] = struct{}{}
		ids = append(ids, item.ID)
	}
	return ids, nil
}

func (t *OCIBillTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	var args ociBillArgs
	if len(rawArgs) > 0 && string(rawArgs) != "{}" {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, fmt.Errorf("invalid oci_bill args: %w", err)
		}
	}

	report, err := FetchBillReport(ctx, t.client, t.loc)
	if err != nil {
		return nil, fmt.Errorf("fetch OCI bill report error: %w", err)
	}
	if event := entity.SensoryEventFromContext(ctx); event == nil || !event.IsPartnerPrivate() {
		report.Markdown = core.RedactSensitiveIPs(report.Markdown)
	}

	res := &core.ToolResult{Text: report.Markdown}
	if args.RenderCard != nil && *args.RenderCard {
		res.FallbackText = report.Markdown
		cardCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if pngBytes, err := render.RenderCard(cardCtx, report.Markdown); err == nil && len(pngBytes) > 0 {
			res.ImageData = pngBytes
		} else {
			slog.Warn("oci_bill: 账单卡片渲染失败，保留 Markdown 报告", "err", err)
		}
	}

	return res, nil
}
