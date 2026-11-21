package chat

import (
	"daidai/internal/core"
	"daidai/internal/entity"
)

func applyAttachmentIntent(event *entity.SensoryEvent, intent attachmentIntentResult) {
	if event == nil {
		return
	}
	switch intent.Disposition {
	case attachmentIgnore:
		event.Images = nil
		event.OmittedImages = 0
		event.PendingImages = nil
		event.OmittedPendingImages = 0
		event.Files = nil
		event.OmittedFiles = 0
		event.PendingFiles = nil
		event.OmittedPendingFiles = 0
		event.VisionSummary = ""
	case attachmentProcess:
		applyCurrentAttachmentSelection(event, intent)
		applyPendingImageSelection(event, intent.PendingImages)
		applyPendingFileSelection(event, intent.PendingFiles)
	}
}

func applyCurrentAttachmentSelection(event *entity.SensoryEvent, intent attachmentIntentResult) {
	if len(intent.CurrentImages) == 0 {
		event.Images = nil
		event.OmittedImages = 0
	} else {
		event.Images = selectAttachments(event.Images, intent.CurrentImages)
	}
	if len(intent.CurrentFiles) == 0 {
		event.Files = nil
		event.OmittedFiles = 0
	} else {
		event.Files = selectAttachments(event.Files, intent.CurrentFiles)
	}
}

func applyPendingImageSelection(event *entity.SensoryEvent, indices []int) {
	if len(indices) > 0 {
		pendingImages := selectAttachments(event.PendingImages, indices)
		seen := make(map[string]struct{}, len(event.Images)+len(pendingImages))
		for _, attachment := range event.Images {
			if attachment != nil {
				seen[attachment.URL] = struct{}{}
			}
		}
		for _, attachment := range pendingImages {
			if attachment == nil {
				continue
			}
			if _, duplicate := seen[attachment.URL]; duplicate {
				continue
			}
			seen[attachment.URL] = struct{}{}
			if len(event.Images) >= core.MaxVisionImagesPerRequest {
				event.OmittedImages++
				continue
			}
			event.Images = append(event.Images, attachment)
		}
		event.OmittedImages += event.OmittedPendingImages
	}
	event.PendingImages = nil
	event.OmittedPendingImages = 0
}

func applyPendingFileSelection(event *entity.SensoryEvent, indices []int) {
	if len(indices) == 0 {
		event.PendingFiles = nil
		event.OmittedPendingFiles = 0
		return
	}
	event.PendingFiles = selectAttachments(event.PendingFiles, indices)
}
