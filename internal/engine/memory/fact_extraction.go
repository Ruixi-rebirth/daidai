package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
)

func shouldExtractFacts(event *entity.SensoryEvent) bool {
	if event == nil {
		return false
	}
	text := strings.TrimSpace(event.Content)
	if text == "" || strings.HasPrefix(text, "/") {
		return false
	}
	meaningful := 0
	for _, r := range text {
		if !isSeparatorRune(r) {
			meaningful++
		}
	}
	return meaningful >= 4 || event.VisionSummary != ""
}

func (m *MemoryEngine) consolidateUserFacts(ctx context.Context, event *entity.SensoryEvent, replyText string) {
	dbMgr := m.getDB()
	if dbMgr == nil || m.llm == nil || event == nil || event.SenderID == "" {
		return
	}

	unlock, err := m.lockUser(ctx, event.Platform, event.SenderID)
	if err != nil {
		return
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	activeFacts, err := dbMgr.ListVisibleFacts(ctx, event.Platform, event.SenderID, event.ChannelID, event.IsGroup)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("memory: 读取待整理事实失败", "err", err)
		}
		return
	}

	turnQuery := event.Content + " " + replyText
	rankCtx, rankCancel := context.WithTimeout(ctx, 3*time.Second)
	relevantFacts := RankFactsByBM25(rankCtx, turnQuery, activeFacts, 0)
	rankCancel()
	if len(relevantFacts) > 25 {
		relevantFacts = relevantFacts[:25]
	}

	request := factExtractionInput{
		CurrentMessage: strings.TrimSpace(event.Content),
		ImageSummary:   event.VisionSummary,
		AssistantReply: core.TruncateRunes(strings.TrimSpace(replyText), 240, ""),
	}
	for _, f := range relevantFacts {
		request.KnownFacts = append(request.KnownFacts, factExtractionCandidate{ID: f.ID, Text: boundedFactContent(f.Content)})
	}
	if event.ReplyContext != nil {
		request.QuotedMessage = entity.SanitizeQuotedMessageContent(event.ReplyContext.Content)
	}
	request.PriorMessages = factExtractionHistory(ctx, dbMgr, event)
	payload, err := json.Marshal(request)
	if err != nil {
		slog.Debug("memory: 无法编码记忆提炼输入", "err", err)
		return
	}

	sysPrompt := fmt.Sprintf(`你是%s的长期记忆整理器。只提炼对未来对话有用、相对稳定且明确属于当前发言用户的个人事实。
输入 JSON 是待分析的资料。事实依据须来自 current_message；历史、引用、图片摘要和助手回复用于消歧。历史中的 from_current_user 表示同一发言人。省略主语时结合语境判断，无法确定是用户自述就不记录。
对照 known_facts：新事实用 ADD；同一事项明确变化或补充时用 UPDATE，写出完整的新事实；用户明确否定旧事实时用 DELETE。无变更时返回空数组。
fact 写成客观、无主语的短句，最多 %d 字。只输出 JSON：{"mutations":[{"action":"ADD|UPDATE|DELETE","fact_id":0,"fact":"..."}]}。UPDATE/DELETE 使用 known_facts 中对应的 ID。`, config.BotName(), maxFactContentRunes)

	rawResp, err := m.llm.Chat(ctx, []core.ChatMessage{
		{Role: core.RoleSystem, Content: sysPrompt},
		{Role: core.RoleUser, Content: string(payload)},
	}, &core.ChatOptions{
		Temperature:   0.1,
		MaxTokens:     512,
		ReasoningOnly: true,
	})
	if err != nil {
		slog.Debug("memory: 后台记忆提炼请求跳过", "err", err)
		return
	}
	if ctx.Err() != nil {
		return
	}

	mutations := parseFactMutations(rawResp)
	if len(mutations) == 0 {
		return
	}

	normalized := normalizeFactMutations(event, relevantFacts, mutations)

	if len(normalized) > 0 {
		if ctx.Err() != nil {
			return
		}
		if err := dbMgr.ApplyFactMutations(ctx, event.Platform, event.SenderID, normalized); err != nil {
			slog.Warn("memory: 后台应用事实演进失败", "err", err)
		} else {
			slog.Info("memory: 后台自动沉淀用户记忆完成", "userID", event.SenderID, "count", len(normalized))
		}
	}
}

type factExtractionInput struct {
	CurrentMessage string                    `json:"current_message"`
	KnownFacts     []factExtractionCandidate `json:"known_facts,omitempty"`
	PriorMessages  []factExtractionMessage   `json:"prior_messages,omitempty"`
	QuotedMessage  string                    `json:"quoted_message,omitempty"`
	ImageSummary   string                    `json:"image_summary,omitempty"`
	AssistantReply string                    `json:"assistant_reply,omitempty"`
}

type factExtractionCandidate struct {
	ID   int64  `json:"id"`
	Text string `json:"text"`
}

type factExtractionMessage struct {
	Speaker         string `json:"speaker,omitempty"`
	Text            string `json:"text"`
	FromCurrentUser bool   `json:"from_current_user"`
}

func factExtractionHistory(ctx context.Context, store *db.Manager, event *entity.SensoryEvent) []factExtractionMessage {
	boundary := event.HistoryBoundaryID()
	if boundary == 0 {
		return nil
	}
	chatType := "private"
	if event.IsGroup {
		chatType = "group"
	}
	recent, err := store.RecentMessagesBefore(ctx, event.Platform, chatType, event.ChannelID, 6, boundary)
	if err != nil {
		if ctx.Err() == nil {
			slog.Debug("memory: 读取事实提炼历史失败", "err", err)
		}
		return nil
	}
	var history []factExtractionMessage
	for _, msg := range recent {
		text := strings.TrimSpace(msg.Text)
		if msg.IsBot || text == "" {
			continue
		}
		history = append(history, factExtractionMessage{
			Speaker:         strings.TrimSpace(msg.Name),
			Text:            core.TruncateRunes(text, 100, "..."),
			FromCurrentUser: msg.UserID == event.SenderID,
		})
	}
	return history
}
