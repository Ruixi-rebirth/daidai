package memory

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
	"daidai/internal/groupchat"
)

func historyUserText(event *entity.SensoryEvent) string {
	if event == nil {
		return ""
	}
	userText := strings.TrimSpace(core.TruncateRunes(event.Content, core.MaxUserMessageRunes, "\n[消息过长，后续内容已截断]"))
	if entity.IsSensitiveAuthCommand(event.Content) {
		userText = "[OAuth authorization code redacted]"
	}
	var notes []string
	if note := strings.TrimSpace(event.HistoryAttachmentNote); note != "" {
		notes = append(notes, note)
	}
	if summary := strings.TrimSpace(core.TruncateRunes(event.VisionSummary, core.MaxVisionSummaryRunes, "\n[视觉摘要过长，后续内容已截断]")); summary != "" {
		notes = append(notes, "[图片内容: "+summary+"]")
	}
	return core.AppendTextWithinRuneLimit(userText, strings.Join(notes, "\n"), core.MaxUserMessageRunes, "\n[原消息过长，已截短以保留附件信息]")
}

func shouldRecordHistoryEvent(event *entity.SensoryEvent) bool {
	if event == nil {
		return false
	}
	content := strings.TrimSpace(event.Content)
	// Unaddressed group media is kept in the pending attachment inbox, but its
	// placeholder must not become a conversation turn or pollute memory recall.
	if event.IsGroup && event.GroupMessageObserved && content == "" {
		return false
	}
	if strings.HasPrefix(content, "/") || content == "继续" || entity.IsSensitiveAuthCommand(content) {
		return false
	}
	return historyUserText(event) != ""
}

// RecordInbound persists each accepted raw user message before debounce or cognition can fail.
func (m *MemoryEngine) RecordInbound(ctx context.Context, event *entity.SensoryEvent) error {
	if m == nil || event == nil || event.Platform == "" || event.LastHistoryRecordID() > 0 || !shouldRecordHistoryEvent(event) {
		return nil
	}
	dbMgr := m.getDB()
	if dbMgr == nil {
		return nil
	}
	chatType := "private"
	if event.IsGroup {
		chatType = "group"
	}
	id, err := dbMgr.LogInboundMessage(ctx, event.Platform, chatType, event.ChannelID, event.SenderID, event.SenderName, historyUserText(event), event.RawMessageID)
	if err != nil {
		return err
	}
	event.HistoryRecordIDs = []int64{id}
	return nil
}

// Commit stores only delivered bot output and attaches derived vision text to the already persisted user row.
func (m *MemoryEngine) Commit(ctx context.Context, event *entity.SensoryEvent, replyText string) {
	dbMgr := m.getDB()
	if dbMgr == nil || event == nil || event.Platform == "" {
		return
	}

	chatType := "private"
	if event.IsGroup {
		chatType = "group"
	}

	visionSummary := core.TruncateRunes(event.VisionSummary, core.MaxVisionSummaryRunes, "\n[视觉摘要过长，后续内容已截断]")
	replyText = core.TruncateRunes(replyText, core.MaxUserMessageRunes, "\n[消息过长，后续内容已截断]")
	userText, userMsgID := m.commitInboundHistory(ctx, dbMgr, event, visionSummary)
	botMsgID := m.commitBotHistory(ctx, dbMgr, event, chatType, replyText, userMsgID)
	m.schedulePostCommit(dbMgr, event, userText, replyText, userMsgID, botMsgID)
}

func (m *MemoryEngine) commitInboundHistory(ctx context.Context, dbMgr *db.Manager, event *entity.SensoryEvent, visionSummary string) (string, int64) {
	if !shouldRecordHistoryEvent(event) {
		return "", 0
	}
	if event.LastHistoryRecordID() == 0 {
		if err := m.RecordInbound(ctx, event); err != nil {
			slog.Warn("memory: 保存入站消息失败", "platform", event.Platform, "channel", event.ChannelID, "err", err)
		}
	}
	userMsgID := event.LastHistoryRecordID()
	if userMsgID == 0 {
		return "", 0
	}
	var userText string
	var err error
	if visionSummary != "" {
		userText, err = dbMgr.AppendMessageText(ctx, event.Platform, userMsgID, "[图片内容: "+visionSummary+"]")
	} else {
		userText, err = dbMgr.MessageTextByID(ctx, event.Platform, userMsgID)
	}
	if err != nil {
		slog.Warn("memory: 更新入站消息记录失败", "platform", event.Platform, "channel", event.ChannelID, "err", err)
	}
	return userText, userMsgID
}

func (m *MemoryEngine) commitBotHistory(ctx context.Context, dbMgr *db.Manager, event *entity.SensoryEvent, chatType, replyText string, userMsgID int64) int64 {
	if replyText == "" {
		return 0
	}
	historyText := replyText
	if event.IsGroup && event.GroupExpression == groupchat.Dream {
		historyText = fmt.Sprintf("[%s的梦境联想：虚构故事，不是群成员的真实经历]\n%s", config.BotName(), replyText)
	}
	botMsgID, err := dbMgr.LogBotReply(ctx, event.Platform, chatType, event.ChannelID, historyText, event.SenderID, userMsgID)
	if err != nil {
		slog.Warn("memory: 保存机器人回复失败", "platform", event.Platform, "channel", event.ChannelID, "err", err)
	}
	return botMsgID
}
