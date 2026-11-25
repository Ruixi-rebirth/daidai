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
	ambiguousNames := collidingGroupSpeakerNames(recents[activeStart:], event)
	memberNames := groupMemberNamesByID(recents, event, ambiguousNames)

	// 从最新向最旧反向累计字符预算；闲置断档已由 activeConversationStart 统一裁定。
	var selected []core.ChatMessage
	totalChars := 0
	for i := len(recents) - 1; i >= activeStart; i-- {
		h := recents[i]
		role := core.RoleUser
		content := h.Text
		replyTarget := ""
		if h.IsBot {
			role = core.RoleAssistant
			if h.IsGroup {
				content = GroupPersonalMemoryCardHistoryText(content)
			}
			if h.IsGroup && h.ReplyToUID != "" {
				replyTarget = "other_member"
				if h.ReplyToUID == event.SenderID {
					replyTarget = "current_speaker"
				}
				if (event.GroupInterjection || h.ReplyToUID != event.SenderID) && !strings.HasPrefix(strings.TrimSpace(content), "[") {
					if targetLabel := memberNames[h.ReplyToUID]; targetLabel != "" {
						content = fmt.Sprintf("[对 %s说]: %s", targetLabel, content)
					}
				}
			}
		} else if h.IsGroup {
			speaker := formatGroupSpeakerLabel(h.Name, h.UserID, ambiguousNames)
			content = fmt.Sprintf("[%s]: %s", speaker, h.Text)
		}
		content = core.TruncateRunes(content, core.MaxUserMessageRunes, "\n[历史消息过长，后续内容已截断]")
		cLen := utf8.RuneCountInString(content)
		if len(selected) > 0 && totalChars+cLen > maxWorkingChars {
			break
		}
		totalChars += cLen
		selected = append(selected, core.ChatMessage{
			Role: role, Content: content, GroupReplyTarget: replyTarget,
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

func collidingGroupSpeakerNames(messages []db.StoredMessage, event *entity.SensoryEvent) map[string]bool {
	if event == nil || !event.IsGroup {
		return nil
	}
	firstUserByName := make(map[string]string)
	colliding := make(map[string]bool)
	record := func(name, userID string) {
		name = strings.TrimSpace(name)
		userID = strings.TrimSpace(userID)
		if name == "" || userID == "" {
			return
		}
		if prev, exists := firstUserByName[name]; exists && prev != userID {
			colliding[name] = true
			return
		}
		firstUserByName[name] = userID
	}
	for _, msg := range messages {
		if !msg.IsBot {
			record(msg.Name, msg.UserID)
		}
	}
	record(event.SenderName, event.SenderID)
	if len(colliding) == 0 {
		return nil
	}
	return colliding
}

func formatGroupSpeakerLabel(name, userID string, colliding map[string]bool) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		if sid := shortID(strings.TrimSpace(userID)); sid != "" {
			return "群成员#" + sid
		}
		return "群成员"
	}
	if colliding[trimmed] {
		if sid := shortID(strings.TrimSpace(userID)); sid != "" {
			return trimmed + "#" + sid
		}
	}
	return trimmed
}

func groupMemberNamesByID(messages []db.StoredMessage, event *entity.SensoryEvent, colliding map[string]bool) map[string]string {
	if event == nil || !event.IsGroup {
		return nil
	}
	names := make(map[string]string)
	put := func(userID, name string) {
		uid := strings.TrimSpace(userID)
		if uid == "" {
			return
		}
		if _, exists := names[uid]; exists && strings.TrimSpace(name) == "" {
			return
		}
		names[uid] = formatGroupSpeakerLabel(name, uid, colliding)
	}
	for _, msg := range messages {
		if !msg.IsBot {
			put(msg.UserID, msg.Name)
		}
	}
	for _, p := range event.GroupParticipants {
		put(p.ID, p.Name)
	}
	for _, u := range event.MentionedUsers {
		put(u.ID, u.Name)
	}
	put(event.SenderID, event.SenderName)
	return names
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

// RecallEpisodic 回忆关于用户的长期情景记忆画像、交情分寸与跨场次历史脉络 (实现 entity.Memory)
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
			sections = append(sections, fmt.Sprintf("【当前对话用户 (%s) 的特征与长期记忆（仅作自然背景，无关话题时绝不生硬提及）】：\n%s", event.SenderName, strings.Join(lines, "\n")))
		} else {
			sections = append(sections, fmt.Sprintf("【用户特征与长期记忆（仅作自然背景，无关话题时绝不生硬提及）】：\n%s", strings.Join(lines, "\n")))
		}
	}

	anchorTime := event.OccurredAt
	if anchorTime.IsZero() {
		anchorTime = time.Now()
	}

	if relationSection := m.recallRelationContext(ctx, event, anchorTime); relationSection != "" {
		sections = append(sections, relationSection)
	}

	if dbMgr := m.getDB(); dbMgr != nil && event.ChannelID != "" {
		chatType := "private"
		if event.IsGroup {
			chatType = "group"
		}
		boundary := event.HistoryBoundaryID()
		if recents, msgErr := dbMgr.RecentMessagesBefore(ctx, event.Platform, chatType, event.ChannelID, 40, boundary); msgErr == nil && len(recents) > 0 {
			activeStart := activeConversationStart(recents, anchorTime)
			if peerThread := recallGroupPeerThread(recents[activeStart:], event); peerThread != "" {
				sections = append(sections, peerThread)
			}
			if digest := formatPreviousSessionDigest(recents, activeStart, anchorTime); digest != "" {
				sections = append(sections, digest)
			}
		}
	}

	return strings.Join(sections, "\n\n")
}

