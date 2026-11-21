package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/db"
	"daidai/internal/entity"
)

func deliverOCIAlert(
	ctx context.Context,
	database *db.Manager,
	channels *entity.ChannelRegistry,
	recipient, hourKey string,
	reasons, alertKeys []string,
	costAlertValue float64,
	state *ociSchedulerState,
	persistState func(),
) {
	alert := fmt.Sprintf("[OCI 资源防扣费告警]\n- %s", strings.Join(reasons, "\n- "))
	dedupeSuffix := strings.Join(alertKeys, "+")
	if costAlertValue > 0 {
		dedupeSuffix += fmt.Sprintf("-cost-%.4f", costAlertValue)
	}
	key := "oci-alert-" + hourKey + "-" + dedupeSuffix
	queueCtx, queueCancel := context.WithTimeout(context.Background(), 5*time.Second)
	inserted, queueErr := database.EnqueuePendingNotification(queueCtx, "qq", recipient, key, "oci_alert", alert)
	queueCancel()
	if queueErr != nil {
		for _, key := range alertKeys {
			switch key {
			case "cost":
				state.LastCostSuccessBucket = ""
			default:
				state.LastResourceSuccessBucket = ""
			}
		}
		persistState()
		slog.Error("oci_scheduler: 告警无法写入待发队列，跳过主动发送", "reasons", reasons, "err", queueErr)
		return
	}
	if !inserted {
		markOCIAlertState(state, alertKeys, costAlertValue)
		persistState()
		slog.Info("oci_scheduler: 告警已在待发队列中，不重复主动发送", "reasons", reasons)
		return
	}
	claimCtx, claimCancel := context.WithTimeout(context.Background(), 5*time.Second)
	claimErr := database.ClaimPendingNotificationByKey(claimCtx, "qq", recipient, key)
	claimCancel()
	if claimErr != nil {
		slog.Error("oci_scheduler: 无法确认告警发送状态，跳过主动发送", "reasons", reasons, "err", claimErr)
		return
	}
	sendErr := channels.Transmit(ctx, &entity.Utterance{
		Platform: "qq", ChannelID: recipient, Proactive: true, Text: alert,
	})
	if sendErr != nil {
		var partial *entity.PartialDeliveryError
		if errors.As(sendErr, &partial) {
			remaining := strings.TrimSpace(partial.UndeliveredText)
			if remaining == "" {
				deletePendingNotification(database, recipient, key)
			} else if err := updatePendingNotificationBody(database, recipient, key, remaining); err != nil {
				slog.Error("oci_scheduler: 告警部分送达后无法保存剩余正文", "reasons", reasons, "err", err)
				markPendingNotificationUncertain(database, recipient, key)
			}
		} else {
			setPendingNotificationFailureState(database, recipient, key, sendErr)
			slog.Warn("oci_scheduler: 主动推送失败，告警保留在待发队列", "reasons", reasons, "err", sendErr)
		}
	} else {
		deletePendingNotification(database, recipient, key)
		slog.Info("oci_scheduler: OCI 告警已主动发送", "reasons", reasons)
	}
	markOCIAlertState(state, alertKeys, costAlertValue)
	persistState()
}

func markOCIAlertState(state *ociSchedulerState, alertKeys []string, costAlertValue float64) {
	for _, key := range alertKeys {
		if key == "cost" {
			state.AlertedCost = costAlertValue
		} else {
			state.AlertedRisk[key] = true
		}
	}
}
