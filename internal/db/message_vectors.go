package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"daidai/internal/core"
)

// RecentVectorMessagesInChat restricts vector recall to the requested chat type.
func (m *Manager) RecentVectorMessagesInChat(ctx context.Context, platform, chatType, convID, modelIdentity string, limit int) ([]StoredMessage, error) {
	if chatType != "group" && chatType != "private" {
		return nil, fmt.Errorf("invalid chat type %q", chatType)
	}
	return m.recentVectorMessages(ctx, platform, chatType, convID, modelIdentity, limit)
}

func (m *Manager) recentVectorMessages(ctx context.Context, platform, chatType, convID, modelIdentity string, limit int) ([]StoredMessage, error) {
	if convID == "" || strings.TrimSpace(modelIdentity) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 500
	}
	p := NormPlatform(platform)
	db, err := m.GetConn(p)
	if err != nil {
		return nil, err
	}

	queryScope := "platform = ? AND chat_type = ? AND conv_id = ?"
	queryArgs := []any{p, chatType, convID, limit}
	resetID, err := sessionHistoryBoundary(ctx, db, chatType, convID)
	if err != nil {
		return nil, err
	}
	queryScope += " AND id > ?"
	queryArgs = append(queryArgs[:len(queryArgs)-1], resetID, limit)
	queryScope += " AND embedding_model = ?"
	queryArgs = append(queryArgs[:len(queryArgs)-1], modelIdentity, limit)
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, platform, chat_type, conv_id, user_id, name, substr(text, 1, %d), is_bot, created_at, embedding, embedding_model
		FROM messages
		WHERE %s AND embedding IS NOT NULL AND length(embedding) > 0
		ORDER BY id DESC
		LIMIT ?
	`, core.MaxUserMessageRunes+1, queryScope), queryArgs...)
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
	return list, nil
}