const emotionContinuityTTL = 6 * time.Hour

func effectiveInteractionCount(ctx context.Context, dbMgr *db.Manager, event *entity.SensoryEvent, rel *db.UserRelation) int {
	interactions := 0
	if rel != nil {
		interactions = rel.InteractionCount
	}
	// Preserve natural familiarity for members who already have chat history
	// before user_relations began accumulating turns.
	if dbMgr != nil && event != nil && interactions < 30 && strings.TrimSpace(event.ChannelID) != "" {
		chatType := "private"
		if event.IsGroup {
			chatType = "group"
		}
		if recentMsgs, msgErr := dbMgr.RecentUserMessagesInChat(ctx, event.Platform, chatType, event.ChannelID, event.SenderID, 35); msgErr == nil && len(recentMsgs) > interactions {
			interactions = len(recentMsgs)
		}
	}
	return interactions
}

func (m *MemoryEngine) recallRelationContext(ctx context.Context, event *entity.SensoryEvent, anchorTime time.Time) string {
	dbMgr := m.getDB()
	if dbMgr == nil {
		return ""
	}
	rel, err := dbMgr.GetVisibleUserRelation(ctx, event.Platform, event.SenderID, event.ChannelID, event.IsGroup)
	if err != nil {
		return ""
	}
	interactions := effectiveInteractionCount(ctx, dbMgr, event, rel)

	var notes []string
	switch {
	case interactions >= 30:
		notes = append(notes, "【熟悉度分寸】你们已经很熟：说话可省去客套、自然接梗或轻吐槽，但不要肉麻、不要报熟悉度数值，也不推断现实关系。")
	case interactions >= 10:
		notes = append(notes, "【熟悉度分寸】双方比较熟：语气可以更自然简短，少点客套，偶尔顺着话头开轻松玩笑；不推断现实关系。")
	case interactions >= 3:
		notes = append(notes, "【熟悉度分寸】已有少量交流：保持自然放松，不必拘谨寒暄，也别装多年老友。")
	}

	if rel != nil {
		if imp := strings.TrimSpace(rel.Impression); imp != "" {
			notes = append(notes, "【你心里对 TA 的印象（只作相处语气底色，绝不生硬背诵或强行扯进无关话题）】："+imp)
		}
		if emo := strings.TrimSpace(rel.RecentEmotion); emo != "" && !rel.EmotionUpdatedAt.IsZero() {
			age := anchorTime.Sub(rel.EmotionUpdatedAt)
			if age >= 0 && age <= emotionContinuityTTL {
				emoNote := fmt.Sprintf("【对方近期情绪底色】：%s（仅作接话温度参考，不要明说“看你情绪不好”，不心理分析、不说教", emo)
				if interactions >= 3 && (emo == "疲惫" || emo == "烦躁" || emo == "低落") {
					emoNote += "；你们已熟识，语气可轻轻放软一点，简短自然，别像客服安慰"
				}
				emoNote += "）。"
				notes = append(notes, emoNote)
			}
		}
	}
	return strings.Join(notes, "\n")
}

