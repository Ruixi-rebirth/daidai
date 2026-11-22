package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
)

type quotedMessageContext struct {
	Author           string                `json:"author,omitempty"`
	Content          string                `json:"content"`
	ContentKind      string                `json:"content_kind,omitempty"`
	ContentComplete  bool                  `json:"content_complete,omitempty"`
	ImageCount       int                   `json:"image_count,omitempty"`
	FileCount        int                   `json:"file_count,omitempty"`
	ImageObservation string                `json:"image_observation,omitempty"`
	OmittedImages    int                   `json:"omitted_images,omitempty"`
	OmittedFiles     int                   `json:"omitted_files,omitempty"`
	QuotedMessage    *quotedMessageContext `json:"quoted_message,omitempty"`
}

type savedDraftContext struct {
	Messages []core.ChatMessage `json:"messages"`
}

const resumeDraftInstruction = "请继续未完成的任务。保留之前完成的工作，不要重复调用已经完成的工具。"
const resumeDraftOutputInstruction = "根据草稿里的原始请求、已取得的资料和已写正文，接着完成对用户的回复。只写后续正文。"

func draftChatType(event *entity.SensoryEvent) string {
	if event.IsGroup {
		return "group"
	}
	return "private"
}

func (c *Cortex) saveDraft(ctx context.Context, event *entity.SensoryEvent, messages []core.ChatMessage) (string, error) {
	if c.drafts == nil || event == nil || event.SenderID == "" || event.ChannelID == "" {
		return "", fmt.Errorf("draft storage or owner identity unavailable")
	}
	messages = slices.Clone(messages)
	for i := range messages {
		if messages[i].RecoveryContent != "" {
			messages[i].Content = messages[i].RecoveryContent
		}
	}
	data, err := json.Marshal(savedDraftContext{Messages: messages})
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("%s-%d", event.Platform, time.Now().UnixNano())
	err = c.drafts.SaveConversationDraft(ctx, db.ConversationDraft{ID: id, Platform: event.Platform, ChatType: draftChatType(event), ConversationID: event.ChannelID, RequesterID: event.SenderID, Goal: core.TruncateRunes(event.Content, 240, "…"), ContextJSON: string(data), UpdatedAt: time.Now()})
	if err != nil {
		return "", err
	}
	return id, nil
}

// ContinueDraft resumes only a draft owned by this exact platform, chat, and sender.
// The string result is a user-facing notice when there is no utterance.
func (c *Cortex) ContinueDraft(ctx context.Context, event *entity.SensoryEvent, id string) (*entity.Utterance, string, error) {
	if c.drafts == nil || event == nil || event.SenderID == "" {
		return nil, "", fmt.Errorf("无法确认草稿所有者")
	}
	chatType := draftChatType(event)
	if id == "" {
		drafts, err := c.drafts.ListConversationDraftSummaries(ctx, event.Platform, chatType, event.ChannelID, event.SenderID)
		if err != nil {
			return nil, "", err
		}
		switch len(drafts) {
		case 0:
			return nil, "", nil
		case 1:
			id = drafts[0].ID
		default:
			var choices strings.Builder
			for i, draft := range drafts {
				fmt.Fprintf(&choices, "\n- %d. %s（任务 %s）", i+1, draft.Goal, draft.ID)
			}
			return nil, "你有多个未完成的任务，请回复“/continue <任务编号>”选择：" + choices.String(), nil
		}
	}
	selected, err := c.drafts.GetConversationDraft(ctx, event.Platform, chatType, event.ChannelID, event.SenderID, id)
	if err != nil {
		return nil, "", err
	}
	if selected == nil {
		return nil, "这个任务编号不属于你，或草稿已过期。", nil
	}
	var saved savedDraftContext
	if err := json.Unmarshal([]byte(selected.ContextJSON), &saved); err != nil {
		return nil, "", fmt.Errorf("读取续写草稿失败: %w", err)
	}
	if len(saved.Messages) == 0 {
		return nil, "", fmt.Errorf("续写草稿缺少模型上下文")
	}
	// Older checkpoints persisted synthetic continuation requests between segments.
	seenUserRequest := false
	saved.Messages = slices.DeleteFunc(saved.Messages, func(message core.ChatMessage) bool {
		if message.Role != core.RoleUser {
			return false
		}
		if seenUserRequest && message.Content == resumeDraftInstruction {
			return true
		}
		seenUserRequest = true
		return false
	})
	request := append(slices.Clone(saved.Messages), core.ChatMessage{Role: core.RoleUser, Content: resumeDraftOutputInstruction})
	if promptPayloadExceedsBudget(request, nil) {
		return nil, "这份任务草稿和已收集的资料超过单次续写容量，暂时无法可靠续写。草稿仍保留；请明确指定尚未完成的部分，或缩小范围后重新提问。", nil
	}
	resp, err := c.llm.ChatWithTools(ctx, request, nil, &core.ChatOptions{
		Temperature:   0.4,
		MaxTokens:     4096,
		ReadOnly:      event.Role == entity.RoleGuest,
		ReasoningOnly: true,
	})
	if err != nil {
		return nil, "", err
	}
	if resp == nil {
		return nil, "", fmt.Errorf("模型没有返回续写正文")
	}
	truncated := core.OutputLimitReached(resp.FinishReason)
	text := strings.TrimSpace(resp.Content)
	if truncated && text == "" {
		return nil, "模型在生成本段正文前达到输出上限，草稿仍保留。回复“/continue " + selected.ID + "”可重试。", nil
	}
	if len(resp.ToolCalls) > 0 && !truncated {
		return nil, "", fmt.Errorf("模型返回了工具请求，续写正文尚未完成")
	}
	if text == "" {
		return nil, "", fmt.Errorf("模型没有返回续写正文")
	}
	utterance := &entity.Utterance{
		Platform: event.Platform, IsGroup: event.IsGroup, ChannelID: event.ChannelID,
		QuoteMsgID: event.RawMessageID, DraftID: selected.ID, Text: text,
	}
	if truncated {
		saved.Messages = append(saved.Messages, core.ChatMessage{Role: core.RoleAssistant, Content: utterance.Text})
		data, err := json.Marshal(saved)
		if err != nil {
			return nil, "", err
		}
		if err := c.drafts.UpdateConversationDraft(ctx, *selected, string(data), time.Now()); err != nil {
			return nil, "", err
		}
		utterance.Text = core.CloseMarkdownFence(utterance.Text) + "\n\n[本段已保存，仍未完成。回复“/continue " + selected.ID + "”接着写。]"
	} else {
		completedDraft := *selected
		store := c.drafts
		utterance.OnDelivered = func(deliveryCtx context.Context) {
			if err := store.DeleteConversationDraft(deliveryCtx, completedDraft); err != nil {
				slog.Warn("cortex: 已交付续写但删除草稿失败", "draft_id", completedDraft.ID, "err", err)
			}
		}
	}
	return utterance, "", nil
}

