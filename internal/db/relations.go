package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"daidai/internal/core"
)

const MaxRelationImpressionRunes = 160

// UserRelation 呆呆对特定会话范围内用户的熟悉度、主观印象与近期情绪底色
type UserRelation struct {
	UserID           string
	ScopeType        string
	ScopeID          string
	InteractionCount int
	Impression       string
	ImpressionCount  int
	RecentEmotion    string
	EmotionUpdatedAt time.Time
	UpdatedAt        time.Time
}

func relationScope(groupID string, isGroup bool) (string, string, bool) {
	if isGroup {
		gid := strings.TrimSpace(groupID)
		if gid == "" {
			return "", "", false
		}
		return "group", gid, true
	}
	return "global", "", true
}

// GetVisibleUserRelation 读取当前会话可见范围内的用户关系与主观印象（私聊与各群严格隔离）。
func (m *Manager) GetVisibleUserRelation(ctx context.Context, platform, userID, groupID string, isGroup bool) (*UserRelation, error) {
	userID = strings.TrimSpace(userID)
	if m == nil || userID == "" {
		return nil, nil
	}
	scopeType, scopeID, ok := relationScope(groupID, isGroup)
	if !ok {
		return nil, nil
	}
	conn, err := m.GetConn(platform)
	if err != nil {
		return nil, err
	}
	var rel UserRelation
	var emotionMs, updatedMs int64
	err = conn.QueryRowContext(ctx, `
		SELECT user_id, scope_type, scope_id, interaction_count, impression, impression_count, recent_emotion, emotion_updated_at, updated_at
		FROM user_relations
		WHERE platform = ? AND user_id = ? AND scope_type = ? AND scope_id = ?`,
		NormPlatform(platform), userID, scopeType, scopeID,
	).Scan(
		&rel.UserID, &rel.ScopeType, &rel.ScopeID,
		&rel.InteractionCount, &rel.Impression, &rel.ImpressionCount,
		&rel.RecentEmotion, &emotionMs, &updatedMs,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if emotionMs > 0 {
		rel.EmotionUpdatedAt = time.UnixMilli(emotionMs)
	}
	if updatedMs > 0 {
		rel.UpdatedAt = time.UnixMilli(updatedMs)
	}
	return &rel, nil
}

// BumpUserRelationInteraction 在一轮有效回复送达后原子累加该范围内的互动次数。
func (m *Manager) BumpUserRelationInteraction(ctx context.Context, platform, userID, groupID string, isGroup bool, at time.Time) (*UserRelation, error) {
	userID = strings.TrimSpace(userID)
	if m == nil || userID == "" {
		return nil, nil
	}
	scopeType, scopeID, ok := relationScope(groupID, isGroup)
	if !ok {
		return nil, nil
	}
	conn, err := m.GetConn(platform)
	if err != nil {
		return nil, err
	}
	if at.IsZero() {
		at = time.Now()
	}
	nowMs := at.UnixMilli()
	var rel UserRelation
	var emotionMs, updatedMs int64
	err = conn.QueryRowContext(ctx, `
		INSERT INTO user_relations (platform, user_id, scope_type, scope_id, interaction_count, updated_at)
		VALUES (?, ?, ?, ?, 1, ?)
		ON CONFLICT(platform, user_id, scope_type, scope_id) DO UPDATE SET
			interaction_count = user_relations.interaction_count + 1,
			updated_at = excluded.updated_at
		RETURNING user_id, scope_type, scope_id, interaction_count, impression, impression_count, recent_emotion, emotion_updated_at, updated_at`,
		NormPlatform(platform), userID, scopeType, scopeID, nowMs,
	).Scan(
		&rel.UserID, &rel.ScopeType, &rel.ScopeID,
		&rel.InteractionCount, &rel.Impression, &rel.ImpressionCount,
		&rel.RecentEmotion, &emotionMs, &updatedMs,
	)
	if err != nil {
		return nil, err
	}
	if emotionMs > 0 {
		rel.EmotionUpdatedAt = time.UnixMilli(emotionMs)
	}
	if updatedMs > 0 {
		rel.UpdatedAt = time.UnixMilli(updatedMs)
	}
	return &rel, nil
}

// SaveUserRelationInsight 原子持久化增量演化后的主观印象及/或近期情绪标签。
func (m *Manager) SaveUserRelationInsight(ctx context.Context, platform, userID, groupID string, isGroup bool, impression string, impressionCount int, recentEmotion string, at time.Time) error {
	userID = strings.TrimSpace(userID)
	impression = strings.TrimSpace(core.TruncateRunes(impression, MaxRelationImpressionRunes, "…"))
	recentEmotion = strings.TrimSpace(recentEmotion)
	if m == nil || userID == "" || (impression == "" && recentEmotion == "") {
		return nil
	}
	scopeType, scopeID, ok := relationScope(groupID, isGroup)
	if !ok {
		return nil
	}
	conn, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	if at.IsZero() {
		at = time.Now()
	}
	nowMs := at.UnixMilli()
	if impressionCount < 0 {
		impressionCount = 0
	}
	emotionMs := int64(0)
	if recentEmotion != "" {
		emotionMs = nowMs
	}
	_, err = conn.ExecContext(ctx, `
		INSERT INTO user_relations (
			platform, user_id, scope_type, scope_id, interaction_count,
			impression, impression_count, recent_emotion, emotion_updated_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(platform, user_id, scope_type, scope_id) DO UPDATE SET
			impression = CASE WHEN excluded.impression <> '' THEN excluded.impression ELSE user_relations.impression END,
			impression_count = CASE
				WHEN excluded.impression <> '' AND excluded.impression_count > user_relations.impression_count THEN excluded.impression_count
				WHEN excluded.impression <> '' THEN user_relations.interaction_count
				ELSE user_relations.impression_count
			END,
			recent_emotion = CASE WHEN excluded.recent_emotion <> '' THEN excluded.recent_emotion ELSE user_relations.recent_emotion END,
			emotion_updated_at = CASE WHEN excluded.recent_emotion <> '' THEN excluded.emotion_updated_at ELSE user_relations.emotion_updated_at END,
			updated_at = excluded.updated_at`,
		NormPlatform(platform), userID, scopeType, scopeID, impressionCount,
		impression, impressionCount, recentEmotion, emotionMs, nowMs,
	)
	return err
}
