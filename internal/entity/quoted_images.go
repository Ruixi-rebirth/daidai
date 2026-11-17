package entity

import "daidai/internal/core"

// QuotedImagesFromEvent returns unique images in the quoted-message chain,
// skipping layout images whose complete text representation is already stored.
func QuotedImagesFromEvent(event *SensoryEvent) ([]*core.Attachment, int) {
	var images []*core.Attachment
	seen := make(map[string]struct{})
	omitted := 0
	for current, depth := event, 0; current != nil && depth < 4; current, depth = current.ReplyContext, depth+1 {
		if current.ContentComplete && current.ContentKind == "text_card" {
			continue
		}
		for _, image := range current.Images {
			if image == nil || image.URL == "" {
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
	for current := event; current != nil; current = current.ReplyContext {
		if !current.ContentComplete || current.ContentKind != "text_card" {
			omitted += current.OmittedImages
		}
	}
	return images, omitted
}