func (c *Cortex) checkpointTruncatedReply(ctx context.Context, event *entity.SensoryEvent, messages []core.ChatMessage, response *core.ChatResponse) (text, draftID string) {
	partial := strings.TrimSpace(response.Content)
	if len(response.ToolCalls) > 0 {
		slog.Warn("cortex: 模型在工具调用中达到输出上限，丢弃不完整调用", "calls", len(response.ToolCalls))
	}
	if partial != "" {
		messages = append(append([]core.ChatMessage(nil), messages...), core.ChatMessage{Role: core.RoleAssistant, Content: partial})
		partial = core.CloseMarkdownFence(partial)
	} else {
		partial = "模型达到输出上限，没能生成完整回复。"
	}
	id, err := c.saveDraft(ctx, event, messages)
	if err != nil {
		slog.Error("cortex: 输出截断且草稿保存失败", "err", err)
		return partial + "\n\n[内容未完成，草稿保存失败，请重新提出剩余部分。]", ""
	}
	return partial + "\n\n[内容未完成，已保存进度。回复“/continue " + id + "”接着写。]", id
}

func (c *Cortex) checkpointToolResults(ctx context.Context, event *entity.SensoryEvent, messages []core.ChatMessage) (text, draftID string) {
	summary := "工具已返回信息，但我没能完成回答。"
	progress := pageReadProgressSummary(messages)
	id, err := c.saveDraft(ctx, event, messages)
	if err != nil {
		slog.Error("cortex: 保存已有工具结果失败", "err", err)
		return summary + "草稿未能保存。" + progress, ""
	}
	return summary + "已保存结果，回复“/continue " + id + "”接着写。" + progress, id
}

func pageReadProgressSummary(messages []core.ChatMessage) string {
	var urls []string
	statuses := make(map[string]string)
	for _, message := range messages {
		if p := message.PageProgress; p != nil {
			if _, seen := statuses[p.URL]; !seen {
				urls = append(urls, p.URL)
			}
			if previous := statuses[p.URL]; strings.HasPrefix(previous, "已读") && !strings.HasPrefix(p.Status, "已读") {
				statuses[p.URL] = "已读部分正文，本次接续未完成"
			} else {
				statuses[p.URL] = p.Status
			}
		}
	}
	if len(urls) == 0 {
		return ""
	}
	var summary strings.Builder
	summary.WriteString("\n\n网页进度")
	for _, target := range urls {
		fmt.Fprintf(&summary, "\n- %s · %s", statuses[target], core.TruncateRunes(target, 120, "…"))
	}
	return summary.String()
}

func quotedMessageContextFromEvent(event *entity.SensoryEvent, depth int, remaining *int) *quotedMessageContext {
	if event == nil || depth >= entity.MaxQuotedMessageDepth || remaining == nil || *remaining <= 0 {
		return nil
	}
	content := entity.SanitizeQuotedMessageContent(event.Content)
	complete := content != "" && event.ContentComplete && content == strings.TrimSpace(event.Content)
	nodeBudget := *remaining
	if event.ReplyContext != nil && depth+1 < entity.MaxQuotedMessageDepth {
		nodeBudget = (*remaining * 3) / 4
	}
	contentRunes := []rune(content)
	if len(contentRunes) > nodeBudget {
		complete = false
		content = string(contentRunes[:nodeBudget]) + "…"
		*remaining -= nodeBudget
	} else {
		*remaining -= len(contentRunes)
	}
	quoted := &quotedMessageContext{
		Author:           strings.TrimSpace(event.SenderName),
		Content:          content,
		ContentKind:      event.ContentKind,
		ContentComplete:  complete,
		ImageCount:       countAttachments(event.Images),
		FileCount:        countAttachments(event.Files),
		ImageObservation: core.TruncateRunes(strings.TrimSpace(event.VisionSummary), core.MaxVisionSummaryRunes, "\n[引用图片观察已截断]"),
		OmittedImages:    event.OmittedImages,
		OmittedFiles:     event.OmittedFiles,
	}
	quoted.QuotedMessage = quotedMessageContextFromEvent(event.ReplyContext, depth+1, remaining)
	return quoted
}

func countAttachments(attachments []*core.Attachment) int {
	count := 0
	for _, attachment := range attachments {
		if attachment != nil {
			count++
		}
	}
	return count
}
