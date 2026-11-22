package entity

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/core"
)

const (
	automaticCardPageRunes = 1400
	automaticCardTimeout   = 45 * time.Second
)

type automaticCardRenderer interface {
	ShouldRenderCard(context.Context, string) bool
	RenderCardPages(context.Context, *SensoryEvent, string, int) ([][]byte, error)
}

type inlineAssetCardRenderer interface {
	RenderCardPagesWithInlineAssets(context.Context, *SensoryEvent, string, int, []core.InlineAsset) ([][]byte, error)
}

func (a *SuperAgent) renderAutomaticReplyCard(ctx context.Context, event *SensoryEvent, utterance *Utterance) {
	if event == nil || utterance == nil ||
		(event.Platform != "qq" && event.Platform != "telegram") || event.GroupInterjection ||
		utterance.FixedPresentation || utterance.SuppressAutoCard || utterance.InlineImages ||
		strings.TrimSpace(utterance.Text) == "" {
		return
	}
	renderer, ok := a.Cognition.(automaticCardRenderer)
	if !ok {
		return
	}

	replyContent := utterance.Text
	if !event.IsPartnerPrivate() {
		replyContent = core.RedactSensitiveIPs(replyContent)
	}
	if !renderer.ShouldRenderCard(ctx, replyContent) {
		return
	}

	images := make([][]byte, 0, len(utterance.Images)+1)
	for _, image := range utterance.Images {
		if len(image) > 0 {
			images = append(images, image)
		}
	}
	if len(utterance.CardData) > 0 {
		images = append(images, utterance.CardData)
	}
	maxPages := len(core.SplitMarkdownChunks(replyContent, automaticCardPageRunes))
	if maxPages == 0 {
		maxPages = 1
	}
	cardCtx, cancel := context.WithTimeout(ctx, automaticCardTimeout)
	var pages [][]byte
	var renderErr error
	if len(utterance.InlineAssets) > 0 {
		assetRenderer, ok := renderer.(inlineAssetCardRenderer)
		if !ok {
			cancel()
			slog.Warn("superagent: 长回复含内嵌图片，但卡片渲染器不支持内嵌资源", "platform", event.Platform, "channel", event.ChannelID)
			return
		}
		pages, renderErr = assetRenderer.RenderCardPagesWithInlineAssets(cardCtx, event, replyContent, maxPages, utterance.InlineAssets)
	} else {
		pages, renderErr = renderer.RenderCardPages(cardCtx, event, replyContent, maxPages)
	}
	cancel()
	if renderErr != nil || len(pages) == 0 {
		slog.Warn("superagent: 长回复卡片渲染失败，回退为文字", "platform", event.Platform, "channel", event.ChannelID, "err", renderErr)
		return
	}

	utterance.InlineNonTextImageCount = len(images)
	for len(utterance.ImageSources) < len(images) {
		utterance.ImageSources = append(utterance.ImageSources, "")
	}
	historyContent := replyContent
	fallbackContent := replyContent
	for _, asset := range utterance.InlineAssets {
		fallback := asset.Fallback
		if fallback == "" {
			fallback = "[流程图]"
		}
		historyContent = strings.Replace(historyContent, asset.Placeholder, fallback+"\n"+asset.SemanticText, 1)
		fallbackContent = strings.Replace(fallbackContent, asset.Placeholder, fallback, 1)
	}
	for range pages {
		utterance.ImageSources = append(utterance.ImageSources, historyContent)
	}
	images = append(images, pages...)
	utterance.FallbackText = strings.TrimSpace(strings.Join(nonEmptyStrings([]string{
		strings.TrimSpace(utterance.FallbackText), fallbackContent,
	}), "\n\n"))
	utterance.Text = ""
	utterance.CardData = nil
	utterance.Images = images
	utterance.InlineImages = true
	utterance.InlineAssets = nil
	slog.Info("superagent: 长回复已分页渲染为图文消息", "platform", event.Platform, "channel", event.ChannelID, "pages", len(pages), "images", len(images), "textBytes", len(replyContent))
}
