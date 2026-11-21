package chat

import (
	"context"
	"strings"

	"daidai/internal/core"
	"daidai/internal/entity"
)

func (c *Cortex) finishConversation(
	ctx context.Context,
	event *entity.SensoryEvent,
	input *chatInput,
	readOnly bool,
	hasToolResult bool,
	messages []core.ChatMessage,
	media *toolMediaDelivery,
) (*entity.Utterance, error) {
	finalMessages := fitPromptContext(messages, input.workingHistoryStart, input.workingHistoryEnd, nil)
	if promptPayloadExceedsBudget(finalMessages, nil) {
		// Keep any requested attachments available for a narrower retry.
		event.ProcessedAttachments = nil
		if hasToolResult {
			return media.utterance(event, "本轮已经调用工具并收到部分结果，但这些结果加上请求内容超出了单轮整理范围，我没能完成回答。请把查询缩小到具体对象后重问。"), nil
		}
		return media.utterance(event, "本轮上下文超出单次处理容量，我没能完成回复。请缩小问题范围后重问。"), nil
	}
	finalResp, err := c.llm.ChatWithTools(ctx, finalMessages, nil, &core.ChatOptions{
		Temperature:   0.7,
		MaxTokens:     4096,
		ReadOnly:      readOnly,
		ReasoningOnly: true,
	})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if err == nil && finalResp != nil && core.OutputLimitReached(finalResp.FinishReason) {
		text, draftID := c.checkpointTruncatedReply(ctx, event, finalMessages, finalResp)
		reply := media.utterance(event, text)
		reply.DraftID = draftID
		return reply, nil
	}
	if err == nil && finalResp != nil && len(finalResp.ToolCalls) == 0 && strings.TrimSpace(finalResp.Content) != "" {
		return media.utterance(event, strings.TrimSpace(finalResp.Content)), nil
	}
	if hasToolResult {
		text, draftID := c.checkpointToolResults(ctx, event, finalMessages)
		reply := media.utterance(event, text)
		reply.DraftID = draftID
		return reply, nil
	}
	return media.utterance(event, "工具调用没有返回可确认的结果。"), nil
}
