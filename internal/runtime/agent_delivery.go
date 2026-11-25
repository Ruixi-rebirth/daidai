package runtime

import (
	"context"
	"daidai/internal/entity"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"daidai/internal/core"
)

func (a *SuperAgent) transmitReplyText(ctx context.Context, event *entity.SensoryEvent, text string) error {
	_, err := a.transmitReplyTextDelivered(ctx, event, text)
	return err
}

func (a *SuperAgent) transmitReplyTextDelivered(ctx context.Context, event *entity.SensoryEvent, text string) (string, error) {
	if a.Channels == nil {
		err := fmt.Errorf("channel registry is not configured")
		slog.Warn("superagent: 文本回复发送失败", "platform", event.Platform, "channel", event.ChannelID, "err", err)
		return "", err
	}
	if !event.IsPartnerPrivate() {
		text = core.RedactSensitiveIPs(text)
	}
	delivered, err := a.transmitUtterance(ctx, &entity.Utterance{
		Platform:     event.Platform,
		IsGroup:      event.IsGroup,
		ChannelID:    event.ChannelID,
		QuoteMsgID:   event.RawMessageID,
		TargetUserID: event.SenderID,
		Text:         text,
	}, event.SenderID)
	if err != nil {
		slog.Warn("superagent: 文本回复发送失败", "platform", event.Platform, "channel", event.ChannelID, "err", err)
	}
	return delivered, err
}

func addressReply(utterance *entity.Utterance, event *entity.SensoryEvent) {
	if utterance.TargetUserID == "" {
		utterance.TargetUserID = event.SenderID
	}
	if event.Platform == "qq" && event.IsGroup && !event.GroupInterjection && len(utterance.MentionUserIDs) == 0 && event.SenderID != "" {
		utterance.MentionUserIDs = []string{event.SenderID}
	}
}

func (a *SuperAgent) transmitUtterance(ctx context.Context, utterance *entity.Utterance, senderID string) (string, error) {
	return a.transmitUtteranceWithContinuationContext(ctx, ctx, utterance, senderID)
}

func (a *SuperAgent) transmitUtteranceWithContinuationContext(ctx, continuationCtx context.Context, utterance *entity.Utterance, senderID string) (string, error) {
	if utterance == nil {
		return "", nil
	}
	if a.Channels == nil {
		return "", fmt.Errorf("channel registry is not configured")
	}
	utterance = utterance.WithoutEmptyImages()
	var partialErr *entity.PartialDeliveryError
	if err := a.Channels.Transmit(ctx, utterance); err == nil {
		return deliveredReplyText(utterance), nil
	} else if errors.As(err, &partialErr) {
		slog.Warn("superagent: 消息部分送达，不重复发送正文", "channel", utterance.ChannelID, "err", err)
		if !utterance.FixedPresentation {
			a.setReplyContinuation(continuationCtx, senderID, utterance, partialErr.UndeliveredText)
		}
		return partialErr.DeliveredText, nil
	} else if ctx.Err() != nil {
		return "", ctx.Err()
	} else if errors.Is(err, entity.ErrMessageTooLong) {
		fallbackText := textForFallback(utterance)
		if fallbackText == "" {
			fallbackText = "回复内容超过平台单次发送上限。"
		}
		return a.sendTextFallback(ctx, continuationCtx, utterance, fallbackText, senderID)
	} else {
		if utterance.FixedPresentation {
			var unsent *entity.UnsentError
			if !errors.As(err, &unsent) {
				return "", err
			}
		}
		if !utterance.HasMedia() {
			a.setReplyContinuation(continuationCtx, senderID, utterance, utterance.Text)
			return "", err
		}
		if !canFallbackToText(err) {
			return "", err
		}
		slog.Warn("superagent: 媒体消息发送失败，回退为文字", "channel", utterance.ChannelID, "err", err)
		fallbackText := textForFallback(utterance)
		if fallbackText == "" {
			fallbackText = "图片暂时没发出去。"
			if len(utterance.Videos) > 0 && len(utterance.Images) == 0 && len(utterance.CardData) == 0 {
				fallbackText = "视频暂时没发出去。"
			}
		}
		deliveredText, fallbackErr := a.sendTextFallback(ctx, continuationCtx, utterance, fallbackText, senderID)
		if fallbackErr != nil {
			return "", fmt.Errorf("send image failed: %v; text fallback failed: %w", err, fallbackErr)
		}
		return deliveredText, nil
	}
}

func deliveredReplyText(u *entity.Utterance) string {
	text := u.QuoteText
	if text == "" && u.HasMedia() && len(u.ImageSources) > 0 {
		sources := append(append([]string(nil), u.ImageSources...), u.Text)
		text, _ = entity.ImageDeliveryText(sources, len(sources))
	}
	if text == "" {
		text = u.Text
	}
	if text == "" && u.HasMedia() {
		text = u.FallbackText
	}
	if u.HasMedia() && u.MediaHistory != "" {
		return strings.TrimSpace(u.MediaHistory + "\n\n" + text)
	}
	return text
}

