package telegram

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"daidai/internal/core"
	"daidai/internal/entity"
)

type inlineReplyPart struct {
	text   formattedText
	source string
	asset  *core.InlineAsset
}

func (c *Channel) transmitInlineAssets(ctx context.Context, u *entity.Utterance, reply int64) (string, error) {
	var parts []inlineReplyPart
	addText := func(markdown string) {
		for _, chunk := range splitFormatted(formatMarkdown(markdown), 1800) {
			if source := chunk.continuationText(); strings.TrimSpace(source) != "" {
				parts = append(parts, inlineReplyPart{text: chunk, source: source})
			}
		}
	}
	text := u.Text
	for i := range u.InlineAssets {
		asset := &u.InlineAssets[i]
		before, after, found := strings.Cut(text, asset.Placeholder)
		if !found {
			continue
		}
		addText(before)
		parts = append(parts, inlineReplyPart{source: asset.Fallback, asset: asset})
		text = after
	}
	addText(text)

	var delivered []string
	for i, part := range parts {
		source, err := c.sendInlineReplyPart(ctx, u, reply, part)
		if err == nil {
			delivered = append(delivered, source)
			continue
		}
		var receiptErr *entity.PartialDeliveryError
		if i == 0 && !errors.As(err, &receiptErr) {
			return "", err
		}
		next := i
		if part.asset == nil && errors.As(err, &receiptErr) {
			// Telegram accepted the text but did not return its message ID.
			next++
		}
		var remaining []string
		for _, pending := range parts[next:] {
			remaining = append(remaining, pending.source)
		}
		return "", &entity.PartialDeliveryError{
			Err: err, DeliveredText: joinDeliveryText(delivered...), UndeliveredText: joinDeliveryText(remaining...),
		}
	}
	return joinDeliveryText(delivered...), nil
}

func (c *Channel) sendInlineReplyPart(ctx context.Context, u *entity.Utterance, reply int64, part inlineReplyPart) (string, error) {
	if part.asset == nil {
		return part.source, c.sendFormattedText(ctx, u.ChannelID, part.text, reply, part.source)
	}
	asset := part.asset
	results, err := c.sendImages(ctx, u.ChannelID, reply, [][]byte{asset.Data}, asset.Alt)
	if err != nil {
		var unsent *entity.UnsentError
		var rejected *entity.DeliveryRejection
		if errors.As(err, &unsent) || (errors.As(err, &rejected) && rejected.After <= 0) {
			text := "[流程图未能发送]"
			return text, c.sendText(ctx, u.ChannelID, text, reply, text)
		}
		return "", err
	}
	source := strings.TrimSpace(asset.Fallback + "\n" + asset.SemanticText)
	if err := c.storeQuote(ctx, u.ChannelID, results[0].ID, &quoteRecord{
		Content: source, Kind: "text_card", Complete: true, SenderName: c.bot.FirstName,
	}); err != nil {
		slog.Warn("telegram: diagram quote persistence failed", "messageID", results[0].ID, "err", err)
	}
	return source, nil
}
