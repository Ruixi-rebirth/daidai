package entity

import "daidai/internal/core"

const MaxQuotedMessageDepth = 4

const (
	CurrentImageSource = "current_message"
	QuotedImageSource  = "quoted_message"
	UserAvatarSource   = "user_avatar"
)

// CurrentImagesFromEvent selects the available images for this turn.
func CurrentImagesFromEvent(event *SensoryEvent) ([]*core.Attachment, int) {
	return selectEventImages(event, 1, false)
}

// QuotedImagesFromEvent returns unique images in the quoted-message chain,
// skipping layout images whose complete text representation is already stored.
func QuotedImagesFromEvent(event *SensoryEvent) ([]*core.Attachment, int) {
	return selectEventImages(event, MaxQuotedMessageDepth, true)
}

func selectEventImages(event *SensoryEvent, depthLimit int, skipTextCards bool) ([]*core.Attachment, int) {
	var images []*core.Attachment
	seen := make(map[string]struct{})
	omitted := 0
	for current, depth := event, 0; current != nil && depth < depthLimit; current, depth = current.ReplyContext, depth+1 {
		if skipTextCards && current.ContentComplete && current.ContentKind == "text_card" {
			continue
		}
		omitted += current.OmittedImages
		for _, image := range current.Images {
			if image == nil {
				continue
			}
			if image.URL == "" {
				omitted++
				continue
			}
			if _, exists := seen[image.URL]; exists {
				continue
			}
			seen[image.URL] = struct{}{}
			if len(images) < core.MaxVisionImagesPerRequest {
				images = append(images, image)
			} else {
				omitted++
			}
		}
	}
	return images, omitted
}
