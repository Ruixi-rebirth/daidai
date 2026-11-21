package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type ConversationDraft struct {
	ID, Platform, ChatType, ConversationID, RequesterID, Goal, ContextJSON string
	UpdatedAt                                                              time.Time
}

type ConversationDraftSummary struct {
	ID   string
	Goal string
}

const (
	conversationDraftLifetime = 7 * 24 * time.Hour
	activeDraftScope          = "platform=? AND chat_type=? AND conv_id=? AND requester_id=? AND updated_at>?"
	expiredDraftScope         = "platform=? AND chat_type=? AND conv_id=? AND requester_id=? AND updated_at<=?"
	draftVersionScope         = "id=? AND platform=? AND chat_type=? AND conv_id=? AND requester_id=? AND updated_at=? AND context_json=?"
)

func (m *Manager) SaveConversationDraft(ctx context.Context, draft ConversationDraft) error {
	if strings.TrimSpace(draft.ID) == "" || strings.TrimSpace(draft.RequesterID) == "" || strings.TrimSpace(draft.ConversationID) == "" || len(draft.ContextJSON) > 1<<20 {
		return fmt.Errorf("conversation draft identity or size is invalid")
	}
	conn, err := m.GetConn(draft.Platform)
	if err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	cutoff := time.Now().Add(-conversationDraftLifetime).UnixMilli()
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_drafts WHERE `+expiredDraftScope,
		NormPlatform(draft.Platform), draft.ChatType, draft.ConversationID, draft.RequesterID, cutoff); err != nil {
		return fmt.Errorf("delete expired conversation drafts: %w", err)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO conversation_drafts
		(id, platform, chat_type, conv_id, requester_id, goal, context_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		draft.ID, NormPlatform(draft.Platform), draft.ChatType, draft.ConversationID, draft.RequesterID, draft.Goal, draft.ContextJSON, draft.UpdatedAt.UnixMilli())
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return fmt.Errorf("conversation draft ID already exists")
	}
	return tx.Commit()
}

// UpdateConversationDraft advances only the version used to produce this segment.
func (m *Manager) UpdateConversationDraft(ctx context.Context, previous ConversationDraft, contextJSON string, updatedAt time.Time) error {
	if len(contextJSON) > 1<<20 {
		return fmt.Errorf("conversation draft exceeds size limit")
	}
	conn, err := m.GetConn(previous.Platform)
	if err != nil {
		return err
	}
	updatedMS := max(updatedAt.UnixMilli(), previous.UpdatedAt.UnixMilli()+1)
	args := append([]any{contextJSON, updatedMS}, draftVersionArgs(previous)...)
	result, err := conn.ExecContext(ctx, `UPDATE conversation_drafts SET context_json=?, updated_at=? WHERE `+draftVersionScope, args...)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return fmt.Errorf("conversation draft changed or was removed during continuation")
	}
	return nil
}

func draftVersionArgs(draft ConversationDraft) []any {
	return []any{draft.ID, NormPlatform(draft.Platform), draft.ChatType, draft.ConversationID, draft.RequesterID, draft.UpdatedAt.UnixMilli(), draft.ContextJSON}
}

func (m *Manager) ListConversationDraftSummaries(ctx context.Context, platform, chatType, convID, requesterID string) (drafts []ConversationDraftSummary, retErr error) {
	conn, err := m.GetConn(platform)
	if err != nil {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT id, goal FROM conversation_drafts WHERE `+activeDraftScope+
		` ORDER BY updated_at DESC, id DESC`, NormPlatform(platform), chatType, convID, requesterID, time.Now().Add(-conversationDraftLifetime).UnixMilli())
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); retErr == nil && closeErr != nil {
			retErr = closeErr
		}
	}()
	for rows.Next() {
		var draft ConversationDraftSummary
		if err := rows.Scan(&draft.ID, &draft.Goal); err != nil {
			return nil, err
		}
		drafts = append(drafts, draft)
	}
	return drafts, rows.Err()
}

// GetConversationDraft loads one active draft within the supplied owner scope.
func (m *Manager) GetConversationDraft(ctx context.Context, platform, chatType, convID, requesterID, id string) (*ConversationDraft, error) {
	conn, err := m.GetConn(platform)
	if err != nil {
		return nil, err
	}
	draft := &ConversationDraft{
		ID: id, Platform: NormPlatform(platform), ChatType: chatType, ConversationID: convID, RequesterID: requesterID,
	}
	var updated int64
	err = conn.QueryRowContext(ctx, `SELECT goal, context_json, updated_at FROM conversation_drafts WHERE `+activeDraftScope+` AND id=?`,
		draft.Platform, chatType, convID, requesterID, time.Now().Add(-conversationDraftLifetime).UnixMilli(), id).
		Scan(&draft.Goal, &draft.ContextJSON, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	draft.UpdatedAt = time.UnixMilli(updated)
	return draft, nil
}

func (m *Manager) DeleteConversationDraft(ctx context.Context, draft ConversationDraft) error {
	conn, err := m.GetConn(draft.Platform)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `DELETE FROM conversation_drafts WHERE `+draftVersionScope, draftVersionArgs(draft)...)
	return err
}
