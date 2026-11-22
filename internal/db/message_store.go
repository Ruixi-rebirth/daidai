package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"daidai/internal/config"
	"daidai/internal/core"
)

func (m *Manager) LogMessage(ctx context.Context, platform, chatType, convID, userID, name, text string, isBot bool, replyToUID, platformMsgID string) (int64, error) {
	return m.logMessage(ctx, platform, chatType, convID, userID, name, text, isBot, replyToUID, platformMsgID, 0)
}

// LogBotReply records which exact inbound message a delivered bot reply answers.
func (m *Manager) LogBotReply(ctx context.Context, platform, chatType, convID, text, replyToUID string, replyToMessageID int64) (int64, error) {
	return m.logMessage(ctx, platform, chatType, convID, "daidai", config.BotName(), text, true, replyToUID, "", replyToMessageID)
}

func (m *Manager) logMessage(ctx context.Context, platform, chatType, convID, userID, name, text string, isBot bool, replyToUID, platformMsgID string, replyToMessageID int64) (int64, error) {
	text = core.TruncateRunes(text, core.MaxUserMessageRunes, "…[消息已截断]")
	p := NormPlatform(platform)
	db, err := m.GetConn(p)
	if err != nil {
		return 0, err
	}

	botVal := 0
	if isBot {
		botVal = 1
	}

	nowMs := time.Now().UnixMilli()
	res, err := db.ExecContext(ctx,
		"INSERT INTO messages (platform, chat_type, conv_id, user_id, name, text, is_bot, created_at, reply_to_uid, reply_to_message_id, platform_msg_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		p, chatType, convID, userID, name, text, botVal, nowMs, replyToUID, nullableMessageID(replyToMessageID), platformMsgID,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func nullableMessageID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

// LogInboundMessage records one raw user event once, keyed by its platform
// message ID. It runs before debounce so merged turns cannot erase messages.
func (m *Manager) LogInboundMessage(ctx context.Context, platform, chatType, convID, userID, name, text, platformMsgID string) (int64, error) {
	if platformMsgID == "" {
		return m.LogMessage(ctx, platform, chatType, convID, userID, name, text, false, "", "")
	}
	text = core.TruncateRunes(text, core.MaxUserMessageRunes, "…[消息已截断]")
	p := NormPlatform(platform)
	database, err := m.GetConn(p)
	if err != nil {
		return 0, err
	}
	result, err := database.ExecContext(ctx, `
		INSERT INTO messages (platform, chat_type, conv_id, user_id, name, text, is_bot, created_at, reply_to_uid, platform_msg_id)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?, '', ?)
		ON CONFLICT DO NOTHING`, p, chatType, convID, userID, name, text, time.Now().UnixMilli(), platformMsgID)
	if err != nil {
		return 0, err
	}
	if inserted, err := result.RowsAffected(); err != nil {
		return 0, err
	} else if inserted > 0 {
		return result.LastInsertId()
	}
	var id int64
	err = database.QueryRowContext(ctx, `SELECT id FROM messages
		WHERE platform = ? AND chat_type = ? AND conv_id = ? AND platform_msg_id = ? AND is_bot = 0
		ORDER BY id DESC LIMIT 1`, p, chatType, convID, platformMsgID).Scan(&id)
	return id, err
}

// MessageTextByID loads one bounded history row for its asynchronous embedding.
func (m *Manager) MessageTextByID(ctx context.Context, platform string, id int64) (string, error) {
	if id <= 0 {
		return "", nil
	}
	database, err := m.GetConn(platform)
	if err != nil {
		return "", err
	}
	var text string
	err = database.QueryRowContext(ctx, `SELECT substr(text, 1, ?) FROM messages WHERE platform = ? AND id = ? AND is_bot = 0`, core.MaxUserMessageRunes+1, NormPlatform(platform), id).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return text, err
}

// AppendMessageText adds a derived note, such as a vision summary, to an
// already persisted inbound row without duplicating the note on retries.
func (m *Manager) AppendMessageText(ctx context.Context, platform string, id int64, addition string) (string, error) {
	if id <= 0 || strings.TrimSpace(addition) == "" {
		return m.MessageTextByID(ctx, platform, id)
	}
	database, err := m.GetConn(platform)
	if err != nil {
		return "", err
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT text FROM messages WHERE platform = ? AND id = ? AND is_bot = 0`, NormPlatform(platform), id).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if strings.HasSuffix(current, addition) {
		return current, nil
	}
	current = core.AppendTextWithinRuneLimit(current, addition, core.MaxUserMessageRunes, "\n[原消息过长，已截短以保留图片内容]")
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET text = ? WHERE platform = ? AND id = ? AND is_bot = 0`, current, NormPlatform(platform), id); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return current, nil
}

// MessageContentByPlatformID retrieves the latest stored body for a platform message reference.
func (m *Manager) MessageContentByPlatformID(ctx context.Context, platform, chatType, convID, platformMsgID string) (string, error) {
	if platformMsgID == "" || (chatType != "private" && chatType != "group") || strings.TrimSpace(convID) == "" {
		return "", nil
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return "", err
	}
	var content string
	err = db.QueryRowContext(ctx, "SELECT text FROM messages WHERE platform = ? AND chat_type = ? AND conv_id = ? AND platform_msg_id = ? ORDER BY id DESC LIMIT 1", NormPlatform(platform), chatType, convID, platformMsgID).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return content, err
}

// StoredMessage 持久化的会话历史消息记录
type StoredMessage struct {
	ID             int64
	Platform       string
	IsGroup        bool
	ConvID         string
	UserID         string
	Name           string
	Text           string
	IsBot          bool
	ReplyToUID     string
	Embedding      []float32
	EmbeddingModel string
	CreatedAt      time.Time
}
