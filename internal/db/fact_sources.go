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

// LogMessage 写入纯净原始消息 (零前缀)
type FactSource struct {
	MessageID int64
	Text      string
	CreatedAt time.Time
	Excerpt   bool
}

func insertFactSources(ctx context.Context, tx *sql.Tx, factID int64, messageIDs []int64, platform, userID, scopeType, scopeID string, linkedAt int64) error {
	chatType := "private"
	convID := ""
	if scopeType == "group" {
		chatType = "group"
		convID = scopeID
	}
	for _, messageID := range messageIDs {
		if messageID <= 0 {
			return fmt.Errorf("invalid memory source message ID %d", messageID)
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO user_fact_sources (fact_id, message_id, linked_at)
		SELECT ?, id, ? FROM messages
		WHERE id = ? AND platform = ? AND user_id = ? AND chat_type = ? AND (chat_type = 'private' OR conv_id = ?) AND is_bot = 0
		ON CONFLICT(fact_id, message_id) DO NOTHING`,
			factID, linkedAt, messageID, platform, userID, chatType, convID)
		if err != nil {
			return err
		}
		if affected, err := result.RowsAffected(); err != nil {
			return err
		} else if affected == 0 {
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM user_fact_sources WHERE fact_id = ? AND message_id = ?`, factID, messageID).Scan(&exists); err == nil {
				continue
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			return fmt.Errorf("memory source message %d is missing or outside the fact scope", messageID)
		}
	}
	return nil
}

// PageVisibleFactSources reads a consistent count and page of original messages
// in the current fact scope. A saved excerpt is one item if no messages remain.
func (m *Manager) PageVisibleFactSources(ctx context.Context, platform, userID, groupID string, isGroup bool, factID int64, page, pageSize int) ([]FactSource, int, error) {
	if page < 1 || pageSize < 1 || pageSize > 100 || page-1 > int(^uint(0)>>1)/pageSize {
		return nil, 0, fmt.Errorf("invalid source page %d with size %d", page, pageSize)
	}
	database, err := m.GetConn(platform)
	if err != nil {
		return nil, 0, err
	}
	tx, err := database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	scopeType, scopeID := "global", ""
	if isGroup {
		if strings.TrimSpace(groupID) == "" {
			return nil, 0, fmt.Errorf("group ID is required to read group fact sources")
		}
		scopeType, scopeID = "group", groupID
	}
	var quote string
	args := []any{factID, NormPlatform(platform), userID, scopeType, scopeID}
	err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT substr(quote, 1, %d) FROM user_facts
		WHERE id = ? AND platform = ? AND user_id = ? AND scope_type = ? AND scope_id = ? AND status = 'active'`, core.MaxUserMessageRunes), args...).Scan(&quote)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	const sourceQuery = `FROM user_fact_sources s
		JOIN user_facts f ON f.id = s.fact_id
		JOIN messages m ON m.id = s.message_id AND m.platform = f.platform AND m.user_id = f.user_id AND m.is_bot = 0
			AND ((f.scope_type = 'global' AND m.chat_type = 'private') OR (f.scope_type = 'group' AND m.chat_type = 'group' AND m.conv_id = f.scope_id))
		WHERE s.fact_id = ? AND f.platform = ? AND f.user_id = ? AND f.scope_type = ? AND f.scope_id = ? AND f.status = 'active'`
	var total int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) `+sourceQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 && strings.TrimSpace(quote) != "" {
		if page == 1 {
			return []FactSource{{Text: quote, Excerpt: true}}, 1, nil
		}
		return nil, 1, nil
	}
	query := fmt.Sprintf(`SELECT m.id, substr(m.text, 1, %d), m.created_at `, core.MaxUserMessageRunes) + sourceQuery + ` ORDER BY s.linked_at DESC, m.id DESC LIMIT ? OFFSET ?`
	rows, err := tx.QueryContext(ctx, query, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	sources := make([]FactSource, 0, pageSize)
	for rows.Next() {
		var source FactSource
		var createdAt int64
		if err := rows.Scan(&source.MessageID, &source.Text, &createdAt); err != nil {
			return nil, 0, err
		}
		source.CreatedAt = time.UnixMilli(createdAt)
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return sources, total, nil
}
