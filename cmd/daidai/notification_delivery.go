package main

import (
	"context"
	"log/slog"
	"time"

	"daidai/internal/db"
	"daidai/internal/entity"
)

func deletePendingNotification(database *db.Manager, recipient, key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := database.DeletePendingNotificationByKey(ctx, "qq", recipient, key); err != nil {
		slog.Error("oci_scheduler: 通知已发送但待发副本未能清除", "key", key, "err", err)
	}
}

func updatePendingNotificationBody(database *db.Manager, recipient, key, body string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return database.UpdatePendingNotificationBodyByKey(ctx, "qq", recipient, key, body)
}

func setPendingNotificationFailureState(database *db.Manager, recipient, key string, sendErr error) {
	state := db.PendingNotificationStateUncertain
	if entity.NotificationDeliveryCanRetry(sendErr) {
		state = db.PendingNotificationStatePending
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := database.SetPendingNotificationStateByKey(ctx, "qq", recipient, key, state); err != nil {
		slog.Error("oci_scheduler: 通知失败后无法更新发送状态", "key", key, "state", state, "err", err)
	}
}

func markPendingNotificationUncertain(database *db.Manager, recipient, key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := database.SetPendingNotificationStateByKey(ctx, "qq", recipient, key, db.PendingNotificationStateUncertain); err != nil {
		slog.Error("oci_scheduler: 通知剩余正文未能保存，且无法标记为结果不确定", "key", key, "err", err)
	}
}
