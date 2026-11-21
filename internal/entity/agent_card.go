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
	ShouldAutoRenderReply(string) bool
	RenderCardPages(context.Context, *SensoryEvent, string, int) ([][]byte, error)
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
	if !renderer.ShouldAutoRenderReply(replyContent) {
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
	pages, renderErr := renderer.RenderCardPages(cardCtx, event, replyContent, maxPages)
	cancel()
	if renderErr != nil || len(pages) == 0 {
		slog.Warn("superagent: 长回复卡片渲染失败，回退为文字", "platform", event.Platform, "channel", event.ChannelID, "err", renderErr)
		return
	}

	utterance.InlineNonTextImageCount = len(images)
	for len(utterance.ImageSources) < len(images) {
		utterance.ImageSources = append(utterance.ImageSources, "")
	}
	for range pages {
		utterance.ImageSources = append(utterance.ImageSources, replyContent)
	}
	images = append(images, pages...)
	utterance.FallbackText = strings.TrimSpace(strings.Join(nonEmptyStrings([]string{
		strings.TrimSpace(utterance.FallbackText), replyContent,
	}), "\n\n"))
	utterance.Text = ""
	utterance.CardData = nil
	utterance.Images = images
	utterance.InlineImages = true
	slog.Info("superagent: 长回复已分页渲染为图文消息", "platform", event.Platform, "channel", event.ChannelID, "pages", len(pages), "images", len(images), "textBytes", len(replyContent))
}
