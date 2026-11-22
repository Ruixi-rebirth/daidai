package entity

import (
	"fmt"
	"strings"
)

// ImageDeliveryText accounts for shared sources only after all pages carrying
// that source were delivered. Empty sources represent genuine images.
func ImageDeliveryText(sources []string, delivered int) (complete, pending string) {
	last := map[string]int{}
	for i, source := range sources {
		if source = strings.TrimSpace(source); source != "" {
			last[source] = i
		}
	}
	seen := map[string]bool{}
	var done, todo []string
	missing := 0
	for i, source := range sources {
		source = strings.TrimSpace(source)
		if source == "" {
			if i >= delivered {
				missing++
			}
			continue
		}
		if seen[source] {
			continue
		}
		seen[source] = true
		if last[source] < delivered {
			done = append(done, source)
		} else {
			todo = append(todo, source)
		}
	}
	if missing > 0 {
		todo = append(todo, fmt.Sprintf("[%d 张图片未发送完成；图片无法通过文字续发，请重新请求发送或生成。]", missing))
	}
	return strings.Join(done, "\n\n"), strings.Join(todo, "\n\n")
}

// WithoutEmptyImages keeps image positions aligned with their source text.
func (u *Utterance) WithoutEmptyImages() *Utterance {
	for _, image := range u.Images {
		if len(image) != 0 {
			continue
		}
		filtered := *u
		filtered.Images = nil
		filtered.ImageSources = nil
		filtered.InlineNonTextImageCount = 0
		filtered.ReplyCardPages = 0
		for index, data := range u.Images {
			if len(data) == 0 {
				continue
			}
			filtered.Images = append(filtered.Images, data)
			if index < len(u.ImageSources) {
				filtered.ImageSources = append(filtered.ImageSources, u.ImageSources[index])
			}
			if index < u.InlineNonTextImageCount {
				filtered.InlineNonTextImageCount++
			}
			if index >= len(u.Images)-u.ReplyCardPages {
				filtered.ReplyCardPages++
			}
		}
		filtered.InlineImages = len(filtered.Images) > 0 && u.InlineImages
		return &filtered
	}
	return u
}
