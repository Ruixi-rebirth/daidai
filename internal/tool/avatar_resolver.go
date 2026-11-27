package tool

import (
	"context"
	"fmt"
	"strings"

	"daidai/internal/db"
	"daidai/internal/entity"
	"daidai/internal/media"
)

// AvatarResolver maps a natural-language user target in the current conversation
// (e.g. "self", a mentioned member, a quoted author, or a recent group member's nickname)
// to an opaque platform avatar media reference (e.g. "qq-avatar:<openid>").
type AvatarResolver struct {
	database *db.Manager
	sources  *media.Sources
}

// NewAvatarResolver constructs a reusable avatar reference resolver.
func NewAvatarResolver(database *db.Manager, sources *media.Sources) *AvatarResolver {
	return &AvatarResolver{
		database: database,
		sources:  sources,
	}
}

func avatarSchemeForPlatform(platform string) string {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "qq":
		return "qq-avatar"
	default:
		return ""
	}
}

// SupportsPlatform reports whether avatar media loading is available for platform.
func (r *AvatarResolver) SupportsPlatform(platform string) bool {
	if r == nil || r.sources == nil {
		return false
	}
	scheme := avatarSchemeForPlatform(platform)
	return scheme != "" && r.sources.HasScheme(scheme)
}

// Resolve resolves target into (displayName, opaqueMediaRef).
func (r *AvatarResolver) Resolve(ctx context.Context, event *entity.SensoryEvent, target string) (string, string, error) {
	if event == nil {
		return "", "", fmt.Errorf("缺少当前会话上下文，无法解析用户头像")
	}
	scheme := avatarSchemeForPlatform(event.Platform)
	if scheme == "" || r == nil || r.sources == nil || !r.sources.HasScheme(scheme) {
		return "", "", fmt.Errorf("当前平台（%s）暂不支持获取用户头像", event.Platform)
	}

	userID, displayName, err := r.resolveMember(ctx, event, target)
	if err != nil {
		return "", "", err
	}
	if displayName == "" {
		displayName = userID
	}
	return displayName, scheme + ":" + userID, nil
}

func isSelfTarget(target string) bool {
	switch strings.ToLower(strings.TrimSpace(target)) {
	case "", "self", "me", "自己", "我", "本人", "我的":
		return true
	default:
		return false
	}
}

func matchesMemberTarget(target, id, name string) bool {
	t := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(target, "@"), "＠"))
	if t == "" {
		return false
	}
	if id != "" && strings.EqualFold(t, strings.TrimSpace(id)) {
		return true
	}
	if name != "" && strings.EqualFold(t, strings.TrimSpace(name)) {
		return true
	}
	return false
}

func (r *AvatarResolver) resolveMember(ctx context.Context, event *entity.SensoryEvent, target string) (string, string, error) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(target, "@"), "＠"))

	// 1. Self / empty target -> current speaker.
	if isSelfTarget(trimmed) || matchesMemberTarget(trimmed, event.SenderID, event.SenderName) {
		if strings.TrimSpace(event.SenderID) == "" {
			return "", "", fmt.Errorf("无法识别当前发言人身份")
		}
		return strings.TrimSpace(event.SenderID), strings.TrimSpace(event.SenderName), nil
	}

	// 2. Explicitly @-mentioned users in the current message.
	for _, m := range event.MentionedUsers {
		if matchesMemberTarget(trimmed, m.ID, m.Name) && strings.TrimSpace(m.ID) != "" {
			return strings.TrimSpace(m.ID), strings.TrimSpace(m.Name), nil
		}
	}

	// 3. Author of the quoted message.
	if event.ReplyContext != nil && matchesMemberTarget(trimmed, event.ReplyContext.SenderID, event.ReplyContext.SenderName) && strings.TrimSpace(event.ReplyContext.SenderID) != "" {
		return strings.TrimSpace(event.ReplyContext.SenderID), strings.TrimSpace(event.ReplyContext.SenderName), nil
	}

	// 4. Known recent group participants attached to the event.
	for _, p := range event.GroupParticipants {
		if matchesMemberTarget(trimmed, p.ID, p.Name) && strings.TrimSpace(p.ID) != "" {
			return strings.TrimSpace(p.ID), strings.TrimSpace(p.Name), nil
		}
	}

	// 5. Recent conversation history in SQLite (newest first).
	if r.database != nil && event.ChannelID != "" {
		chatType := "private"
		if event.IsGroup {
			chatType = "group"
		}
		msgs, err := r.database.RecentMessages(ctx, event.Platform, chatType, event.ChannelID, 200)
		if err == nil {
			// Pass 1: exact (case-insensitive) match on user_id or name.
			for i := len(msgs) - 1; i >= 0; i-- {
				m := msgs[i]
				if m.IsBot || strings.TrimSpace(m.UserID) == "" {
					continue
				}
				if matchesMemberTarget(trimmed, m.UserID, m.Name) {
					return strings.TrimSpace(m.UserID), strings.TrimSpace(m.Name), nil
				}
			}
			// Pass 2: substring match on nickname when target is at least 1 rune.
			lowerTarget := strings.ToLower(trimmed)
			for i := len(msgs) - 1; i >= 0; i-- {
				m := msgs[i]
				if m.IsBot || strings.TrimSpace(m.UserID) == "" || strings.TrimSpace(m.Name) == "" {
					continue
				}
				if strings.Contains(strings.ToLower(strings.TrimSpace(m.Name)), lowerTarget) {
					return strings.TrimSpace(m.UserID), strings.TrimSpace(m.Name), nil
				}
			}
		}
	}

	return "", "", fmt.Errorf("在当前会话中未找到名为「%s」的成员（请先 @ 对方或等对方发过消息后再试）", trimmed)
}
