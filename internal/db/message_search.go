package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"daidai/internal/core"
)

// SearchMessagesInChat searches history within one conversation type, preventing
// a group search from matching a private conversation that happens to share its ID.
func (m *Manager) SearchMessagesInChat(ctx context.Context, platform, chatType, convID string, terms []string, limit int) ([]StoredMessage, error) {
	if chatType != "group" && chatType != "private" {
		return nil, fmt.Errorf("invalid chat type %q", chatType)
	}
	return m.searchMessages(ctx, platform, chatType, convID, terms, limit)
}

func (m *Manager) searchMessages(ctx context.Context, platform, chatType, convID string, terms []string, limit int) ([]StoredMessage, error) {
	if convID == "" || len(terms) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}

	p := NormPlatform(platform)
	db, err := m.GetConn(p)
	if err != nil {
		return nil, err
	}

	var (
		whereConds []string
		scoreTerms []string
		whereArgs  []any
		scoreArgs  []any
	)
	for _, t := range terms {
		if t == "" {
			continue
		}
		likeVal := "%" + t + "%"
		whereConds = append(whereConds, "text LIKE ?")
		whereArgs = append(whereArgs, likeVal)
		scoreTerms = append(scoreTerms, "CASE WHEN text LIKE ? THEN 1 ELSE 0 END")
		scoreArgs = append(scoreArgs, likeVal)
	}
	if len(whereConds) == 0 {
		return nil, nil
	}

	queryScope := "platform = ? AND chat_type = ? AND conv_id = ?"
	args := []any{p, chatType, convID}
	resetID, err := sessionHistoryBoundary(ctx, db, chatType, convID)
	if err != nil {
		return nil, err
	}
	queryScope += " AND id > ?"
	args = append(args, resetID)
	args = append(args, whereArgs...)
	args = append(args, scoreArgs...)
	args = append(args, limit)

	query := fmt.Sprintf(`
		SELECT id, platform, chat_type, conv_id, user_id, name, substr(text, 1, %d), is_bot, created_at, embedding, embedding_model
		FROM messages
		WHERE %s AND (%s)
		ORDER BY (%s) DESC, id DESC
		LIMIT ?
	`, core.MaxUserMessageRunes+1, queryScope, strings.Join(whereConds, " OR "), strings.Join(scoreTerms, " + "))
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var list []StoredMessage
	for rows.Next() {
		var (
			id        int64
			plat      string
			cType     string
			cID       string
			uID       string
			name      string
			text      string
			isBotInt  int
			createdMs int64
			embBlob   []byte
			embModel  string
		)
		if err := rows.Scan(&id, &plat, &cType, &cID, &uID, &name, &text, &isBotInt, &createdMs, &embBlob, &embModel); err != nil {
			return nil, err
		}
		list = append(list, StoredMessage{
			ID:             id,
			Platform:       plat,
			IsGroup:        cType == "group",
			ConvID:         cID,
			UserID:         uID,
			Name:           name,
			Text:           core.TruncateRunes(text, core.MaxUserMessageRunes, "…[消息已截断]"),
			IsBot:          isBotInt == 1,
			Embedding:      decodeFloat32Slice(embBlob),
			EmbeddingModel: embModel,
			CreatedAt:      time.UnixMilli(createdMs),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
	return list, nil
}
