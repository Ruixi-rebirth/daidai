package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	period, err := fetchOCIUsagePeriod(ctx, client, []string{"service"})
	if err != nil {
		return nil, err
	}
	nowUTC := period.NowUTC
	var total, today float64
	for _, item := range period.Items {
		started := item.TimeUsageStarted.UTC()
		total += item.ComputedAmount
		if started.Format("2006-01-02") == nowUTC.Format("2006-01-02") {
			today += item.ComputedAmount
		}
	}
	return &OCICostSummary{Date: nowUTC.Format("2006-01-02"), TodayCost: today, MonthTotal: total, Currency: period.Currency}, nil
}

// FetchBillReport 从 OCI API 查询生成完整账单报告
func FetchBillReport(ctx context.Context, client *OCIClient, loc *time.Location) (*OCIBillReport, error) {
	if loc == nil {
		loc = time.FixedZone("SGT", 8*3600)
	}

	// 1. 请求 Usage API 汇总
	period, err := fetchOCIUsagePeriod(ctx, client, []string{"service", "skuName"})
	if err != nil {
		return nil, err
	}
	usageItems := period.Items
	now := period.NowUTC
	nowUTC := period.NowUTC

	byService := make(map[string]float64)
	byDay := make(map[string]float64)
	var nonzeroSKUs []string
	currency := period.Currency
	var monthTotal float64

	for _, item := range usageItems {
		amt := item.ComputedAmount
		itemCurrency := strings.TrimSpace(item.Currency)
		started := item.TimeUsageStarted.UTC()
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
	todayStr := nowUTC.Format("2006-01-02")
	yesterdayStr := nowUTC.Add(-24 * time.Hour).Format("2006-01-02")
	todayCost := byDay[todayStr]
	yesterdayCost := byDay[yesterdayStr]

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
	totalGB, volCount, err := inspectOCIVolumes(ctx, client, compartments, ads)
	if err != nil {
		return nil, err
	}

	// 3. 预留公网 IP 巡检
	unboundIPs, boundIPs, err := inspectOCIReservedIPs(ctx, client, compartments)
	if err != nil {
		return nil, err
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
