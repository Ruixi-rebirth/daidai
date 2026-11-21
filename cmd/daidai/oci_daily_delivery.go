package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/engine/render"
	"daidai/internal/entity"
	"daidai/internal/tool"
)

func deliverOCIDailyReport(
	ctx context.Context,
	database *db.Manager,
	channels *entity.ChannelRegistry,
	recipient, localDay string,
	report *tool.OCIBillReport,
	state *ociSchedulerState,
	persistState func(),
) {
	cardCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	card, renderErr := render.RenderCard(cardCtx, report.Markdown)
	cancel()
	if renderErr != nil {
		slog.Warn("oci_scheduler: 日报卡片渲染失败，将发送文本", "err", renderErr)
		card = nil
	}

	message := report.Markdown
	if len(card) > 0 {
		message = "📊 OCI 每日账单与资源巡检"
	} else if len(core.SplitMarkdownChunks(message, 1200)) > 5 {
		queueCtx, queueCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, queueErr := database.EnqueuePendingNotification(queueCtx, "qq", recipient,
			"oci-daily-"+localDay, "oci_daily", report.Markdown)
		queueCancel()
		if queueErr != nil {
			slog.Error("oci_scheduler: 日报超过文本发送上限且完整报告未能入队，将稍后重试", "date", localDay, "err", queueErr)
			return
		}
		state.LastDailySentDay = localDay
		persistState()
		slog.Info("oci_scheduler: 日报超过文本发送上限，已保存并等待下次私聊送达", "date", localDay)
		return
	}

	key := "oci-daily-" + localDay
	queueCtx, queueCancel := context.WithTimeout(context.Background(), 5*time.Second)
	inserted, queueErr := database.EnqueuePendingNotification(queueCtx, "qq", recipient, key, "oci_daily", report.Markdown)
	queueCancel()
	if queueErr != nil {
		slog.Error("oci_scheduler: 无法持久化日报，跳过主动发送以避免结果不明", "date", localDay, "err", queueErr)
		return
	}
	if !inserted {
		state.LastDailySentDay = localDay
		persistState()
		slog.Info("oci_scheduler: 日报已在待发队列中，不重复主动发送", "date", localDay)
		return
	}
	claimCtx, claimCancel := context.WithTimeout(context.Background(), 5*time.Second)
	claimErr := database.ClaimPendingNotificationByKey(claimCtx, "qq", recipient, key)
	claimCancel()
	if claimErr != nil {
		slog.Error("oci_scheduler: 无法确认日报发送状态，跳过主动发送", "date", localDay, "err", claimErr)
		return
	}
	sendErr := channels.Transmit(ctx, &entity.Utterance{
		Platform: "qq", ChannelID: recipient, Proactive: true,
		Text: message, CardData: card, FallbackText: report.Markdown,
	})
	if sendErr != nil {
		var partialErr *entity.PartialDeliveryError
		if errors.As(sendErr, &partialErr) {
			pendingBody := strings.TrimSpace(partialErr.UndeliveredText)
			if pendingBody == "" {
				deletePendingNotification(database, recipient, key)
				state.LastDailySentDay = localDay
				persistState()
				return
			}
			if err := updatePendingNotificationBody(database, recipient, key, pendingBody); err != nil {
				slog.Error("oci_scheduler: 日报部分送达后无法保存剩余正文", "date", localDay, "err", err)
				markPendingNotificationUncertain(database, recipient, key)
			} else {
				slog.Warn("oci_scheduler: 日报部分送达，队列仅保留未送达正文", "date", localDay)
			}
		} else {
			setPendingNotificationFailureState(database, recipient, key, sendErr)
			slog.Warn("oci_scheduler: 日报主动推送失败，保留在待发队列", "date", localDay, "err", sendErr)
		}
		state.LastDailySentDay = localDay
		persistState()
		return
	}

	deletePendingNotification(database, recipient, key)
	state.LastDailySentDay = localDay
	persistState()
	slog.Info("oci_scheduler: 每日账单已发送或完整报告已排队", "date", localDay)
}
