package qq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
)

func attachmentHistoryNote(images, omittedImages int, files, omittedFiles int) string {
	var kinds []string
	if count := images + omittedImages; count > 0 {
		kinds = append(kinds, fmt.Sprintf("图片 %d 张", count))
	}
	if count := files + omittedFiles; count > 0 {
		kinds = append(kinds, fmt.Sprintf("文件 %d 个", count))
	}
	if len(kinds) == 0 {
		return ""
	}
	return "[附件：" + strings.Join(kinds, "，") + "；未保存附件本体]"
}

// Transmit 物理发声：投递全息表达体到 QQ 客户端 (支持大文本自动分片与图文混合发送)
func (q *QQChannel) Transmit(ctx context.Context, u *entity.Utterance) (sendErr error) {
	if u == nil {
		return nil
	}
	defer func() {
		if sendErr == nil && u.IsGroup && u.ChannelID != "" {
			target := u.TargetUserID
			if target == "" && len(u.MentionUserIDs) > 0 {
				target = u.MentionUserIDs[0]
			}
			q.recordGroupActiveSession(u.ChannelID, target)
		}
	}()
	if u.Proactive {
		defer func() {
			observation := &sendObservation{at: time.Now(), success: sendErr == nil}
			if u.IsGroup {
				q.proactiveGroup.Store(observation)
			} else {
				q.proactivePrivate.Store(observation)
			}
		}()
	}
	if q.client == nil {
		return fmt.Errorf("QQ client is unavailable")
	}

	u = u.WithoutEmptyImages()
	images := u.Images
	if len(images) == 0 && len(u.CardData) > 0 {
		images = [][]byte{u.CardData}
	}
	content := u.Text
	if content == "" && len(images) == 0 {
		return nil
	}

	if u.ChannelID == "" {
		return fmt.Errorf("empty channelID in utterance")
	}
	if u.Proactive && u.QuoteMsgID != "" {
		return fmt.Errorf("QQ 主动消息不能同时携带被动回复引用 ID")
	}
	if u.GroupInterjection && (!u.IsGroup || u.Proactive || u.QuoteMsgID == "") {
		return fmt.Errorf("QQ群聊插话必须关联触发它的群消息，并通过被动回复发送")
	}
	if u.QuoteMsgID == "" && !u.Proactive {
		return fmt.Errorf("QQ 消息缺少入站引用 ID，必须显式标记为主动发送")
	}
	partLimit := maxQQOutgoingParts
	replyWindow := time.Duration(0)
	if !u.Proactive {
		partLimit = maxQQPassiveReplies(u.IsGroup)
		replyWindow = time.Hour
		if u.IsGroup {
			replyWindow = 5 * time.Minute
		}
	}
	// Mentions are Markdown markup. Keep the mention and image in the same
	// Markdown message rather than dropping it on the rich-media branch.
	if len(images) > 0 && (u.InlineImages || (u.IsGroup && len(u.MentionUserIDs) > 0)) {
		return q.transmitInlineImages(ctx, u, images, content, partLimit, replyWindow)
	}

	chunkLimit := 1200
	if u.IsGroup && len(u.MentionUserIDs) > 0 {
		chunkLimit -= len([]rune(qqMentionMarkup(u.MentionUserIDs)))
		if chunkLimit <= 0 {
			return fmt.Errorf("QQ 群聊 @ 提及标记超过单条消息长度限制")
		}
	}
	chunks := core.SplitMarkdownChunks(content, chunkLimit)
	if len(chunks) == 0 {
		chunks = []string{""}
	}
	if len(chunks) > partLimit {
		return fmt.Errorf("%w: QQ 单次发送最多 %d 条，当前文字需要 %d 条，请缩短回复", entity.ErrMessageTooLong, partLimit, len(chunks))
	}
	parts := make([]qqOutgoingPart, 0, len(chunks)+len(images)-1)
	sources := make([]string, len(images))
	for index := range images {
		sources[index] = u.FallbackText
		if index < len(u.ImageSources) {
			sources[index] = u.ImageSources[index]
		}
	}
	for i, chunk := range chunks {
		part := qqOutgoingPart{text: chunk}
		if i == 0 && len(images) > 0 {
			part.image = images[0]
			part.source = sources[0]
		}
		parts = append(parts, part)
	}
	if len(images) > 1 {
		for index, image := range images[1:] {
			parts = append(parts, qqOutgoingPart{image: image, source: sources[index+1]})
		}
	}
	if len(parts) > partLimit {
		return fmt.Errorf("%w: 图文内容需要发送 %d 条，超过 QQ 单次最多 %d 条的限制", entity.ErrMessageTooLong, len(parts), partLimit)
	}
	budget, reservedCount, limitedByBudget, err := q.reservePassiveReplyBudget(u.QuoteMsgID, len(parts), partLimit, replyWindow)
	if err != nil {
		return err
	}
	defer func() { q.releasePassiveReplyReservations(budget, reservedCount) }()

	targetType := "users"
	if u.IsGroup {
		targetType = "groups"
	}
	sentParts := 0
	sendTrackedPart := func(part qqOutgoingPart, wireText string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if sentParts == 0 && u.IsGroup && len(u.MentionUserIDs) > 0 && len(part.image) == 0 {
			if at := qqMentionMarkup(u.MentionUserIDs); at != "" {
				wireText = at + wireText
				if len([]rune(wireText)) > 1200 {
					return fmt.Errorf("群聊回复的 @ 提及标记超过 QQ 单条消息长度限制")
				}
			}
		}
		if budget != nil {
			// Commit immediately before the request: a network error may arrive
			// after QQ accepted it, but local validation failures must not consume quota.
			q.commitPassiveReplyAttempt(budget)
			reservedCount--
		}
		if len(part.image) == 0 {
			return q.client.sendMessageWithContext(ctx, targetType, u.ChannelID, wireText, u.QuoteMsgID, QuoteMetadata{Text: part.text, Kind: qqQuoteText, TextComplete: true})
		}
		metadata := mediaQuoteMetadata(part.text)
		if source := strings.TrimSpace(part.source); source != "" {
			metadata = QuoteMetadata{Text: source, Kind: qqQuoteMixed, TextComplete: true}
			if len(u.ImageSources) > 0 {
				metadata.Kind = qqQuoteTextCard
				metadata.Text = strings.TrimSpace(source + "\n\n" + part.text)
			}
		}
		return q.client.sendMediaMessage(ctx, targetType, u.ChannelID, wireText, part.image, u.QuoteMsgID, metadata)
	}

	deliveredText := func() string {
		var delivered strings.Builder
		imageCount := 0
		for _, part := range parts[:sentParts] {
			if part.text != "" {
				if delivered.Len() > 0 {
					delivered.WriteString("\n\n")
				}
				delivered.WriteString(part.text)
			}
			if len(part.image) > 0 {
				imageCount++
			}
		}
		done, _ := entity.ImageDeliveryText(sources, imageCount)
		return strings.TrimSpace(delivered.String() + "\n\n" + done)
	}
	undeliveredText := func(from int) string {
		var remaining strings.Builder
		imageCount := 0
		for _, part := range parts[:from] {
			if len(part.image) > 0 {
				imageCount++
			}
		}
		for _, part := range parts[from:] {
			if part.text != "" {
				if remaining.Len() > 0 {
					remaining.WriteString("\n\n")
				}
				remaining.WriteString(part.text)
			}
		}
		_, pending := entity.ImageDeliveryText(sources, imageCount)
		return strings.TrimSpace(remaining.String() + "\n\n" + pending)
	}
	partsToSend := len(parts)
	if limitedByBudget {
		partsToSend = reservedCount
	}
	for i, part := range parts[:partsToSend] {
		wireText := part.text
		if (limitedByBudget || u.HasMoreText) && i == partsToSend-1 {
			command := "继续"
			if u.DraftID != "" {
				command = "/continue " + u.DraftID
			}
			withNotice := strings.TrimSpace(part.text + "\n[剩余内容未发送；回复“" + command + "”可接着看。]")
			if len([]rune(withNotice)) <= chunkLimit {
				wireText = withNotice
			}
		}
		if i > 0 {
			select {
			case <-ctx.Done():
				if sentParts > 0 {
					return &entity.PartialDeliveryError{Err: ctx.Err(), DeliveredText: deliveredText(), UndeliveredText: undeliveredText(sentParts)}
				}
				return ctx.Err()
			case <-time.After(150 * time.Millisecond):
			}
		}
		err := sendTrackedPart(part, wireText)
		if err != nil {
			var unsent *entity.UnsentError
			if budget != nil && errors.As(err, &unsent) {
				q.restoreUnsentPassiveReply(budget)
				reservedCount++
			}
			slog.Error("qq_channel: 消息分片发送失败", "channelID", u.ChannelID, "isGroup", u.IsGroup, "quoteID", u.QuoteMsgID, "sentParts", sentParts, "err", err)
			if sentParts > 0 {
				partialText := deliveredText()
				if ctx.Err() != nil {
					return &entity.PartialDeliveryError{
						Err:             ctx.Err(),
						DeliveredText:   partialText,
						UndeliveredText: undeliveredText(sentParts),
					}
				}
				q.releasePassiveReplyReservations(budget, reservedCount)
				reservedCount = 0
				canSendNotice := sentParts+1 < partLimit
				if budget != nil {
					canSendNotice = q.reserveAdditionalPassiveReply(budget)
					if canSendNotice {
						reservedCount++
					}
				}
				if canSendNotice {
					notice := "部分内容发送失败，前面的内容已送达。"
					if len(part.image) > 0 {
						notice = "部分图片发送失败，前面的内容已送达。"
					}
					if noticeErr := sendTrackedPart(qqOutgoingPart{text: notice}, notice); noticeErr != nil {
						return &entity.PartialDeliveryError{
							Err:             fmt.Errorf("send part: %v; send partial-delivery notice: %w", err, noticeErr),
							DeliveredText:   partialText,
							UndeliveredText: undeliveredText(sentParts),
						}
					}
				}
				return &entity.PartialDeliveryError{Err: err, DeliveredText: partialText, UndeliveredText: undeliveredText(sentParts)}
			}
			return err
		}
		sentParts++
	}
	if partsToSend < len(parts) {
		return &entity.PartialDeliveryError{
			Err:             fmt.Errorf("QQ 被动回复额度仅剩 %d 条，已发送前 %d 条；剩余内容等待续发", partsToSend, sentParts),
			DeliveredText:   deliveredText(),
			UndeliveredText: undeliveredText(sentParts),
		}
	}

	slog.Info("qq_channel: 表达成功送达客户端", "channelID", u.ChannelID, "isGroup", u.IsGroup, "hasMedia", u.HasMedia())
	return nil
}