func canFallbackToText(err error) bool {
	var unsent *entity.UnsentError
	var rejection *entity.DeliveryRejection
	return errors.As(err, &unsent) || (errors.As(err, &rejection) && rejection.After <= 0)
}

// textForFallback keeps the media's text equivalent and accompanying reply.
func textForFallback(utterance *entity.Utterance) string {
	body := strings.TrimSpace(utterance.FallbackText)
	if body == "" {
		body, _ = entity.ImageDeliveryText(utterance.ImageSources, len(utterance.ImageSources))
	}
	caption := strings.TrimSpace(core.InlineAssetText(utterance.Text, utterance.InlineAssets, false))
	if body == "" {
		return caption
	}
	if caption == "" || body == caption || strings.HasSuffix(body, "\n\n"+caption) {
		return body
	}
	return body + "\n\n" + caption
}

func (a *SuperAgent) transmitTextFallback(ctx context.Context, utterance *entity.Utterance, text, senderID string) (string, error) {
	return a.sendTextFallback(ctx, ctx, utterance, text, senderID)
}

func (a *SuperAgent) sendTextFallback(ctx, continuationCtx context.Context, utterance *entity.Utterance, text, senderID string) (string, error) {
	delivered, err := a.transmitTextFallbackWithContinuationContext(ctx, continuationCtx, utterance, text, senderID)
	if err != nil && (ctx == nil || ctx.Err() == nil) {
		a.setReplyContinuation(continuationCtx, senderID, utterance, text)
	}
	return delivered, err
}

func (a *SuperAgent) transmitTextFallbackWithContinuationContext(ctx, continuationCtx context.Context, utterance *entity.Utterance, text, senderID string) (string, error) {
	if a.Channels == nil {
		return "", fmt.Errorf("channel registry is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if continuationCtx == nil {
		continuationCtx = ctx
	}
	fallback := *utterance
	fallback.FallbackText = ""
	fallback.CardData = nil
	fallback.Images = nil
	fallback.Videos = nil
	fallback.InlineAssets = nil
	fallback.QuoteText = ""
	fallback.ReplyCardPages = 0
	fallback.ImageSources = nil
	fallback.InlineImages = false
	fallback.InlineNonTextImageCount = 0
	chunks := core.SplitMarkdownChunks(text, 1100)
	if len(chunks) <= 5 {
		fallback.Text = text
		if err := a.Channels.Transmit(ctx, &fallback); err != nil {
			var partialErr *entity.PartialDeliveryError
			if errors.As(err, &partialErr) {
				if continuationCtx.Err() == nil && strings.TrimSpace(partialErr.UndeliveredText) != "" {
					a.setReplyContinuation(continuationCtx, senderID, utterance, partialErr.UndeliveredText)
				}
				return partialErr.DeliveredText, nil
			}
			return "", err
		}
		return fallback.Text, nil
	}

	remaining := strings.Join(chunks[5:], "\n\n")
	fallback.OnDelivered = nil
	command := "/continue"
	if utterance.DraftID != "" {
		command += " " + utterance.DraftID
	}
	marker := "[已达到平台单次发送上限，后续内容未发送；发送“" + command + "”可接着看。]"
	page := append([]string(nil), chunks[:5]...)
	if fallback.Platform == "qq" {
		fallback.HasMoreText = true
	} else {
		page[len(page)-1] += "\n" + marker
	}
	fallback.Text = strings.Join(page, "\n\n")
	if err := a.Channels.Transmit(ctx, &fallback); err != nil {
		var partialErr *entity.PartialDeliveryError
		if errors.As(err, &partialErr) {
			// The pagination hint is transport metadata, not reply content.
			pending := strings.TrimSuffix(partialErr.UndeliveredText, marker)
			if remaining != "" {
				pending = strings.TrimSpace(pending + "\n\n" + remaining)
			}
			if continuationCtx.Err() == nil && strings.TrimSpace(pending) != "" {
				a.setReplyContinuation(continuationCtx, senderID, utterance, pending)
			}
			return strings.TrimSpace(strings.TrimSuffix(partialErr.DeliveredText, marker)), nil
		}
		return "", err
	}
	a.setReplyContinuation(continuationCtx, senderID, utterance, remaining)
	return strings.Join(chunks[:5], "\n\n"), nil
}

func (a *SuperAgent) replyText(ctx context.Context, event *entity.SensoryEvent, text string) {
	_ = a.transmitReplyText(ctx, event, text)
}
