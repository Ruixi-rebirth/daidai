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

type QQQuoteRecord struct {
	MsgID        string
	Content      string
	ChatType     string
	ConvID       string
	Direction    string
	ContentKind  string
	TextComplete bool
}

func (m *Manager) SaveQQQuoteRecord(ctx context.Context, record QQQuoteRecord, limit int) error {
	if record.MsgID == "" || (record.ChatType != "private" && record.ChatType != "group") || strings.TrimSpace(record.ConvID) == "" {
		return fmt.Errorf("QQ quote record requires message ID and valid conversation scope")
	}
	if record.Direction != "inbound" && record.Direction != "outbound" {
		return fmt.Errorf("invalid QQ quote direction %q", record.Direction)
	}
	switch record.ContentKind {
	case "text", "text_card", "mixed_media", "image", "file", "unknown":
	default:
		return fmt.Errorf("invalid QQ quote content kind %q", record.ContentKind)
	}
	if record.ContentKind == "image" || record.ContentKind == "file" {
		record.Content = ""
		record.TextComplete = false
	}
	if record.Content == "" {
		record.TextComplete = false
	}
	if limit <= 0 {
		limit = 1000
	}
	if len([]rune(record.Content)) > core.MaxUserMessageRunes {
		record.Content = core.TruncateRunes(record.Content, core.MaxUserMessageRunes, "…[引用正文已截断]")
		record.TextComplete = false
	}
	database, err := m.GetConn("qq")
	if err != nil {
		return err
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO qq_quote_records (msg_id, chat_type, conv_id, direction, content, content_kind, text_complete, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(msg_id, chat_type, conv_id) DO UPDATE SET
			direction = excluded.direction,
			content = excluded.content,
			content_kind = excluded.content_kind,
			text_complete = excluded.text_complete,
			updated_at = excluded.updated_at`,
		record.MsgID, record.ChatType, record.ConvID, record.Direction, record.Content, record.ContentKind, boolToSQLite(record.TextComplete), time.Now().UnixMilli()); err != nil {
		return err
	}
	if err := pruneQQQuoteRecords(ctx, tx, limit); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Manager) PruneQQQuoteRecords(ctx context.Context, limit int) error {
	if limit <= 0 {
		limit = 1000
	}
	database, err := m.GetConn("qq")
	if err != nil {
		return err
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := pruneQQQuoteRecords(ctx, tx, limit); err != nil {
		return err
	}
	return tx.Commit()
}

func pruneQQQuoteRecords(ctx context.Context, tx *sql.Tx, limit int) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM qq_quote_records
		WHERE rowid NOT IN (
			SELECT rowid FROM qq_quote_records ORDER BY updated_at DESC, rowid DESC LIMIT ?
		)`, limit)
	return err
}

func (m *Manager) LoadQQQuoteRecords(limit int) ([]QQQuoteRecord, error) {
	if limit <= 0 {
		limit = 1000
	}
	database, err := m.GetConn("qq")
	if err != nil {
		return nil, err
	}
	rows, err := database.Query(`
		SELECT msg_id, content, chat_type, conv_id, direction, content_kind, text_complete
		FROM qq_quote_records
		WHERE (msg_id, chat_type, conv_id) IN (
			SELECT msg_id, chat_type, conv_id FROM qq_quote_records ORDER BY updated_at DESC, rowid DESC LIMIT ?
		)
		ORDER BY updated_at ASC, rowid ASC`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []QQQuoteRecord
	for rows.Next() {
		var record QQQuoteRecord
		var complete int
		if err := rows.Scan(&record.MsgID, &record.Content, &record.ChatType, &record.ConvID, &record.Direction, &record.ContentKind, &complete); err != nil {
			return nil, err
		}
		record.TextComplete = complete == 1
		records = append(records, record)
	}
	return records, rows.Err()
}

func (m *Manager) GetQQQuoteRecord(ctx context.Context, msgID, chatType, convID string) (QQQuoteRecord, bool, error) {
	if msgID == "" || (chatType != "private" && chatType != "group") || strings.TrimSpace(convID) == "" {
		return QQQuoteRecord{}, false, nil
	}
	database, err := m.GetConn("qq")
	if err != nil {
		return QQQuoteRecord{}, false, err
	}
	var record QQQuoteRecord
	var complete int
	err = database.QueryRowContext(ctx, `
		SELECT msg_id, content, chat_type, conv_id, direction, content_kind, text_complete
		FROM qq_quote_records WHERE msg_id = ? AND chat_type = ? AND conv_id = ?`,
		msgID, chatType, convID).Scan(&record.MsgID, &record.Content, &record.ChatType, &record.ConvID, &record.Direction, &record.ContentKind, &complete)
	if errors.Is(err, sql.ErrNoRows) {
		return QQQuoteRecord{}, false, nil
	}
	if err != nil {
		return QQQuoteRecord{}, false, err
	}
	record.TextComplete = complete == 1
	return record, true, nil
}

func (m *Manager) DeleteQQQuoteRecord(msgID, chatType, convID string) error {
	if msgID == "" || (chatType != "private" && chatType != "group") || strings.TrimSpace(convID) == "" {
		return nil
	}
	database, err := m.GetConn("qq")
	if err != nil {
		return err
	}
	_, err = database.Exec(`DELETE FROM qq_quote_records WHERE msg_id = ? AND chat_type = ? AND conv_id = ?`, msgID, chatType, convID)
	return err
}

func boolToSQLite(value bool) int {
	if value {
		return 1
	}
	return 0
}

// WorkspaceEntry 工作空间条目
