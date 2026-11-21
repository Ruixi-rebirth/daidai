package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type PendingNotificationState string

const (
	PendingNotificationStatePending           PendingNotificationState = "pending"
	PendingNotificationStateSending           PendingNotificationState = "sending"
	PendingNotificationStateUncertain         PendingNotificationState = "uncertain"
	PendingNotificationStateUncertainNotified PendingNotificationState = "uncertain_notified"
)

type PendingNotification struct {
	ID        int64
	DedupeKey string
	Category  string
	Body      string
	State     PendingNotificationState
	CreatedAt time.Time
}

// EnqueuePendingNotification durably queues a notification when the platform
// cannot deliver an unsolicited push. Duplicate keys are idempotent.
func (m *Manager) EnqueuePendingNotification(ctx context.Context, platform, recipientID, dedupeKey, category, body string) (bool, error) {
	platform = NormPlatform(platform)
	recipientID = strings.TrimSpace(recipientID)
	dedupeKey = strings.TrimSpace(dedupeKey)
	category = strings.TrimSpace(category)
	if recipientID == "" || dedupeKey == "" || category == "" || strings.TrimSpace(body) == "" {
		return false, fmt.Errorf("notification recipient, dedupe key, category, and body are required")
	}
	if len(body) > 64<<10 {
		return false, fmt.Errorf("notification body exceeds 64 KiB")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return false, err
	}
	result, err := db.ExecContext(ctx, `INSERT INTO pending_notifications
		(platform, recipient_id, dedupe_key, category, body, created_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(platform, recipient_id, dedupe_key) DO NOTHING`,
		platform, recipientID, dedupeKey, category, body, time.Now().UnixMilli())
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return inserted > 0, nil
}

func (m *Manager) ListPendingNotifications(ctx context.Context, platform, recipientID string, limit int) ([]PendingNotification, error) {
	db, err := m.GetConn(platform)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, err := db.QueryContext(ctx, `SELECT id, dedupe_key, category, body, delivery_state, created_at FROM pending_notifications
		WHERE platform=? AND recipient_id=? AND delivery_state IN ('pending', 'uncertain') ORDER BY id ASC LIMIT ?`, NormPlatform(platform), strings.TrimSpace(recipientID), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var notifications []PendingNotification
	for rows.Next() {
		var notification PendingNotification
		var createdAt int64
		if err := rows.Scan(&notification.ID, &notification.DedupeKey, &notification.Category, &notification.Body, &notification.State, &createdAt); err != nil {
			return nil, err
		}
		notification.CreatedAt = time.UnixMilli(createdAt)
		notifications = append(notifications, notification)
	}
	return notifications, rows.Err()
}

func (m *Manager) DeletePendingNotification(ctx context.Context, platform, recipientID string, id int64) error {
	if id <= 0 || strings.TrimSpace(recipientID) == "" {
		return fmt.Errorf("notification ID and recipient are required")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `DELETE FROM pending_notifications WHERE id=? AND platform=? AND recipient_id=?`,
		id, NormPlatform(platform), strings.TrimSpace(recipientID))
	return err
}

func (m *Manager) DeletePendingNotificationByKey(ctx context.Context, platform, recipientID, dedupeKey string) error {
	if strings.TrimSpace(recipientID) == "" || strings.TrimSpace(dedupeKey) == "" {
		return fmt.Errorf("notification recipient and dedupe key are required")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `DELETE FROM pending_notifications WHERE platform=? AND recipient_id=? AND dedupe_key=?`,
		NormPlatform(platform), strings.TrimSpace(recipientID), strings.TrimSpace(dedupeKey))
	return err
}

// UpdatePendingNotificationBody replaces the remaining payload after a
// notification was delivered in bounded pages.
func (m *Manager) UpdatePendingNotificationBody(ctx context.Context, platform, recipientID string, id int64, body string) error {
	if id <= 0 || strings.TrimSpace(recipientID) == "" || strings.TrimSpace(body) == "" {
		return fmt.Errorf("notification ID, recipient, and body are required")
	}
	if len(body) > 64<<10 {
		return fmt.Errorf("notification body exceeds 64 KiB")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE pending_notifications SET body = ?, delivery_state = 'pending' WHERE id = ? AND platform = ? AND recipient_id = ?`,
		body, id, NormPlatform(platform), strings.TrimSpace(recipientID))
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (m *Manager) UpdatePendingNotificationBodyByKey(ctx context.Context, platform, recipientID, dedupeKey, body string) error {
	if strings.TrimSpace(recipientID) == "" || strings.TrimSpace(dedupeKey) == "" || strings.TrimSpace(body) == "" {
		return fmt.Errorf("notification recipient, dedupe key, and body are required")
	}
	if len(body) > 64<<10 {
		return fmt.Errorf("notification body exceeds 64 KiB")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE pending_notifications SET body = ?, delivery_state = 'pending' WHERE platform = ? AND recipient_id = ? AND dedupe_key = ?`,
		body, NormPlatform(platform), strings.TrimSpace(recipientID), strings.TrimSpace(dedupeKey))
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (m *Manager) ClaimPendingNotification(ctx context.Context, platform, recipientID string, id int64) error {
	if id <= 0 || strings.TrimSpace(recipientID) == "" {
		return fmt.Errorf("notification ID and recipient are required")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE pending_notifications SET delivery_state = ?
		WHERE id = ? AND platform = ? AND recipient_id = ? AND delivery_state = 'pending'`,
		PendingNotificationStateSending, id, NormPlatform(platform), strings.TrimSpace(recipientID))
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (m *Manager) ClaimPendingNotificationByKey(ctx context.Context, platform, recipientID, dedupeKey string) error {
	if strings.TrimSpace(recipientID) == "" || strings.TrimSpace(dedupeKey) == "" {
		return fmt.Errorf("notification recipient and dedupe key are required")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE pending_notifications SET delivery_state = ?
		WHERE platform = ? AND recipient_id = ? AND dedupe_key = ? AND delivery_state = 'pending'`,
		PendingNotificationStateSending, NormPlatform(platform), strings.TrimSpace(recipientID), strings.TrimSpace(dedupeKey))
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (m *Manager) SetPendingNotificationState(ctx context.Context, platform, recipientID string, id int64, state PendingNotificationState) error {
	if id <= 0 || strings.TrimSpace(recipientID) == "" || !validPendingNotificationState(state) {
		return fmt.Errorf("notification ID, recipient, or state is invalid")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE pending_notifications SET delivery_state = ? WHERE id = ? AND platform = ? AND recipient_id = ?`,
		state, id, NormPlatform(platform), strings.TrimSpace(recipientID))
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (m *Manager) SetPendingNotificationStateByKey(ctx context.Context, platform, recipientID, dedupeKey string, state PendingNotificationState) error {
	if strings.TrimSpace(recipientID) == "" || strings.TrimSpace(dedupeKey) == "" || !validPendingNotificationState(state) {
		return fmt.Errorf("notification recipient, dedupe key, or state is invalid")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE pending_notifications SET delivery_state = ? WHERE platform = ? AND recipient_id = ? AND dedupe_key = ?`,
		state, NormPlatform(platform), strings.TrimSpace(recipientID), strings.TrimSpace(dedupeKey))
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func validPendingNotificationState(state PendingNotificationState) bool {
	switch state {
	case PendingNotificationStatePending, PendingNotificationStateSending, PendingNotificationStateUncertain, PendingNotificationStateUncertainNotified:
		return true
	default:
		return false
	}
}
