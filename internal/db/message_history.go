package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"daidai/internal/core"
)

func sessionHistoryBoundary(ctx context.Context, db *sql.DB, chatType, convID string) (int64, error) {
	var resetID int64
	resetKey := sessionResetKey(chatType, convID)
	err := db.QueryRowContext(ctx, "SELECT reset_id FROM session_resets WHERE conv_id = ?", resetKey).Scan(&resetID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return resetID, err
}

// RecentMessages 读取指定会话最近 limit 条纯净消息 (正序返回)
func (m *Manager) RecentMessages(ctx context.Context, platform, chatType, convID string, limit int) ([]StoredMessage, error) {
	return m.RecentMessagesBefore(ctx, platform, chatType, convID, limit, 0)
}

// RecentMessagesBefore excludes the current turn and later queued messages.
// A zero boundary requests the latest history, for callers outside a live turn.
func (m *Manager) RecentMessagesBefore(ctx context.Context, platform, chatType, convID string, limit int, beforeID int64) ([]StoredMessage, error) {
	if chatType != "private" && chatType != "group" {
		return nil, fmt.Errorf("chatType must be private or group")
	}
	if strings.TrimSpace(convID) == "" {
		return nil, fmt.Errorf("conversation ID is required")
	}
	if limit <= 0 {
		limit = 30
	}

	p := NormPlatform(platform)
	db, err := m.GetConn(p)
	if err != nil {
		return nil, err
	}

	resetID, err := sessionHistoryBoundary(ctx, db, chatType, convID)
	if err != nil {
		return nil, err
	}

	query := fmt.Sprintf(`
		SELECT id, platform, chat_type, conv_id, user_id, name, substr(text, 1, %d), is_bot, created_at, reply_to_uid
		FROM messages
		WHERE platform = ? AND chat_type = ? AND conv_id = ? AND id > ?
	`, core.MaxUserMessageRunes+1)
	queryArgs := []any{p, chatType, convID, resetID}
	if beforeID > 0 {
		query += " AND id < ?"
		queryArgs = append(queryArgs, beforeID)
	}
	query += " ORDER BY id DESC LIMIT ?"
	queryArgs = append(queryArgs, limit)
	rows, err := db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var list []StoredMessage
	for rows.Next() {
		var (
			id         int64
			plat       string
			cType      string
			cID        string
			uID        string
			name       string
			text       string
			isBotInt   int
			createdMs  int64
			replyToUID string
		)
		if err := rows.Scan(&id, &plat, &cType, &cID, &uID, &name, &text, &isBotInt, &createdMs, &replyToUID); err != nil {
			return nil, err
		}

		list = append(list, StoredMessage{
			ID:         id,
			Platform:   plat,
			IsGroup:    cType == "group",
			ConvID:     cID,
			UserID:     uID,
			Name:       name,
			Text:       core.TruncateRunes(text, core.MaxUserMessageRunes, "…[消息已截断]"),
			IsBot:      isBotInt == 1,
			ReplyToUID: replyToUID,
			CreatedAt:  time.UnixMilli(createdMs),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 翻转为正序（早的在前，近的在后）
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}

	return list, nil
}

// SampleMessagesInChatSince samples evenly across every message in the given
// time range while keeping the returned payload bounded. Session reset
// boundaries are applied before sampling.
func (m *Manager) SampleMessagesInChatSince(ctx context.Context, platform, chatType, convID string, since time.Time, limit int) ([]StoredMessage, error) {
	if chatType != "private" && chatType != "group" {
		return nil, fmt.Errorf("chatType must be private or group")
	}
	if strings.TrimSpace(convID) == "" {
		return nil, fmt.Errorf("conversation ID is required")
	}
	if limit <= 0 {
		limit = 500
	}
	if limit > 1000 {
		limit = 1000
	}
	p := NormPlatform(platform)
	db, err := m.GetConn(p)
	if err != nil {
		return nil, err
	}
	resetID, err := sessionHistoryBoundary(ctx, db, chatType, convID)
	if err != nil {
		return nil, err
	}
	where := `platform = ? AND chat_type = ? AND conv_id = ? AND created_at >= ? AND id > ?`
	args := []any{limit, p, chatType, convID, since.UnixMilli(), resetID}
	query := fmt.Sprintf(`
		WITH scoped AS (
			SELECT id, platform, chat_type, conv_id, user_id, name,
			       substr(text, 1, %d) AS text, is_bot, created_at,
			       NTILE(?) OVER (ORDER BY id ASC) AS sample_bucket
			FROM messages WHERE %s
		), ranked AS (
			SELECT id, platform, chat_type, conv_id, user_id, name, text, is_bot, created_at,
			       ROW_NUMBER() OVER (PARTITION BY sample_bucket ORDER BY id ASC) AS bucket_row
			FROM scoped
		)
		SELECT id, platform, chat_type, conv_id, user_id, name, text, is_bot, created_at
		FROM ranked WHERE bucket_row = 1 ORDER BY id ASC`, core.MaxSummaryMessageRunes+1, where)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var messages []StoredMessage
	for rows.Next() {
		var message StoredMessage
		var messageChatType string
		var isBot int
		var createdAt int64
		if err := rows.Scan(&message.ID, &message.Platform, &messageChatType, &message.ConvID, &message.UserID, &message.Name, &message.Text, &isBot, &createdAt); err != nil {
			return nil, err
		}
		message.IsGroup = messageChatType == "group"
		message.IsBot = isBot == 1
		message.CreatedAt = time.UnixMilli(createdAt)
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}

// RecentUserMessagesInChat retrieves only one user's own messages from one chat,
// respecting the same reset boundary as normal conversation recall.
func (m *Manager) RecentUserMessagesInChat(ctx context.Context, platform, chatType, convID, userID string, limit int) ([]StoredMessage, error) {
	if chatType != "private" && chatType != "group" {
		return nil, fmt.Errorf("chatType must be private or group")
	}
	if strings.TrimSpace(convID) == "" || strings.TrimSpace(userID) == "" {
		return nil, fmt.Errorf("conversation ID and user ID are required")
	}
	if limit <= 0 {
		limit = 40
	}
	if limit > 500 {
		limit = 500
	}

	p := NormPlatform(platform)
	db, err := m.GetConn(p)
	if err != nil {
		return nil, err
	}
	resetID, err := sessionHistoryBoundary(ctx, db, chatType, convID)
	if err != nil {
		return nil, err
	}
	query := `SELECT id, name, substr(text, 1, %d), created_at
		FROM messages WHERE platform = ? AND chat_type = ? AND conv_id = ?
		AND user_id = ? AND is_bot = 0 AND id > ? ORDER BY id DESC LIMIT ?`
	args := []any{p, chatType, convID, userID, resetID, limit}
	query = fmt.Sprintf(query, core.MaxUserMessageRunes+1)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	messages := make([]StoredMessage, 0, limit)
	for rows.Next() {
		var message StoredMessage
		var createdAt int64
		if err := rows.Scan(&message.ID, &message.Name, &message.Text, &createdAt); err != nil {
			return nil, err
		}
		message.Platform = p
		message.IsGroup = chatType == "group"
		message.ConvID = convID
		message.UserID = userID
		message.CreatedAt = time.UnixMilli(createdAt)
		message.Text = core.TruncateRunes(message.Text, core.MaxUserMessageRunes, "…[消息已截断]")
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
	return messages, nil
}
