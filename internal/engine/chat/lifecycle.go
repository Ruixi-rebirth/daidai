package chat

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"daidai/internal/core"
	"daidai/internal/engine/render"
	"daidai/internal/entity"
	"daidai/internal/media"
)

// Think 对感知事件进行认知推理并一次性合成全息生命表达体 (Utterance)
func (c *Cortex) Think(ctx context.Context, event *entity.SensoryEvent) (out *entity.Utterance, thinkErr error) {
	presentation := &thinkPresentation{}

	defer func() {
		var processed []*core.Attachment
		if event != nil {
			processed = event.ProcessedAttachments
			event.ProcessedAttachments = nil
		}
		if out == nil || thinkErr != nil {
			return
		}
		if out.Text != "" && (event.Platform == "qq" || event.Platform == "telegram") {
			rendered, assets, err := render.RenderMermaidBlocks(ctx, out.Text)
			if err != nil {
				slog.Warn("cortex: Mermaid 流程图未能渲染", "channel", event.ChannelID, "err", err)
			}
			out.Text = rendered
			out.InlineAssets = append(out.InlineAssets, assets...)
		}
		var textOnly bool
		needsPresentationDecision := out.HasMedia() || c.ShouldRenderCard(ctx, out.Text)
		if needsPresentationDecision && (event.Platform == "qq" || event.Platform == "telegram") && !event.GroupInterjection && hasReplyFormatTerms(event.Content) {
			textOnly = c.detectTextOnlyIntent(ctx, event.Content)
		}
		if textOnly {
			out.SuppressAutoCard = true
			slog.Info("cortex: 识别到用户要求纯文本表达，跳过卡片出图", "channel", event.ChannelID)
		}
		if presentation.toolFixedPresentation {
			out.FixedPresentation = true
			slog.Info("cortex: 工具结果指定固定展示形式，跳过自动排版", "channel", event.ChannelID)
		}
		if presentation.mediaCaption != "" {
			out.Text = strings.TrimSpace(presentation.mediaCaption + "\n\n" + out.Text)
			out.FixedPresentation = true
		}
		out.InlineImages = out.InlineImages || presentation.mediaInline
		if textOnly {
			useCardSourceText(out)
		}
		if len(presentation.mediaFailures) > 0 {
			out.OnDeliveryFailed = func(ctx context.Context, err error) {
				for _, failed := range presentation.mediaFailures {
					failed(ctx, err)
				}
			}
		}
		if c.inbox != nil && len(processed) > 0 && (event.Platform == "qq" || event.Platform == "telegram") {
			scope := media.AttachmentScope(event.Platform, event.IsGroup, event.ChannelID, event.SenderID)
			presentation.mediaCommits = append(presentation.mediaCommits, func(context.Context) {
				c.inbox.ForgetPending(scope, processed)
			})
		}
		if len(presentation.mediaCommits) > 0 {
			onDelivered := out.OnDelivered
			out.OnDelivered = func(ctx context.Context) {
				if onDelivered != nil {
					onDelivered(ctx)
				}
				for _, commit := range presentation.mediaCommits {
					commit(ctx)
				}
			}
		}
	}()
	if event == nil {
		return nil, fmt.Errorf("cortex cannot process a nil sensory event")
	}
	if c.llm == nil {
		return nil, fmt.Errorf("cortex has no language model provider")
	}
	if event.GroupInterjection {
		return c.thinkGroupInterjection(ctx, event)
	}
	if event.GroupContinuation {
		if !c.addressedGroupContinuation(ctx, event) {
			return nil, nil
		}
		event.GroupMessageObserved = false
	}
	if prepared := c.prepareAttachments(ctx, event); prepared != nil {
		return prepared, nil
	}
	input, inputErr := c.buildChatInput(ctx, event)
	if inputErr != nil {
		return nil, inputErr
	}
	return c.runConversation(ctx, event, input, presentation)
}

// RecordInbound persists accepted user input before debounce can merge messages
// or later processing and delivery can fail.
func (c *Cortex) RecordInbound(ctx context.Context, event *entity.SensoryEvent) error {
	if c == nil || c.memory == nil || event == nil {
		return nil
	}
	recorder, ok := c.memory.(interface {
		RecordInbound(context.Context, *entity.SensoryEvent) error
	})
	if !ok {
		return nil
	}
	return recorder.RecordInbound(ctx, event)
}

// CommitDeliveredReply 在发送层确认结果后持久化本轮消息，避免将未送达或未脱敏的回复写入会话历史。
func (c *Cortex) CommitDeliveredReply(ctx context.Context, event *entity.SensoryEvent, replyText string) {
	if c.memory != nil {
		c.memory.Commit(ctx, event, replyText)
	}
}

// ClearPendingAttachments forgets unprocessed QQ attachment metadata when a
// conversation is explicitly reset.
func (c *Cortex) ClearPendingAttachments(event *entity.SensoryEvent) {
	if c == nil || c.inbox == nil || event == nil || (event.Platform != "qq" && event.Platform != "telegram") {
		return
	}
	c.inbox.ForgetConversationPending(event.Platform, event.IsGroup, event.ChannelID)
}
