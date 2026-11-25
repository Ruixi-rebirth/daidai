package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/config"
	"daidai/internal/db"
	"daidai/internal/entity"
)

type ociSchedulerState struct {
	LastDailySentDay          string          `json:"last_daily_sent_day"`
	DailyAttemptsDay          string          `json:"daily_attempts_day"`
	DailyAttempts             int             `json:"daily_attempts"`
	DailyLastAttemptAt        int64           `json:"daily_last_attempt_at"`
	LastCostSuccessBucket     string          `json:"last_cost_success_bucket"`
	LastCostAttemptBucket     string          `json:"last_cost_attempt_bucket"`
	LastCostAttemptAt         int64           `json:"last_cost_attempt_at"`
	LastResourceSuccessBucket string          `json:"last_resource_success_bucket"`
	LastResourceAttemptBucket string          `json:"last_resource_attempt_bucket"`
	LastResourceAttemptAt     int64           `json:"last_resource_attempt_at"`
	AlertedUTCDate            string          `json:"alerted_utc_date"`
	AlertedCost               float64         `json:"alerted_cost"`
	AlertedRisk               map[string]bool `json:"alerted_risk"`
}

func loadOCISchedulerState(ctx context.Context, database *db.Manager) ociSchedulerState {
	state := ociSchedulerState{AlertedRisk: make(map[string]bool)}
	loadCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	stored, err := database.GetRuntimeState(loadCtx, "qq", "oci_scheduler")
	if err != nil {
		slog.Warn("oci_scheduler: 读取持久化调度状态失败，将使用空状态", "err", err)
	} else if stored != "" {
		if err := json.Unmarshal([]byte(stored), &state); err != nil {
			slog.Warn("oci_scheduler: 持久化调度状态格式无效，将使用空状态", "err", err)
			state = ociSchedulerState{}
		}
	}
	if state.AlertedRisk == nil {
		state.AlertedRisk = make(map[string]bool)
	}
	return state
}

func persistOCISchedulerState(database *db.Manager, state ociSchedulerState) {
	data, err := json.Marshal(state)
	if err != nil {
		slog.Error("oci_scheduler: 序列化调度状态失败", "err", err)
		return
	}
	saveCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := database.SetRuntimeState(saveCtx, "qq", "oci_scheduler", string(data)); err != nil {
		slog.Error("oci_scheduler: 保存调度状态失败", "err", err)
	}
}

// RunScheduler 启动 OCI 每日账单与六小时/每小时资源防扣费巡检后台循环。
func RunScheduler(ctx context.Context, ociCfg config.OCIConfig, recipient string, channels *entity.ChannelRegistry, client *OCIClient, database *db.Manager) {
	if client == nil || channels == nil || recipient == "" || database == nil {
		slog.Warn("oci_scheduler: 客户端、QQ 通道、接收者或数据库缺失，后台定时任务未启动")
		return
	}
	loc, err := time.LoadLocation(strings.TrimSpace(ociCfg.Timezone))
	if err != nil {
		loc = time.FixedZone("SGT", 8*3600)
	}
	pushHour, pushMinute := 8, 30
	if ociCfg.DailyReportHour != nil {
		pushHour = *ociCfg.DailyReportHour
	}
	if ociCfg.DailyReportMinute != nil {
		pushMinute = *ociCfg.DailyReportMinute
	}
	slog.Info("oci_scheduler: 定时账单和资源巡检已启动", "pushTime", fmt.Sprintf("%02d:%02d", pushHour, pushMinute), "timezone", loc.String())

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	state := loadOCISchedulerState(ctx, database)
	persistState := func() { persistOCISchedulerState(database, state) }
	alertCollector := ociAlertCollector{config: ociCfg, location: loc, client: client, state: &state, persist: persistState}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().In(loc)
			localDay := now.Format("2006-01-02")
			utcDay := now.UTC().Format("2006-01-02")
			if state.DailyAttemptsDay != localDay {
				state.DailyAttemptsDay = localDay
				state.DailyAttempts = 0
				state.DailyLastAttemptAt = 0
			}
			if utcDay != state.AlertedUTCDate {
				state.AlertedUTCDate = utcDay
				state.AlertedCost = 0
				state.AlertedRisk = make(map[string]bool)
			}

			resourceBucket := fmt.Sprintf("%s-%d", utcDay, now.UTC().Hour()/6)
			var scannedResources *OCIBillReport
			dailyDue := now.Hour() > pushHour || (now.Hour() == pushHour && now.Minute() >= pushMinute)
			dailyRetryReady := state.DailyLastAttemptAt == 0 || now.Sub(time.UnixMilli(state.DailyLastAttemptAt)) >= 5*time.Minute
			if dailyDue && state.LastDailySentDay != localDay && state.DailyAttempts < 3 && dailyRetryReady {
				state.DailyAttempts++
				state.DailyLastAttemptAt = now.UnixMilli()
				state.LastResourceAttemptBucket = resourceBucket
				state.LastResourceAttemptAt = now.UnixMilli()
				persistState()
				report, err := FetchBillReport(ctx, client, loc)
				if err != nil {
					if state.DailyAttempts >= 3 {
						slog.Error("oci_scheduler: 获取每日账单连续失败 3 次，停止当天重试", "err", err)
					} else {
						slog.Error("oci_scheduler: 获取每日账单失败，稍后重试", "attempt", state.DailyAttempts, "err", err)
					}
				} else {
					scannedResources = report
					state.LastResourceSuccessBucket = resourceBucket
					persistState()
					deliverOCIDailyReport(ctx, database, channels, recipient, localDay, report, &state, persistState)
				}
			}

			hourKey := now.UTC().Format("2006-01-02-15")
			findings := alertCollector.collect(ctx, now, hourKey, utcDay, resourceBucket, scannedResources)
			if len(findings.reasons) == 0 {
				continue
			}
			deliverOCIAlert(ctx, database, channels, recipient, hourKey, findings.reasons, findings.keys, findings.costValue, &state, persistState)
		}
	}
}
