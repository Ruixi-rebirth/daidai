package chat

import (
	"strings"

	"daidai/internal/entity"
)

// Only text cards with complete source documents can be delivered as text.
// Never replace an actual photograph or generated image with a guessed caption.
func useCardSourceText(out *entity.Utterance) {
	for _, asset := range out.InlineAssets {
		plain := asset.SemanticText
		if plain == "" {
			plain = asset.Fallback
		}
		out.Text = strings.Replace(out.Text, asset.Placeholder, plain, 1)
	}
	out.InlineAssets = nil
	out.QuoteText = ""
	media := out.WithoutEmptyImages()
	var sources []string
	var remaining [][]byte
	if len(media.Images) == 0 {
		if len(media.CardData) == 0 || strings.TrimSpace(media.FallbackText) == "" {
			return
		}
		sources = append(sources, media.FallbackText)
	} else {
		for index, image := range media.Images {
			if index < len(media.ImageSources) && strings.TrimSpace(media.ImageSources[index]) != "" {
				sources = append(sources, media.ImageSources[index])
			} else {
				remaining = append(remaining, image)
			}
		}
		if len(sources) == 0 {
			return
		}
	}
	parts := append(sources, out.Text)
	out.Text, _ = entity.ImageDeliveryText(parts, len(parts))
	out.CardData = nil
	out.Images, out.ImageSources = remaining, nil
	out.FallbackText = ""
	out.InlineImages = media.InlineImages && len(remaining) > 0
	out.InlineNonTextImageCount = len(remaining)
}
