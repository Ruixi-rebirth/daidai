package qq

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
)

func (q *QQChannel) transmitInlineAssets(ctx context.Context, utterance *entity.Utterance) (sendErr error) {
	content := utterance.Text
	visibleContent := utterance.Text
	quoteText := utterance.Text
	imageIDs := make([]string, 0, len(utterance.InlineAssets))
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, imageID := range imageIDs {
			if err := q.client.DeleteMarkdownImage(cleanupCtx, imageID); err != nil {
				slog.Warn("qq_channel: failed to release temporary Mermaid image material", "imageID", imageID, "err", err)
			}
		}
	}()
	for index, asset := range utterance.InlineAssets {
		fallback := asset.Fallback
		if fallback == "" {
			fallback = "[流程图]"
		}
		if len(asset.Data) == 0 || len(asset.Data) > core.MaxImageBytes {
			content = strings.Replace(content, asset.Placeholder, fallback, 1)
			visibleContent = strings.Replace(visibleContent, asset.Placeholder, fallback, 1)
			quoteText = strings.Replace(quoteText, asset.Placeholder, fallback+"\n"+asset.SemanticText, 1)
			continue
		}
		dimensions, err := png.DecodeConfig(bytes.NewReader(asset.Data))
		if err != nil {
			content = strings.Replace(content, asset.Placeholder, fallback, 1)
			visibleContent = strings.Replace(visibleContent, asset.Placeholder, fallback, 1)
			quoteText = strings.Replace(quoteText, asset.Placeholder, fallback+"\n"+asset.SemanticText, 1)
			slog.Warn("qq_channel: Mermaid PNG invalid; preserving text fallback", "diagram", index+1, "err", err)
			continue
		}
		imageID, imageURL, err := q.client.UploadMarkdownImage(ctx, asset.Data)
		if err != nil {
			content = strings.Replace(content, asset.Placeholder, fallback, 1)
			visibleContent = strings.Replace(visibleContent, asset.Placeholder, fallback, 1)
			quoteText = strings.Replace(quoteText, asset.Placeholder, fallback+"\n"+asset.SemanticText, 1)
			slog.Warn("qq_channel: Mermaid image upload failed; preserving text fallback", "diagram", index+1, "err", err)
			continue
		}
		imageIDs = append(imageIDs, imageID)
		markup := fmt.Sprintf("![%s #%dpx #%dpx](%s)", asset.Alt, dimensions.Width, dimensions.Height, imageURL)
		if !strings.Contains(content, asset.Placeholder) {
			content += "\n\n" + markup
			visibleContent += "\n\n" + fallback
			quoteText += "\n\n" + fallback + "\n" + asset.SemanticText
		} else {
			content = strings.Replace(content, asset.Placeholder, markup, 1)
			visibleContent = strings.Replace(visibleContent, asset.Placeholder, fallback, 1)
			quoteText = strings.Replace(quoteText, asset.Placeholder, fallback+"\n"+asset.SemanticText, 1)
		}
	}
	utterance.Text = visibleContent
	utterance.FallbackText = visibleContent
	utterance.QuoteText = quoteText
	plain := *utterance
	plain.Text = content
	plain.InlineAssets = nil
	plain.Images = nil
	plain.CardData = nil
	plain.ImageSources = nil
	plain.InlineImages = false
	plain.FallbackText = ""
	plain.QuoteText = quoteText
	if err := q.Transmit(ctx, &plain); err != nil {
		return fmt.Errorf("send QQ reply with inline Mermaid images: %w", err)
	}
	return nil
}
