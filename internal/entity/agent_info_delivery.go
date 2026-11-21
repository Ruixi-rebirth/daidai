package entity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/core"
)

func (a *SuperAgent) transmitInfo(ctx context.Context, event *SensoryEvent, caption, content string, preferCard bool) error {
	if event == nil {
		return fmt.Errorf("cannot send information without an event")
	}
	if a.Channels == nil {
		return fmt.Errorf("channel registry is not configured")
	}
	if !event.IsPartnerPrivate() {
		content = core.RedactSensitiveIPs(content)
	}
	renderer, ok := a.Cognition.(interface {
		RenderCard(context.Context, *SensoryEvent, string) ([]byte, error)
	})
	if !ok {
		return a.transmitReplyText(ctx, event, content)
	}
	if policy, ok := a.Cognition.(interface {
		ShouldRenderCard(context.Context, string) bool
	}); ok && !preferCard && !policy.ShouldRenderCard(ctx, content) {
		return a.transmitReplyText(ctx, event, content)
	}
	cardCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	card, err := renderer.RenderCard(cardCtx, event, content)
	if err != nil || len(card) == 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("superagent: 信息卡片渲染失败，回退为文字", "platform", event.Platform, "channel", event.ChannelID, "err", err)
		return a.transmitReplyText(ctx, event, content)
	}
	cardUtterance := &Utterance{
		Platform:     event.Platform,
		IsGroup:      event.IsGroup,
		ChannelID:    event.ChannelID,
		QuoteMsgID:   event.RawMessageID,
		TargetUserID: event.SenderID,
		Text:         caption,
		FallbackText: content,
		CardData:     card,
	}
	if _, err := a.transmitUtterance(ctx, cardUtterance, event.SenderID); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("superagent: 信息卡片发送失败", "platform", event.Platform, "channel", event.ChannelID, "err", err)
		return err
	}
	return nil
}

// transmitPendingNotification sends a durable notification without moving
// unsent content into the process-local conversational continuation buffer.
func (a *SuperAgent) transmitPendingNotification(ctx context.Context, event *SensoryEvent, caption, content string) (bool, string, error) {
	if event == nil || a.Channels == nil {
		return false, "", fmt.Errorf("notification delivery is not configured")
	}
	if !event.IsPartnerPrivate() {
		content = core.RedactSensitiveIPs(content)
	}
	const maxNotificationParts = 3
	notificationPartLimit := maxNotificationParts
	if renderer, ok := a.Cognition.(interface {
		RenderCard(context.Context, *SensoryEvent, string) ([]byte, error)
	}); ok {
		shouldRender := true
		if policy, ok := a.Cognition.(interface {
			ShouldRenderCard(context.Context, string) bool
		}); ok {
			shouldRender = policy.ShouldRenderCard(ctx, content)
		}
		if shouldRender {
			cardCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			card, err := renderer.RenderCard(cardCtx, event, content)
			cancel()
			if err == nil && len(card) > 0 {
				cardUtterance := &Utterance{
					Platform:     event.Platform,
					IsGroup:      event.IsGroup,
					ChannelID:    event.ChannelID,
					QuoteMsgID:   event.RawMessageID,
					Text:         caption,
					FallbackText: content,
					CardData:     card,
				}
				if sendErr := a.Channels.Transmit(ctx, cardUtterance); sendErr == nil {
					return true, "", nil
				} else {
					var partial *PartialDeliveryError
					if errors.As(sendErr, &partial) {
						pending := strings.TrimSpace(partial.UndeliveredText)
						return pending == "", pending, nil
					}
					if ctx.Err() != nil {
						return false, "", ctx.Err()
					}
					if !canFallbackToText(sendErr) {
						return false, "", sendErr
					}
					// QQ reserves a passive-reply attempt before sending. A failed
					// media attempt still consumes one slot, so reduce the text
					// fallback by one part and preserve a slot for the user's reply.
					notificationPartLimit--
					slog.Warn("superagent: 待发送通知卡片失败，改发文本", "notificationChannel", event.ChannelID, "err", sendErr)
				}
			} else if err != nil {
				if ctx.Err() != nil {
					return false, "", ctx.Err()
				}
				slog.Warn("superagent: 待发送通知卡片渲染失败，改发文本", "notificationChannel", event.ChannelID, "err", err)
			}
		}
	}

	chunks := core.SplitMarkdownChunks(content, 1100)
	if len(chunks) == 0 {
		return true, "", nil
	}
	var remaining string
	// QQ allows four passive replies to one private message. Keep one slot for
	// the answer to the message that triggered this notification delivery.
	if len(chunks) > notificationPartLimit {
		contentParts := notificationPartLimit - 1
		remaining = strings.Join(chunks[contentParts:], "\n\n")
		chunks = append(append([]string(nil), chunks[:contentParts]...), "📨 报告还有剩余内容，将在你下次私聊时继续送达。")
	}
	textUtterance := &Utterance{
		Platform:   event.Platform,
		IsGroup:    event.IsGroup,
		ChannelID:  event.ChannelID,
		QuoteMsgID: event.RawMessageID,
		Text:       strings.Join(chunks, "\n\n"),
	}
	if err := a.Channels.Transmit(ctx, textUtterance); err != nil {
		var partialErr *PartialDeliveryError
		if !errors.As(err, &partialErr) {
			return false, "", err
		}
		pending := strings.TrimSpace(partialErr.UndeliveredText)
		if remaining != "" {
			pending = strings.TrimSuffix(pending, "📨 报告还有剩余内容，将在你下次私聊时继续送达。")
			pending = strings.TrimSpace(pending)
			if pending != "" {
				pending += "\n\n"
			}
			pending += remaining
		}
		if pending == "" {
			return true, "", nil
		}
		return false, pending, nil
	}
	return remaining == "", remaining, nil
}

func (a *SuperAgent) replyInfo(ctx context.Context, event *SensoryEvent, caption, content string) {
	if err := a.transmitInfo(ctx, event, caption, content, false); err != nil {
		slog.Warn("superagent: 信息发送失败", "platform", event.Platform, "channel", event.ChannelID, "err", err)
	}
}
