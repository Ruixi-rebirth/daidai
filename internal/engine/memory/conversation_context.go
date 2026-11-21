package memory

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
)

const (
	maxWorkingChars          = 12000
	maxIdleGap               = 2 * time.Hour
	defaultWorkingCandidates = 200
	maxBridgeAge             = 24 * time.Hour
	maxBridgeActiveMsgs      = 4
	maxPromptFacts           = 12
	maxPrevSessionMsgs       = 8
	maxPrevSessionRunes      = 1500
	maxSingleDigestRunes     = 160
)

// WorkingContext returns the recent sliding window of conversation history.
func (m *MemoryEngine) WorkingContext(ctx context.Context, event *entity.SensoryEvent, limits ...int) []core.ChatMessage {
	dbMgr := m.getDB()
	if dbMgr == nil || event == nil || event.Platform == "" {
		return nil
	}

	limit := defaultWorkingCandidates
	if len(limits) > 0 && limits[0] > 0 {
		limit = limits[0]
	}

	chatType := "private"
	if event.IsGroup {
		chatType = "group"
	}

	boundary := event.HistoryBoundaryID()
	recents, err := dbMgr.RecentMessagesBefore(ctx, event.Platform, chatType, event.ChannelID, limit, boundary)
	if err != nil || len(recents) == 0 {
		return nil
	}

	anchorTime := event.OccurredAt
	if anchorTime.IsZero() {
		anchorTime = time.Now()
	}
	activeStart := activeConversationStart(recents, anchorTime)

	// 从最新向最旧反向累计字符预算；闲置断档已由 activeConversationStart 统一裁定。
	var selected []core.ChatMessage
	totalChars := 0
	for i := len(recents) - 1; i >= activeStart; i-- {
		h := recents[i]
		role := core.RoleUser
		content := h.Text
		if h.IsBot {
			role = core.RoleAssistant
		} else if h.IsGroup {
			speaker := h.Name
			if speaker == "" {
				speaker = "群成员#" + shortID(h.UserID)
			}
			content = fmt.Sprintf("[%s]: %s", speaker, h.Text)
		}
		content = core.TruncateRunes(content, core.MaxUserMessageRunes, "\n[历史消息过长，后续内容已截断]")
		cLen := utf8.RuneCountInString(content)
		if len(selected) > 0 && totalChars+cLen > maxWorkingChars {
			break
		}
		totalChars += cLen
		selected = append(selected, core.ChatMessage{
			Role:    role,
			Content: content,
		})
	}

	// 逆序翻转回正序 (按时间自然演进呈现给大模型)
	msgs := make([]core.ChatMessage, len(selected))
	for i, msg := range selected {
		msgs[len(selected)-1-i] = msg
	}
	return msgs
}

func shortID(id string) string {
	if len(id) > 6 {
		return id[:6]
	}
	return id
}

// activeConversationStart returns the first message after the latest idle gap.
// Both the working window and the previous-session bridge use this same boundary.
func activeConversationStart(messages []db.StoredMessage, anchorTime time.Time) int {
	previous := anchorTime
	for i := len(messages) - 1; i >= 0; i-- {
		createdAt := messages[i].CreatedAt
		if !createdAt.IsZero() && previous.Sub(createdAt) > maxIdleGap {
			return i + 1
		}
		if !createdAt.IsZero() {
			previous = createdAt
		}
	}
	return 0
}

// RecallEpisodic 回忆关于用户的长期情景记忆画像与跨场次历史脉络 (实现 entity.Memory)
func (m *MemoryEngine) RecallEpisodic(ctx context.Context, event *entity.SensoryEvent) string {
	if event == nil || event.Platform == "" || event.SenderID == "" {
		return ""
	}

	var sections []string

	// Keep only a compact, query-ranked memory slice in every chat request.
	// The complete profile remains in SQLite and the memory tool can retrieve
	// additional facts on demand; injecting the whole profile here makes every
	// turn pay the context cost even when those facts are unrelated.
	facts, err := m.RecallProfileForEvent(ctx, event, event.Content, maxPromptFacts)
	if err == nil && len(facts) > 0 {
		var lines []string
		for _, f := range facts {
			lines = append(lines, fmt.Sprintf("- [ID: %d][%s] %s", f.ID, f.ScopeLabel(), boundedFactContent(f.Content)))
		}
		if event.SenderName != "" {
			sections = append(sections, fmt.Sprintf("【当前对话用户 (%s) 的特征与长期记忆】：\n%s", event.SenderName, strings.Join(lines, "\n")))
		} else {
			sections = append(sections, fmt.Sprintf("【用户特征与长期记忆】：\n%s", strings.Join(lines, "\n")))
		}
	}

	if digest := m.recallPreviousSessionDigest(ctx, event); digest != "" {
		sections = append(sections, digest)
	}

	return strings.Join(sections, "\n\n")
}

// recallPreviousSessionDigest 提取因超过 2 小时闲置断档而退出活跃滑动窗口的上一场对话脉络摘要，避免跨时段失忆
func (m *MemoryEngine) recallPreviousSessionDigest(ctx context.Context, event *entity.SensoryEvent) string {
	dbMgr := m.getDB()
	if dbMgr == nil || event == nil || event.Platform == "" || event.ChannelID == "" {
		return ""
	}

	chatType := "private"
	if event.IsGroup {
		chatType = "group"
	}

	boundary := event.HistoryBoundaryID()
	recents, err := dbMgr.RecentMessagesBefore(ctx, event.Platform, chatType, event.ChannelID, 30, boundary)
	if err != nil || len(recents) == 0 {
		return ""
	}

	anchorTime := event.OccurredAt
	if anchorTime.IsZero() {
		anchorTime = time.Now()
	}

	activeStart := activeConversationStart(recents, anchorTime)
	if activeStart == 0 {
		return ""
	}

	// 仅在新一场对话刚开始（活跃轮次 <= 4 条）且上一场对话发生在 24 小时以内时提供跨场次过渡脉络；
	// 一旦新话题已深入展开或旧对话已是数天前的陈年记录，不再每轮重复注入旧摘要
	activeMsgs := len(recents) - activeStart
	if activeMsgs > maxBridgeActiveMsgs {
		return ""
	}
	if lastOld := recents[activeStart-1].CreatedAt; !lastOld.IsZero() && anchorTime.Sub(lastOld) > maxBridgeAge {
		return ""
	}

	older := recents[:activeStart]
	if len(older) > maxPrevSessionMsgs {
		older = older[len(older)-maxPrevSessionMsgs:]
	}

	var lines []string
	totalRunes := 0
	for _, h := range older {
		speaker := "用户"
		if h.IsBot {
			speaker = config.BotName()
		} else if h.Name != "" {
			speaker = h.Name
		}
		text := strings.ReplaceAll(strings.TrimSpace(h.Text), "\n", " ")
		if r := []rune(text); len(r) > maxSingleDigestRunes {
			text = string(r[:maxSingleDigestRunes]) + "..."
		}
		ts := ""
		if !h.CreatedAt.IsZero() {
			ts = h.CreatedAt.Format("01-02 15:04") + " "
		}
		line := fmt.Sprintf("- [%s%s]: %s", ts, speaker, text)
		lRunes := utf8.RuneCountInString(line)
		if totalRunes+lRunes > maxPrevSessionRunes {
			break
		}
		totalRunes += lRunes
		lines = append(lines, line)
	}

	if len(lines) == 0 {
		return ""
	}

	return fmt.Sprintf("【上一场对话历史脉络（距今已间隔超 2 小时，仅供跨场次背景参考）】：\n%s", strings.Join(lines, "\n"))
}
