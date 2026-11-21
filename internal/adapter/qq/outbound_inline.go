package qq

import (
	"bytes"
	"context"
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
	if len(images) == 0 {
		return fmt.Errorf("inline image message has no images")
	}
	type inlinePart struct {
		content, source, caption string
		imageEnd                 int
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
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, imageID := range imageIDs {
			if err := q.client.DeleteMarkdownImage(cleanupCtx, imageID); err != nil {
				slog.Warn("qq_channel: failed to release temporary Markdown image material", "imageID", imageID, "err", err)
			}
		}
		imageIDs = nil
	}
	defer cleanup()
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
	for i, data := range images {
		dimensions, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			cleanup()
			return fmt.Errorf("read inline image %d dimensions: %w", i+1, err)
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
			return fmt.Errorf("upload inline image %d: %w", i+1, err)
		}
		imageIDs = append(imageIDs, imageID)
		markup := fmt.Sprintf("![第 %d 页 #%dpx #%dpx](%s)", i+1, dimensions.Width, dimensions.Height, imageURL)
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
	flushImages(len(images))
	if strings.TrimSpace(content) != "" {
		last := &parts[len(parts)-1]
		if len(last.content)+2+len(content) <= 4000 {
			last.content += "\n\n" + content
			last.caption = content
		} else {
			for _, chunk := range core.SplitMarkdownChunks(content, 1200) {
				parts = append(parts, inlinePart{content: chunk, source: chunk, caption: chunk, imageEnd: len(images)})
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
	for index, part := range parts[:partsToSend] {
		if budget != nil {
			q.commitPassiveReplyAttempt(budget)
			reservedCount--
		}
		metadata := QuoteMetadata{Text: part.source, Kind: qqQuoteText, TextComplete: true}
		if part.hasImages {
			metadata.Kind = qqQuoteTextCard
			metadata.TextComplete = part.source != ""
			if part.hasNonTextImages {
				metadata.Kind = qqQuoteMixed
			}
			metadata.Text = strings.TrimSpace(part.source + "\n\n" + part.caption)
		}
		attempted = true
		if u.IsGroup {
			err = q.client.SendGroupMessageWithContext(ctx, u.ChannelID, part.content, u.QuoteMsgID, metadata)
		} else {
			err = q.client.SendC2CMessageWithContext(ctx, u.ChannelID, part.content, u.QuoteMsgID, metadata)
		}
		if err != nil {
			if index == 0 {
				return fmt.Errorf("send QQ Markdown message with inline images: %w", err)
			}
			done, pending := entity.ImageDeliveryText(imageSources, parts[index-1].imageEnd)
			var sentCaptions, pendingCaptions []string
			for n, p := range parts {
				if p.caption == "" {
					continue
				}
				if n < index {
					sentCaptions = append(sentCaptions, p.caption)
				} else {
					pendingCaptions = append(pendingCaptions, p.caption)
				}
			}
			return &entity.PartialDeliveryError{Err: err,
				DeliveredText:   strings.TrimSpace(done + "\n\n" + strings.Join(sentCaptions, "\n\n")),
				UndeliveredText: strings.TrimSpace(pending + "\n\n" + strings.Join(pendingCaptions, "\n\n"))}
		}
	}
	if limitedByBudget {
		done, pending := entity.ImageDeliveryText(imageSources, parts[partsToSend-1].imageEnd)
		var sentCaptions, pendingCaptions []string
		for index, part := range parts {
			if part.caption == "" {
				continue
			}
			if index < partsToSend {
				sentCaptions = append(sentCaptions, part.caption)
			} else {
				pendingCaptions = append(pendingCaptions, part.caption)
			}
		}
		return &entity.PartialDeliveryError{
			Err:             fmt.Errorf("QQ 被动回复额度仅剩 %d 条，已发送前 %d 条；剩余内容等待续发", partsToSend, partsToSend),
			DeliveredText:   strings.TrimSpace(done + "\n\n" + strings.Join(sentCaptions, "\n\n")),
			UndeliveredText: strings.TrimSpace(pending + "\n\n" + strings.Join(pendingCaptions, "\n\n")),
		}
	}
	slog.Info("qq_channel: 多图 Markdown 消息已接受，已释放临时素材", "channelID", u.ChannelID, "pages", len(images), "messages", len(parts))
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
