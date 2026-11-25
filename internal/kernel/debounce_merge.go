package kernel

import (
	"strings"

	"daidai/internal/core"
)

func mergeQueuedEvents(events []queuedEvent) queuedEvent {
	if len(events) == 0 {
		return queuedEvent{}
	}
	plain := make([]*core.SensoryEvent, 0, len(events))
	for _, event := range events {
		plain = append(plain, event.event)
	}
	last := events[len(events)-1]
	return queuedEvent{event: mergeEvents(plain), generation: last.generation, senderGeneration: last.senderGeneration}
}

func sameReplyContext(a, b *core.SensoryEvent) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.ReplyContext == nil || b.ReplyContext == nil {
		return a.ReplyContext == nil && b.ReplyContext == nil
	}
	return strings.TrimSpace(a.ReplyContext.Content) == strings.TrimSpace(b.ReplyContext.Content) &&
		a.ReplyContext.RawMessageID == b.ReplyContext.RawMessageID
}

// mergeEvents 将同一个人在窗口内的多条感知事件合并为一条事件
func mergeEvents(events []*core.SensoryEvent) *core.SensoryEvent {
	if len(events) == 1 {
		return events[0]
	}
	latest := events[len(events)-1]
	// Preserve the latest envelope (including access and validity metadata).
	// Only fields aggregated below start empty.
	merged := *latest
	merged.HistoryRecordIDs = nil
	merged.MentionedUsers = nil
	merged.OmittedImages, merged.OmittedFiles = 0, 0
	merged.OmittedPendingImages, merged.OmittedPendingFiles = 0, 0

	var textParts []string
	var allImages []*core.Attachment
	var allFiles []*core.Attachment
	mentioned := make(map[string]struct{})

	for _, ev := range events {
		for _, user := range ev.MentionedUsers {
			if user.ID == "" {
				continue
			}
			if _, exists := mentioned[user.ID]; exists {
				continue
			}
			mentioned[user.ID] = struct{}{}
			merged.MentionedUsers = append(merged.MentionedUsers, user)
		}
		for _, id := range ev.HistoryRecordIDs {
			if id > 0 {
				merged.HistoryRecordIDs = append(merged.HistoryRecordIDs, id)
			}
		}
		if ev.Content != "" {
			textParts = append(textParts, ev.Content)
		}
		if ev.ReplyContext != nil {
			merged.ReplyContext = ev.ReplyContext
		}
		merged.PendingFiles = ev.PendingFiles
		merged.PendingImages = ev.PendingImages
		merged.OmittedImages += ev.OmittedImages
		merged.OmittedFiles += ev.OmittedFiles
		if ev.OmittedPendingImages > merged.OmittedPendingImages {
			merged.OmittedPendingImages = ev.OmittedPendingImages
		}
		if ev.OmittedPendingFiles > merged.OmittedPendingFiles {
			merged.OmittedPendingFiles = ev.OmittedPendingFiles
		}
		remainingImages := core.MaxVisionImagesPerRequest - len(allImages)
		if remainingImages > 0 {
			if len(ev.Images) > remainingImages {
				allImages = append(allImages, ev.Images[:remainingImages]...)
				merged.OmittedImages += len(ev.Images) - remainingImages
			} else {
				allImages = append(allImages, ev.Images...)
			}
		} else {
			merged.OmittedImages += len(ev.Images)
		}
		remainingFiles := core.MaxIncomingFilesPerRequest - len(allFiles)
		if remainingFiles > 0 {
			if len(ev.Files) > remainingFiles {
				allFiles = append(allFiles, ev.Files[:remainingFiles]...)
				merged.OmittedFiles += len(ev.Files) - remainingFiles
			} else {
				allFiles = append(allFiles, ev.Files...)
			}
		} else {
			merged.OmittedFiles += len(ev.Files)
		}
	}

	merged.Content = strings.Join(textParts, "\n")
	merged.Images = allImages
	merged.Files = allFiles
	if len(allImages) > 0 && len(merged.PendingImages) > 0 {
		currentURLs := make(map[string]struct{}, len(allImages))
		for _, attachment := range allImages {
			if attachment != nil && attachment.URL != "" {
				currentURLs[attachment.URL] = struct{}{}
			}
		}
		pendingImages := make([]*core.Attachment, 0, len(merged.PendingImages))
		for _, attachment := range merged.PendingImages {
			if attachment == nil {
				continue
			}
			if _, duplicated := currentURLs[attachment.URL]; duplicated {
				continue
			}
			pendingImages = append(pendingImages, attachment)
		}
		merged.PendingImages = pendingImages
	}
	if len(allFiles) > 0 && len(merged.PendingFiles) > 0 {
		currentURLs := make(map[string]struct{}, len(allFiles))
		for _, attachment := range allFiles {
			if attachment != nil && attachment.URL != "" {
				currentURLs[attachment.URL] = struct{}{}
			}
		}
		pendingFiles := make([]*core.Attachment, 0, len(merged.PendingFiles))
		for _, attachment := range merged.PendingFiles {
			if attachment == nil {
				continue
			}
			if _, duplicated := currentURLs[attachment.URL]; duplicated {
				continue
			}
			pendingFiles = append(pendingFiles, attachment)
		}
		merged.PendingFiles = pendingFiles
	}
	return &merged
}
