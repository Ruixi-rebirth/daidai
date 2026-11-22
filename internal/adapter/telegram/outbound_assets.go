package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"daidai/internal/core"
	"daidai/internal/entity"
)

func (c *Channel) transmitInlineAssets(ctx context.Context, utterance *entity.Utterance, reply int64) error {
	content := utterance.Text
	visibleContent := utterance.Text
	quoteText := utterance.Text
	for _, asset := range utterance.InlineAssets {
		fallback := asset.Fallback
		if fallback == "" {
			fallback = "[流程图]"
		}
		if strings.Contains(visibleContent, asset.Placeholder) {
			visibleContent = strings.Replace(visibleContent, asset.Placeholder, fallback, 1)
		} else {
			visibleContent += "\n\n" + fallback
		}
		semantic := fallback
		if asset.SemanticText != "" {
			semantic += "\n" + asset.SemanticText
		}
		if strings.Contains(quoteText, asset.Placeholder) {
			quoteText = strings.Replace(quoteText, asset.Placeholder, semantic, 1)
		} else {
			quoteText += "\n\n" + semantic
		}
	}
	utterance.Text = visibleContent
	utterance.FallbackText = visibleContent
	utterance.QuoteText = quoteText
	sent := false
	delivered := ""
	sendText := func(source string) error {
		chunks := splitFormatted(formatMarkdown(source), 1800)
		for _, chunk := range chunks {
			text := chunk.continuationText()
			if strings.TrimSpace(text) == "" {
				continue
			}
			if err := c.sendFormattedText(ctx, utterance.ChannelID, chunk, reply, text); err != nil {
				if sent {
					return &entity.PartialDeliveryError{Err: err, DeliveredText: delivered, UndeliveredText: source}
				}
				return err
			}
			sent = true
			delivered = joinDeliveryText(delivered, text)
		}
		return nil
	}
	for _, asset := range utterance.InlineAssets {
		position := strings.Index(content, asset.Placeholder)
		if position < 0 {
			position = len(content)
			content += asset.Placeholder
		}
		before := content[:position]
		if err := sendText(before); err != nil {
			return err
		}
		content = content[position+len(asset.Placeholder):]
		if len(asset.Data) == 0 || len(asset.Data) > core.MaxImageBytes {
			fallback := asset.Fallback
			if fallback == "" {
				fallback = "[流程图无法发送]"
			}
			content = fallback + content
			continue
		}
		caption := asset.Alt
		if caption == "" {
			caption = "流程图"
		}
		results, err := c.sendImages(ctx, utterance.ChannelID, reply, [][]byte{asset.Data}, caption)
		if err != nil {
			fallback := asset.Fallback
			if fallback == "" {
				fallback = "[流程图暂时无法发送]"
			}
			if sent {
				return &entity.PartialDeliveryError{Err: err, DeliveredText: delivered, UndeliveredText: fallback + content}
			}
			return err
		}
		sent = true
		if len(results) > 0 {
			for _, result := range results {
				source := asset.SemanticText
				if source == "" {
					source = "[流程图]"
				} else {
					source = "[流程图]\n" + source
				}
				if err := c.storeQuote(ctx, utterance.ChannelID, result.ID, &quoteRecord{
					Content: source, Kind: "text_card", Complete: true, SenderName: c.bot.FirstName,
				}); err != nil {
					// Delivery succeeded; history enrichment must not turn it into a failed send.
					slog.Warn("telegram: Mermaid quote history persistence failed", "messageID", result.ID, "err", err)
				}
			}
		}
		delivered = joinDeliveryText(delivered, asset.Fallback)
	}
	if err := sendText(content); err != nil {
		return err
	}
	if !sent && strings.TrimSpace(content) == "" {
		return fmt.Errorf("inline diagram reply had no deliverable content")
	}
	return nil
}
