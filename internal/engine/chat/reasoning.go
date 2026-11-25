package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
)

type thinkPresentation struct {
	mediaCaption          string
	mediaInline           bool
	mediaCommits          []func(context.Context)
	mediaFailures         []func(context.Context, error)
	toolFixedPresentation bool
}

func (c *Cortex) runConversation(ctx context.Context, event *entity.SensoryEvent, input *chatInput, presentation *thinkPresentation) (_ *entity.Utterance, replyErr error) {
	messages := input.messages
	readOnly := !event.IsPartnerPrivate()
	toolDefs := toolsForEvent(c.hands, event, input.imageSources)
	toolCtx := entity.WithSensoryEvent(ctx, event)
	var hasToolResult bool
	var media toolMediaDelivery
	draftReply := func(text, draftID string) *entity.Utterance {
		reply := media.utterance(event, text)
		reply.DraftID = draftID
		return reply
	}
	fallbackReply := func(promptMessages []core.ChatMessage) (*entity.Utterance, bool) {
		if hasToolResult {
			text, draftID := c.checkpointToolResults(ctx, event, promptMessages)
			return draftReply(text, draftID), true
		}
		if media.hasContent(presentation) {
			return media.fallbackUtterance(event), true
		}
		return nil, false
	}
	seenToolCalls := make(map[string]struct{})
	toolCallsUsed := 0
	var checkpoint []core.ChatMessage
	var needsSynthesis bool
	defer func() {
		if len(checkpoint) == 0 || !errors.Is(replyErr, context.DeadlineExceeded) ||
			ctx.Err() != context.DeadlineExceeded || (event.StillValid != nil && !event.StillValid()) {
			return
		}
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		messages := fitPromptContext(checkpoint, input.workingHistoryStart, input.workingHistoryEnd, nil)
		id, err := c.saveDraft(saveCtx, event, messages)
		if err != nil {
			slog.Error("cortex: 对话超时后保存工具结果失败", "err", err)
			return
		}
		replyErr = &entity.ReplyDeadlineWithDraft{DraftID: id, Progress: pageReadProgressSummary(messages)}
	}()

	// 单次心智推理与多轮工具调用循环
	for round := 0; round < 4; round++ {
		activeTools := toolDefs
		exhausted := toolCallsUsed >= maxToolCallsPerThink
		if exhausted {
			activeTools = nil
		}

		promptMessages := fitPromptContext(messages, input.workingHistoryStart, input.workingHistoryEnd, activeTools)
		if promptPayloadExceedsBudget(promptMessages, activeTools) {
			// Keep user-selected attachments pending so they can be retried with
			// a narrower request instead of consuming them on a budget notice.
			event.ProcessedAttachments = nil
			if !hasToolResult && !media.hasContent(presentation) {
				return media.utterance(event, "这条消息携带的内容超过了单轮可处理范围，我还没开始处理。请缩小问题范围；如果有多个附件，分批发送更稳。"), nil
			}
			if hasToolResult {
				return media.utterance(event, "本轮已经调用工具并收到部分结果，但这些结果加上请求内容超出了单轮整理范围，我没能完成回答。请把查询缩小到具体对象后重问。"), nil
			}
			return media.utterance(event, "图片或其他内容已经准备好，但本轮上下文超出处理容量，没能完成说明。请缩小问题范围后重问。"), nil
		}
		resp, err := c.llm.ChatWithTools(ctx, promptMessages, activeTools, &core.ChatOptions{
			Temperature:   0.7,
			MaxTokens:     4096,
			ReadOnly:      readOnly,
			ReasoningOnly: exhausted,
		})
		if err != nil {
			if ctx.Err() != nil {
				var uncertain *core.UncertainExecutionError
				if errors.As(err, &uncertain) {
					return nil, fmt.Errorf("%w: %w", ctx.Err(), err)
				}
				return nil, ctx.Err()
			}
			var uncertain *core.UncertainExecutionError
			if errors.As(err, &uncertain) {
				return nil, fmt.Errorf("AGY 执行结果不确定: %w", err)
			}
			if reply, ok := fallbackReply(promptMessages); ok {
				if hasToolResult {
					slog.Warn("cortex: 工具返回后模型未能完成回答", "err", err)
				} else {
					slog.Warn("cortex: 图片已生成但模型配文失败，直接交付图片", "err", err)
				}
				return reply, nil
			}
			return nil, fmt.Errorf("cortex reasoning failed: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if resp == nil {
			if reply, ok := fallbackReply(promptMessages); ok {
				if !hasToolResult {
					slog.Warn("cortex: 图片已生成但模型未返回配文，直接交付图片")
				}
				return reply, nil
			}
			return nil, fmt.Errorf("cortex reasoning failed: model returned an empty response")
		}

		if core.OutputLimitReached(resp.FinishReason) {
			return draftReply(c.checkpointTruncatedReply(ctx, event, promptMessages, resp)), nil
		}

		if exhausted && len(resp.ToolCalls) > 0 && hasToolResult {
			reply, _ := fallbackReply(promptMessages)
			return reply, nil
		}
		if len(resp.ToolCalls) > maxToolCallsPerThink {
			if reply, ok := fallbackReply(promptMessages); ok {
				if !hasToolResult {
					slog.Warn("cortex: 图片已生成但模型超出工具调用上限，直接交付图片", "calls", len(resp.ToolCalls))
				}
				return reply, nil
			}
			return nil, fmt.Errorf("cortex reasoning failed: model returned %d tool calls; limit is %d", len(resp.ToolCalls), maxToolCallsPerThink)
		}
		if err := validateToolCalls(resp.ToolCalls); err != nil {
			if reply, ok := fallbackReply(promptMessages); ok {
				if !hasToolResult {
					slog.Warn("cortex: 图片已生成但模型工具响应无效，直接交付图片", "err", err)
				}
				return reply, nil
			}
			return nil, fmt.Errorf("cortex reasoning failed: invalid model tool calls: %w", err)
		}

		// 若未触发任何 Tool 调用，组装文本与（可能存在的）图像一并发送
		if len(resp.ToolCalls) == 0 {
			cleanText := strings.TrimSpace(resp.Content)
			if cleanText == "" && hasToolResult && !media.hasContent(presentation) {
				reply, _ := fallbackReply(promptMessages)
				return reply, nil
			}
			if cleanText == "" && !media.hasContent(presentation) {
				return nil, fmt.Errorf("cortex reasoning failed: model returned neither text nor media")
			}
			return media.utterance(event, cleanText), nil
		}

		messages = append(messages, core.ChatMessage{
			Role:      core.RoleAssistant,
			Content:   core.TruncateRunes(resp.Content, maxAssistantReplyRunes, "\n[模型说明过长，后续内容已截断]"),
			ToolCalls: toolCallsForTranscript(resp.ToolCalls),
		})

		batch, err := c.executeToolBatch(toolCtx, activeTools, resp.ToolCalls, toolCallsUsed, seenToolCalls, &media, presentation)
		if err != nil {
			if batch.hasToolResult {
				for _, call := range resp.ToolCalls[len(batch.messages):] {
					batch.appendToolMessage(call, "本轮在此调用取得可确认结果前中断。")
				}
				checkpoint = append(messages, batch.messages...)
			}
			return nil, err
		}
		messages = append(messages, batch.messages...)
		toolCallsUsed = batch.toolCallsUsed
		hasToolResult = hasToolResult || batch.hasToolResult
		needsSynthesis = needsSynthesis || batch.needsSynthesis
		if hasToolResult {
			checkpoint = messages
		}
		// ImageOnly tools already own the final presentation. Asking the model
		// to present their result again can render the same document twice.
		if batch.preparedViews == batch.uniqueToolCalls && !needsSynthesis {
			return media.utterance(event, ""), nil
		}
		if batch.directReply != "" {
			return media.utterance(event, batch.directReply), nil
		}

	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	return c.finishConversation(ctx, event, input, readOnly, hasToolResult, messages, &media)
}
