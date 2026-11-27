package qq

import (
	"strings"
	"time"

	"daidai/internal/config"
	"daidai/internal/entity"
	"daidai/internal/groupchat"
)

func (q *QQChannel) continuationTracker() *groupchat.ContinuationTracker {
	if q == nil {
		return nil
	}
	q.continuationsOnce.Do(func() {
		if q.continuations == nil {
			q.continuations = groupchat.NewContinuationTracker()
		}
	})
	return q.continuations
}

// recordGroupActiveSession 显式唤醒、插话发声或引用回复呆呆时，建立或重置会话
func (q *QQChannel) recordGroupActiveSession(groupID, speakerID string) {
	if tracker := q.continuationTracker(); tracker != nil {
		tracker.Record(groupID, speakerID, time.Now())
	}
}

// evaluateGroupContinuation 将 QQ 协议层事实交给平台无关的 ContinuationTracker 判定。
func (q *QQChannel) evaluateGroupContinuation(
	groupID, userID, content string,
	eventData map[string]any,
	replyCtx *entity.SensoryEvent,
	quotedBot bool,
) groupchat.ContinuationDecision {
	tracker := q.continuationTracker()
	if tracker == nil {
		return groupchat.ContinuationNone
	}
	trimmed := strings.TrimSpace(content)
	namedBot := (config.BotName() != "" && strings.Contains(content, config.BotName())) || isBotWakeNickname(trimmed)
	return tracker.Evaluate(groupchat.ContinuationInput{
		GroupID:        groupID,
		SpeakerID:      userID,
		Content:        content,
		MentionedOther: qqDirectedAtOtherMember(eventData, trimmed),
		QuotedOther:    replyCtx != nil && !quotedBot,
		QuotedBot:      quotedBot,
		NamedBot:       namedBot,
		Now:            time.Now(),
	})
}

func qqDirectedAtOtherMember(data map[string]any, text string) bool {
	if qqMessageMentionsBot(data) {
		return false
	}
	mentions, _ := data["mentions"].([]any)
	return len(mentions) > 0 || strings.HasPrefix(strings.TrimSpace(text), "@") || strings.HasPrefix(strings.TrimSpace(text), "＠")
}

// isBotWakeNickname 判断文本是否提及机器人的常用称谓/昵称
func isBotWakeNickname(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "呆呆") ||
		strings.Contains(lower, "阿呆") ||
		strings.Contains(lower, "小呆") ||
		strings.Contains(lower, "机器人")
}
