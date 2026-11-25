package oci

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"daidai/internal/config"
)

type ociAlertFindings struct {
	reasons   []string
	keys      []string
	costValue float64
}

type ociAlertCollector struct {
	config   config.OCIConfig
	location *time.Location
	client   *OCIClient
	state    *ociSchedulerState
	persist  func()
}

func (c *ociAlertCollector) collect(
	ctx context.Context,
	now time.Time,
	hourKey, utcDay, resourceBucket string,
	scannedResources *OCIBillReport,
) ociAlertFindings {
	findings := ociAlertFindings{}
	c.collectCost(ctx, now, hourKey, utcDay, &findings)
	resources := c.collectResources(ctx, now, resourceBucket, scannedResources)
	if resources == nil {
		return findings
	}
	if len(resources.UnboundIPs) > 0 && !c.state.AlertedRisk["unbound-ip"] {
		findings.keys = append(findings.keys, "unbound-ip")
		findings.reasons = append(findings.reasons, fmt.Sprintf("发现 %d 个闲置未绑定预留公网 IP (示例: %s)", len(resources.UnboundIPs), SummarizeIPList(resources.UnboundIPs, 10)))
	}
	if resources.StorageGB > 200 && !c.state.AlertedRisk["storage"] {
		findings.keys = append(findings.keys, "storage")
		findings.reasons = append(findings.reasons, fmt.Sprintf("存储占用 %d GB，超过 200 GB 免费额度", resources.StorageGB))
	}
	return findings
}

func (c *ociAlertCollector) collectCost(ctx context.Context, now time.Time, hourKey, utcDay string, findings *ociAlertFindings) {
	if c.state.LastCostSuccessBucket == hourKey || (c.state.LastCostAttemptBucket == hourKey && now.Sub(time.UnixMilli(c.state.LastCostAttemptAt)) < 5*time.Minute) {
		return
	}
	c.state.LastCostAttemptBucket = hourKey
	c.state.LastCostAttemptAt = now.UnixMilli()
	c.persist()
	cost, err := FetchOCICostSummary(ctx, c.client)
	if err != nil {
		slog.Warn("oci_scheduler: 每小时费用检查失败", "err", err)
		return
	}
	c.state.LastCostSuccessBucket = hourKey
	c.persist()
	if cost.TodayCost <= 0.0001 || (c.state.AlertedCost != 0 && cost.TodayCost < c.state.AlertedCost+c.config.CostAlertDelta) {
		return
	}
	findings.keys = append(findings.keys, "cost")
	findings.costValue = cost.TodayCost
	findings.reasons = append(findings.reasons, fmt.Sprintf("截至 UTC %s 当日累计费用 %.4f %s", utcDay, cost.TodayCost, cost.Currency))
}

func (c *ociAlertCollector) collectResources(
	ctx context.Context,
	now time.Time,
	resourceBucket string,
	previous *OCIBillReport,
) *OCIBillReport {
	if previous != nil {
		return previous
	}
	if c.state.LastResourceSuccessBucket == resourceBucket ||
		(c.state.LastResourceAttemptBucket == resourceBucket && now.Sub(time.UnixMilli(c.state.LastResourceAttemptAt)) < 5*time.Minute) {
		return nil
	}
	c.state.LastResourceAttemptBucket = resourceBucket
	c.state.LastResourceAttemptAt = now.UnixMilli()
	c.persist()
	report, err := FetchBillReport(ctx, c.client, c.location)
	if err != nil {
		slog.Warn("oci_scheduler: 六小时资源巡检失败", "err", err)
		return nil
	}
	c.state.LastResourceSuccessBucket = resourceBucket
	c.persist()
	return report
}
