package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/engine/render"
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
	return "查询 Oracle Cloud (OCI) 云服务实时账单与防扣费巡检报告（本月累计花费、今日费用、引导卷与块存储 200GB 免费限额占用、闲置预留 IP）。当搭档询问甲骨文账单、扣费情况或云资源巡检时调用。"
}

func (t *OCIBillTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"render_card": map[string]any{
				"type":        "boolean",
				"description": "是否同时排版渲染为卡片长图 (默认 false)",
			},
		},
	}
}

type ociBillArgs struct {
	RenderCard bool `json:"render_card"`
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

// FetchBillReport 从 OCI API 查询生成完整账单报告
func FetchBillReport(ctx context.Context, client *OCIClient, loc *time.Location) (*OCIBillReport, error) {
	if client == nil {
		return nil, fmt.Errorf("OCI client 未初始化")
	}
	if loc == nil {
		loc = time.FixedZone("SGT", 8*3600)
	}

	nowUTC := time.Now().UTC()
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

	resp, respBytes, err := client.Do(ctx, http.MethodPost, usageURL, usageReqBody)
	if err != nil {
		return nil, fmt.Errorf("request OCI usage API error: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OCI usage API returned code %d: %s", resp.StatusCode, string(respBytes))
	}

	var uResp ociUsageResponse
	if err := json.Unmarshal(respBytes, &uResp); err != nil {
		return nil, fmt.Errorf("unmarshal usage response: %w", err)
	}

	byService := make(map[string]float64)
	byDay := make(map[string]float64)
	var nonzeroSKUs []string
	currency := "SGD"
	var monthTotal float64

	for _, item := range uResp.Items {
		amt := item.ComputedAmount
		svc := item.Service
		if svc == "" {
			svc = "Other"
		}
		if item.Currency != "" {
			currency = item.Currency
		}
		dayStr := item.TimeUsageStarted.UTC().Format("2006-01-02")
		byService[svc] += amt
		byDay[dayStr] += amt
		monthTotal += amt

		if amt > 0.00001 {
			sku := item.SkuName
			if sku == "" {
				sku = svc
			}
			nonzeroSKUs = append(nonzeroSKUs, fmt.Sprintf("%s %s: %.4f %s", dayStr, sku, amt, currency))
		}
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
	if adResp, adBytes, err := client.Do(ctx, http.MethodGet, adURL, nil); err == nil && adResp.StatusCode == http.StatusOK {
		var ads []ociAvailabilityDomain
		if err := json.Unmarshal(adBytes, &ads); err == nil {
			for _, ad := range ads {
				bvURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/bootVolumes?availabilityDomain=%s&compartmentId=%s",
					client.region, url.QueryEscape(ad.Name), url.QueryEscape(client.tenancyOCID))
				if bvResp, bvBytes, err := client.Do(ctx, http.MethodGet, bvURL, nil); err == nil && bvResp.StatusCode == http.StatusOK {
					var bvs []ociVolumeItem
					if err := json.Unmarshal(bvBytes, &bvs); err == nil {
						for _, bv := range bvs {
							if bv.LifecycleState != "TERMINATED" && bv.LifecycleState != "TERMINATING" {
								totalGB += bv.SizeInGBs
								volCount++
							}
						}
					}
				}
			}
		}
	}

	volURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/volumes?compartmentId=%s", client.region, url.QueryEscape(client.tenancyOCID))
	if volResp, volBytes, err := client.Do(ctx, http.MethodGet, volURL, nil); err == nil && volResp.StatusCode == http.StatusOK {
		var vols []ociVolumeItem
		if err := json.Unmarshal(volBytes, &vols); err == nil {
			for _, v := range vols {
				if v.LifecycleState != "TERMINATED" && v.LifecycleState != "TERMINATING" {
					totalGB += v.SizeInGBs
					volCount++
				}
			}
		}
	}

	// 3. 预留公网 IP 巡检
	var unboundIPs []string
	var boundIPs []string
	pipURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps?scope=REGION&compartmentId=%s&lifetime=RESERVED",
		client.region, url.QueryEscape(client.tenancyOCID))
	if pipResp, pipBytes, err := client.Do(ctx, http.MethodGet, pipURL, nil); err == nil && pipResp.StatusCode == http.StatusOK {
		var ips []ociPublicIPItem
		if err := json.Unmarshal(pipBytes, &ips); err == nil {
			for _, ip := range ips {
				if ip.PrivateIPID == "" {
					unboundIPs = append(unboundIPs, ip.IPAddress)
				} else {
					boundIPs = append(boundIPs, ip.IPAddress)
				}
			}
		}
	}

	localDate := time.Now().In(loc).Format("2006-01-02")
	hasRisk := todayCost > 0.0001 || totalGB > 200 || len(unboundIPs) > 0

	var lines []string
	lines = append(lines, fmt.Sprintf("### OCI 账单日报 (%s)", localDate))
	lines = append(lines, fmt.Sprintf("- **本月累计**: `%.4f %s`", monthTotal, currency))
	lines = append(lines, fmt.Sprintf("- **今日费用**: `%.4f %s` (昨日: `%.4f %s`)", todayCost, currency, yesterdayCost, currency))
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
		lines = append(lines, "**非零扣费记录**")
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
		lines = append(lines, fmt.Sprintf("- **闲置预留IP**: `%d` 个 (`%s`) — 建议释放以防产生计费", len(unboundIPs), strings.Join(unboundIPs, ", ")))
	} else {
		boundStr := strings.Join(boundIPs, ", ")
		if boundStr == "" {
			boundStr = "无"
		}
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

func (t *OCIBillTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	var args ociBillArgs
	if len(rawArgs) > 0 && string(rawArgs) != "{}" {
		_ = json.Unmarshal(rawArgs, &args)
	}

	report, err := FetchBillReport(ctx, t.client, t.loc)
	if err != nil {
		return nil, fmt.Errorf("fetch OCI bill report error: %w", err)
	}

	res := &core.ToolResult{
		Text: report.Markdown,
	}

	if args.RenderCard {
		cardCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if pngBytes, err := render.RenderCard(cardCtx, report.Markdown); err == nil && len(pngBytes) > 0 {
			res.ImageData = pngBytes
		}
	}

	return res, nil
}
