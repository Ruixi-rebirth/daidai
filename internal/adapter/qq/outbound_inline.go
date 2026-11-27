package qq

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"image/png"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
)

func (q *QQChannel) transmitInlineImages(ctx context.Context, u *entity.Utterance, images [][]byte, content string, partLimit int, replyWindow time.Duration) (sendErr error) {
	q.markdownImageMu.Lock()
	defer q.markdownImageMu.Unlock()
	attempted := false
	defer func() {
		if sendErr != nil && !attempted {
			sendErr = &entity.UnsentError{Err: sendErr}
		}
	}()
	if len(images) == 0 && len(u.InlineAssets) == 0 {
		return fmt.Errorf("inline image message has no images")
	}
	type inlinePart struct {
		content, source, caption string
		imageEnd                 int
		image                    []byte
		hasImages                bool
		hasNonTextImages         bool
	}
	var parts []inlinePart
	var markdown strings.Builder
	if u.IsGroup {
		if at := qqMentionMarkup(u.MentionUserIDs); at != "" {
			markdown.WriteString(at)
			markdown.WriteString("\n\n")
		}
	}
	imageSources := make([]string, len(images))
	for index := range images {
		imageSources[index] = u.FallbackText
		if index < len(u.ImageSources) {
			imageSources[index] = u.ImageSources[index]
		}
	}
	imageIDs := make([]string, 0, len(images))
	idToURL := make(map[string]string, len(images))
	deliveredIDs := make(map[string]struct{}, len(images))
	cleanupUnsent := func() {
		var unsent []string
		for _, imageID := range imageIDs {
			if _, delivered := deliveredIDs[imageID]; !delivered {
				unsent = append(unsent, imageID)
			}
		}
		if len(unsent) == 0 {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, imageID := range unsent {
			if err := q.client.DeleteMarkdownImage(cleanupCtx, imageID); err != nil {
				slog.Warn("qq_channel: failed to release unsent Markdown image material", "imageID", imageID, "err", err)
			}
		}
	}
	defer cleanupUnsent()
	firstImage := 0
	flushImages := func(end int) {
		source, _ := entity.ImageDeliveryText(imageSources[firstImage:end], end-firstImage)
		hasNonTextImages := firstImage < u.InlineNonTextImageCount
		for _, imageSource := range imageSources[firstImage:end] {
			if strings.TrimSpace(imageSource) == "" {
				hasNonTextImages = true
				break
			}
		}
		parts = append(parts, inlinePart{
			content: markdown.String(), source: source, imageEnd: end,
			hasImages: true, hasNonTextImages: hasNonTextImages,
		})
		markdown.Reset()
		firstImage = end
	}
	upload := func(data []byte, alt string) (string, error) {
		dimensions, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return "", err
		}
		imageID, imageURL, err := q.client.UploadMarkdownImage(ctx, data)
		if err != nil && strings.Contains(err.Error(), "40035007") {
			if reclaimErr := q.client.DeleteReplyMarkdownImages(ctx, imageIDs); reclaimErr != nil {
				slog.Warn("qq_channel: unable to reclaim old reply image materials", "err", reclaimErr)
			} else {
				imageID, imageURL, err = q.client.UploadMarkdownImage(ctx, data)
			}
		}
		if err != nil {
			return "", err
		}
		imageIDs = append(imageIDs, imageID)
		idToURL[imageID] = imageURL
		return fmt.Sprintf("![%s #%dpx #%dpx](%s)", alt, dimensions.Width, dimensions.Height, imageURL), nil
	}
	for i, data := range images {
		markup, err := upload(data, fmt.Sprintf("第 %d 页", i+1))
		if err != nil {
			if len(u.InlineAssets) == 0 || ctx.Err() != nil {
				return fmt.Errorf("prepare inline image %d: %w", i+1, err)
			}
			// Tool images may be JPEG or exceed the Markdown material limit.
			if firstImage < i {
				flushImages(i)
			}
			parts = append(parts, inlinePart{
				source: imageSources[i], imageEnd: i + 1,
				hasImages: true, hasNonTextImages: true, image: data,
			})
			firstImage = i + 1
			continue
		}
		if len(markup) > 4000 {
			return fmt.Errorf("QQ image reference exceeds message length limit")
		}
		if markdown.Len()+2+len(markup) > 4000 {
			if i == firstImage {
				return fmt.Errorf("QQ image reference and mention exceed message length limit")
			}
			flushImages(i)
		}
		if markdown.Len() > 0 {
			markdown.WriteString("\n\n")
		}
		markdown.WriteString(markup)
	}
	if firstImage < len(images) {
		flushImages(len(images))
	}
	var replacements []string
	quoteAssets := append([]core.InlineAsset(nil), u.InlineAssets...)
	for index, asset := range u.InlineAssets {
		if !strings.Contains(content, asset.Placeholder) {
			continue
		}
		markup, err := upload(asset.Data, asset.Alt)
		if err != nil {
			slog.Warn("qq_channel: diagram image upload failed", "err", err)
			markup = "[流程图未能发送]"
			quoteAssets[index].Fallback = markup
			quoteAssets[index].SemanticText = ""
		}
		replacements = append(replacements, asset.Placeholder, markup)
	}
	wire := strings.NewReplacer(replacements...)
	if strings.TrimSpace(content) != "" {
		rendered := wire.Replace(content)
		last := len(parts) - 1
		if last >= 0 && parts[last].image == nil && len(parts[last].content)+2+len(rendered) <= 4000 {
			parts[last].content += "\n\n" + rendered
			parts[last].caption = content
		} else if len(u.InlineAssets) > 0 && markdown.Len()+len(rendered) <= 4000 {
			parts = append(parts, inlinePart{content: markdown.String() + rendered, caption: content, imageEnd: len(images)})
		} else {
			chunkLimit := 1200
			if len(u.InlineAssets) > 0 {
				chunkLimit = 900
			}
			for index, chunk := range core.SplitMarkdownChunks(content, chunkLimit) {
				rendered := wire.Replace(chunk)
				if index == 0 {
					rendered = markdown.String() + rendered
				}
				if len(rendered) > 4000 && len(u.InlineAssets) > 0 {
					return fmt.Errorf("%w: QQ 图文消息超过长度限制", entity.ErrMessageTooLong)
				}
				parts = append(parts, inlinePart{content: rendered, caption: chunk, imageEnd: len(images)})
			}
		}
	}
	if !u.Proactive && len(parts) > partLimit {
		return fmt.Errorf("%w: QQ 图文与索引需要 %d 条消息，当前上限 %d 条", entity.ErrMessageTooLong, len(parts), partLimit)
	}
	budget, reservedCount, limitedByBudget, err := q.reservePassiveReplyBudget(u.QuoteMsgID, len(parts), partLimit, replyWindow)
	if err != nil {
		return err
	}
	defer func() { q.releasePassiveReplyReservations(budget, reservedCount) }()
	partsToSend := len(parts)
	if limitedByBudget {
		partsToSend = reservedCount
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	targetType := "users"
	if u.IsGroup {
		targetType = "groups"
	}
	partial := func(err error, sent int) error {
		done, pending := entity.ImageDeliveryText(imageSources, parts[sent-1].imageEnd)
		var sentCaptions, pendingCaptions []string
		for index, part := range parts {
			if part.caption == "" {
				continue
			}
			if index < sent {
				sentCaptions = append(sentCaptions, core.InlineAssetText(part.caption, quoteAssets, true))
			} else {
				pendingCaptions = append(pendingCaptions, core.InlineAssetText(part.caption, u.InlineAssets, false))
			}
		}
		return &entity.PartialDeliveryError{
			Err:             err,
			DeliveredText:   strings.TrimSpace(done + "\n\n" + strings.Join(sentCaptions, "\n\n")),
			UndeliveredText: strings.TrimSpace(pending + "\n\n" + strings.Join(pendingCaptions, "\n\n")),
		}
	}
	for index, part := range parts[:partsToSend] {
		if budget != nil {
			q.commitPassiveReplyAttempt(budget)
			reservedCount--
		}
		caption := core.InlineAssetText(part.caption, quoteAssets, true)
		metadata := QuoteMetadata{Text: strings.TrimSpace(part.source + "\n\n" + caption), Kind: qqQuoteText, TextComplete: true}
		if part.hasImages || caption != part.caption {
			metadata.Kind = qqQuoteTextCard
			metadata.TextComplete = metadata.Text != ""
			if part.hasNonTextImages {
				metadata.Kind = qqQuoteMixed
			}
		}
		attempted = true
		quoteRef := index == 0 && !u.Proactive && u.QuoteMsgID != "" && !u.AnonymousInterjection && q.hasNewerInboundMessage(u.IsGroup, u.ChannelID, u.QuoteMsgID)
		if len(part.image) > 0 {
			err = q.client.sendMediaMessageWithReference(ctx, targetType, u.ChannelID, part.content, part.image, u.QuoteMsgID, quoteRef, metadata)
		} else {
			err = q.client.sendMessageWithReference(ctx, targetType, u.ChannelID, part.content, u.QuoteMsgID, quoteRef, metadata)
		}
		if err != nil {
			var unsent *entity.UnsentError
			if budget != nil && errors.As(err, &unsent) {
				q.restoreUnsentPassiveReply(budget)
				reservedCount++
			}
			if index == 0 {
				return fmt.Errorf("send QQ Markdown message with inline images: %w", err)
			}
			return partial(err, index)
		}
		for _, imageID := range imageIDs {
			if imageURL := idToURL[imageID]; imageURL != "" && strings.Contains(part.content, imageURL) {
				deliveredIDs[imageID] = struct{}{}
			}
		}
	}
	if limitedByBudget {
		return partial(fmt.Errorf("QQ 被动回复额度仅剩 %d 条，已发送前 %d 条；剩余内容等待续发", partsToSend, partsToSend), partsToSend)
	}
	if len(u.InlineAssets) > 0 {
		source, _ := entity.ImageDeliveryText(imageSources, len(images))
		u.QuoteText = strings.TrimSpace(source + "\n\n" + core.InlineAssetText(content, quoteAssets, true))
	}
	slog.Info("qq_channel: 多图 Markdown 消息已接受", "channelID", u.ChannelID, "pages", len(images), "messages", len(parts))
	return nil
}

func qqMentionMarkup(userIDs []string) string {
	var at strings.Builder
	for _, userID := range userIDs {
		if strings.TrimSpace(userID) == "" {
			continue
		}
		fmt.Fprintf(&at, `<qqbot-at-user id="%s" /> `, html.EscapeString(userID))
	}
	return at.String()
}
