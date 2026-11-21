package chat

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/media"
)

const attachmentCorrelationWindow = 2 * time.Second

func (c *Cortex) mergePendingAttachments(event *entity.SensoryEvent) {
	if c.inbox == nil || (event.Platform != "qq" && event.Platform != "telegram") {
		return
	}
	scope := media.AttachmentScope(event.Platform, event.IsGroup, event.ChannelID, event.SenderID)
	if !event.OccurredAt.IsZero() {
		end := event.OccurredAt.Add(attachmentCorrelationWindow)
		event.PendingImages = appendUniqueAttachments(event.PendingImages, c.inbox.PendingReceivedBetween(scope, core.AttachmentImage, event.OccurredAt, end))
		event.PendingFiles = appendUniqueAttachments(event.PendingFiles, c.inbox.PendingReceivedBetween(scope, core.AttachmentFile, event.OccurredAt, end))
	}
	currentImages, omittedImages := c.inbox.PendingKind(scope, core.AttachmentImage)
	var staleImages int
	event.PendingImages, staleImages = retainStillPending(event.PendingImages, currentImages)
	event.OmittedPendingImages += staleImages
	if omittedImages > event.OmittedPendingImages {
		event.OmittedPendingImages = omittedImages
	}
	currentFiles, omittedFiles := c.inbox.PendingKind(scope, core.AttachmentFile)
	var staleFiles int
	event.PendingFiles, staleFiles = retainStillPending(event.PendingFiles, currentFiles)
	event.OmittedPendingFiles += staleFiles
	if omittedFiles > event.OmittedPendingFiles {
		event.OmittedPendingFiles = omittedFiles
	}
}

func boundAttachmentInput(event *entity.SensoryEvent) {
	if len(event.Images) > core.MaxVisionImagesPerRequest {
		event.OmittedImages += len(event.Images) - core.MaxVisionImagesPerRequest
		event.Images = event.Images[:core.MaxVisionImagesPerRequest]
	}
	event.Content = core.TruncateRunes(event.Content, core.MaxUserMessageRunes, "\n[消息过长，后续内容已截断]")
	event.VisionSummary = core.TruncateRunes(event.VisionSummary, core.MaxVisionSummaryRunes, "\n[视觉摘要过长，后续内容已截断]")
}

func hasAttachmentIntentInput(event *entity.SensoryEvent) bool {
	return len(event.Images) > 0 || event.OmittedImages > 0 || len(event.Files) > 0 || event.OmittedFiles > 0 ||
		len(event.PendingImages) > 0 || event.OmittedPendingImages > 0 || len(event.PendingFiles) > 0 || event.OmittedPendingFiles > 0
}

func (c *Cortex) resolveAttachmentIntent(ctx context.Context, event *entity.SensoryEvent) *entity.Utterance {
	if (event.Platform != "qq" && event.Platform != "telegram") || !hasAttachmentIntentInput(event) {
		return nil
	}
	intent, err := c.attachmentIntent(ctx, event)
	if intent.Disposition == attachmentAsk {
		return attachmentIntentQuestion(event, err)
	}
	applyAttachmentIntent(event, intent)
	return nil
}

func attachmentIntentQuestion(event *entity.SensoryEvent, intentErr error) *entity.Utterance {
	message := "我收到了附件，但还没有查看或读取。请告诉我希望怎么处理，例如描述图片、总结文件或提取信息。"
	if intentErr != nil {
		slog.Warn("cortex: 附件处理意图无法确认，未读取附件", "err", intentErr)
		message = "我暂时无法确认你是否希望我处理这些附件。请明确告诉我希望怎么处理，例如描述图片、总结文件或提取信息。"
	}
	var omitted []string
	if event.OmittedFiles > 0 {
		omitted = append(omitted, fmt.Sprintf("另有 %d 个文件未接收", event.OmittedFiles))
	}
	if event.OmittedImages > 0 {
		omitted = append(omitted, fmt.Sprintf("另有 %d 张图片未接收", event.OmittedImages))
	}
	if event.OmittedPendingFiles > 0 {
		omitted = append(omitted, fmt.Sprintf("另有 %d 个较早的文件已从待处理列表移除", event.OmittedPendingFiles))
	}
	if event.OmittedPendingImages > 0 {
		omitted = append(omitted, fmt.Sprintf("另有 %d 张较早的图片已从待处理列表移除", event.OmittedPendingImages))
	}
	if len(omitted) > 0 {
		message += "\n" + strings.Join(omitted, "；") + "。如需处理，请分批重新发送。"
	}
	return &entity.Utterance{
		Platform:   event.Platform,
		IsGroup:    event.IsGroup,
		ChannelID:  event.ChannelID,
		QuoteMsgID: event.RawMessageID,
		Text:       message,
	}
}