// recallGroupPeerThread extracts a compact slice of recent exchanges between
// Daidai and the current speaker when a busy group has interleaved messages
// from other members, preventing speaker cross-talk.
func recallGroupPeerThread(active []db.StoredMessage, event *entity.SensoryEvent) string {
	if event == nil || !event.IsGroup || event.SenderID == "" || len(active) < 4 {
		return ""
	}
	var peerTurns []db.StoredMessage
	otherSpeakerCount := 0
	seenPeerTurn := false
	for _, msg := range active {
		isPeer := (!msg.IsBot && msg.UserID == event.SenderID) || (msg.IsBot && msg.ReplyToUID == event.SenderID)
		if isPeer {
			peerTurns = append(peerTurns, msg)
			seenPeerTurn = true
			continue
		}
		if seenPeerTurn && !msg.IsBot {
			otherSpeakerCount++
		}
	}
	if len(peerTurns) < 2 || otherSpeakerCount < 3 {
		return ""
	}
	if len(peerTurns) > 6 {
		peerTurns = peerTurns[len(peerTurns)-6:]
	}
	speakerLabel := strings.TrimSpace(event.SenderName)
	if speakerLabel == "" {
		speakerLabel = "当前说话人"
	}
	var lines []string
	for _, msg := range peerTurns {
		who := speakerLabel
		text := msg.Text
		if msg.IsBot {
			who = config.BotName()
			text = GroupPersonalMemoryCardHistoryText(text)
		}
		text = strings.ReplaceAll(strings.TrimSpace(text), "\n", " ")
		text = core.TruncateRunes(text, maxSingleDigestRunes, "…")
		if text != "" {
			lines = append(lines, fmt.Sprintf("- [%s]: %s", who, text))
		}
	}
	if len(lines) < 2 {
		return ""
	}
	return fmt.Sprintf("【你与当前说话人 (%s) 本场近期的对话线（群内多人插话时重点跟住这条线，勿把别人的话题安到 TA 头上）】：\n%s", speakerLabel, strings.Join(lines, "\n"))
}

// formatPreviousSessionDigest 提取因超过 2 小时闲置断档而退出活跃滑动窗口的上一场对话脉络摘要，避免跨时段失忆
func formatPreviousSessionDigest(recents []db.StoredMessage, activeStart int, anchorTime time.Time) string {
	if activeStart == 0 || len(recents) == 0 {
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
		text := h.Text
		if h.IsBot && h.IsGroup {
			text = GroupPersonalMemoryCardHistoryText(text)
		}
		text = strings.ReplaceAll(strings.TrimSpace(text), "\n", " ")
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

// GroupPersonalMemoryCardHistoryText removes another member's personal card
// from shared group context while preserving the fact that the card was shown.
func GroupPersonalMemoryCardHistoryText(text string) string {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "# 💌 ") &&
		strings.Contains(trimmed, "### ✨ 我记得的事") &&
		strings.Contains(trimmed, "这张卡只展示") {
		return "[呆呆已向当时的群成员展示个人记忆卡；卡片内容不属于当前发言人]"
	}
	return text
}
