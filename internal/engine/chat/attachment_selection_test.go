package chat

import (
	"context"
	"strings"
	"testing"

	"daidai/internal/core"
	"daidai/internal/entity"
)

func TestPrepareAttachmentsAcceptsNilEvent(t *testing.T) {
	if got := NewCortex(CortexConfig{}).prepareAttachments(context.Background(), nil); got != nil {
		t.Fatalf("nil event produced a reply: %+v", got)
	}
}

func TestBoundAttachmentInputCapsImagesAndText(t *testing.T) {
	event := &entity.SensoryEvent{
		Images:        make([]*core.Attachment, core.MaxVisionImagesPerRequest+2),
		Content:       strings.Repeat("字", core.MaxUserMessageRunes+10),
		VisionSummary: strings.Repeat("图", core.MaxVisionSummaryRunes+10),
	}
	boundAttachmentInput(event)
	if len(event.Images) != core.MaxVisionImagesPerRequest || event.OmittedImages != 2 {
		t.Fatalf("image count/omission not bounded: %d/%d", len(event.Images), event.OmittedImages)
	}
	if len([]rune(event.Content)) > core.MaxUserMessageRunes || len([]rune(event.VisionSummary)) > core.MaxVisionSummaryRunes {
		t.Fatalf("text fields exceed their budgets: content=%d vision=%d", len([]rune(event.Content)), len([]rune(event.VisionSummary)))
	}
}

func TestApplyAttachmentIntentIgnoreClearsAllAttachmentState(t *testing.T) {
	event := &entity.SensoryEvent{
		Images: []*core.Attachment{{URL: "current-image"}}, OmittedImages: 1,
		PendingImages: []*core.Attachment{{URL: "pending-image"}}, OmittedPendingImages: 2,
		Files: []*core.Attachment{{URL: "current-file"}}, OmittedFiles: 3,
		PendingFiles: []*core.Attachment{{URL: "pending-file"}}, OmittedPendingFiles: 4,
		VisionSummary: "previously observed image",
	}

	applyAttachmentIntent(event, attachmentIntentResult{Disposition: attachmentIgnore})
	if len(event.Images)+len(event.Files)+len(event.PendingImages)+len(event.PendingFiles) != 0 {
		t.Fatalf("ignored attachments were retained: %+v", event)
	}
	if event.OmittedImages+event.OmittedFiles+event.OmittedPendingImages+event.OmittedPendingFiles != 0 || event.VisionSummary != "" {
		t.Fatalf("ignored attachment state was retained: %+v", event)
	}
}

func TestApplyAttachmentIntentSelectsAndMergesAttachmentsDeterministically(t *testing.T) {
	currentImage := &core.Attachment{Kind: core.AttachmentImage, URL: "image-current"}
	currentFile := &core.Attachment{Kind: core.AttachmentFile, URL: "file-current"}
	duplicatePendingImage := &core.Attachment{Kind: core.AttachmentImage, URL: currentImage.URL}
	pendingImage := &core.Attachment{Kind: core.AttachmentImage, URL: "image-pending"}
	lastPendingImage := &core.Attachment{Kind: core.AttachmentImage, URL: "image-last-in-limit"}
	overLimitImage := &core.Attachment{Kind: core.AttachmentImage, URL: "image-over-limit"}
	pendingFile := &core.Attachment{Kind: core.AttachmentFile, URL: "file-pending"}
	event := &entity.SensoryEvent{
		Images: []*core.Attachment{currentImage}, OmittedImages: 1,
		PendingImages: []*core.Attachment{duplicatePendingImage, pendingImage, lastPendingImage, overLimitImage}, OmittedPendingImages: 2,
		Files: []*core.Attachment{{Kind: core.AttachmentFile, URL: "file-unselected"}, currentFile}, OmittedFiles: 1,
		PendingFiles: []*core.Attachment{{Kind: core.AttachmentFile, URL: "file-unselected-pending"}, pendingFile}, OmittedPendingFiles: 1,
	}
	intent := attachmentIntentResult{
		Disposition:   attachmentProcess,
		CurrentImages: []int{0}, CurrentFiles: []int{1},
		PendingImages: []int{0, 1, 2, 3}, PendingFiles: []int{1},
	}

	applyAttachmentIntent(event, intent)
	if len(event.Images) != core.MaxVisionImagesPerRequest || event.Images[0] != currentImage || event.Images[1] != pendingImage || event.Images[2] != lastPendingImage {
		t.Fatalf("unexpected selected image order: %+v", event.Images)
	}
	if event.OmittedImages != 4 {
		t.Fatalf("omitted image count = %d, want existing + pending + over-limit count 4", event.OmittedImages)
	}
	if len(event.Files) != 1 || event.Files[0] != currentFile || len(event.PendingFiles) != 1 || event.PendingFiles[0] != pendingFile {
		t.Fatalf("unexpected selected files: current=%+v pending=%+v", event.Files, event.PendingFiles)
	}
	if event.OmittedFiles != 1 || event.OmittedPendingFiles != 1 {
		t.Fatalf("file omission counts changed: current=%d pending=%d", event.OmittedFiles, event.OmittedPendingFiles)
	}
	if event.PendingImages != nil || event.OmittedPendingImages != 0 {
		t.Fatalf("selected pending images should move into current input: %+v", event)
	}
}
