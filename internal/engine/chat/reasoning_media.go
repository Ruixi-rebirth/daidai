package chat

import (
	"strings"

	"daidai/internal/core"
	"daidai/internal/entity"
)

type toolMediaDelivery struct {
	images       [][]byte
	imageSources []string
	fallbackText string
	mediaHistory string
	historyText  string
}

func (m *toolMediaDelivery) absorb(toolName string, result *core.ToolResult, presentation *thinkPresentation) (hasDelivery bool) {
	if historyText := strings.TrimSpace(result.HistoryText); historyText != "" {
		if m.historyText != "" {
			m.historyText += "\n"
		}
		m.historyText += historyText
	}
	images := make([][]byte, 0, len(result.Images)+1)
	images = append(images, result.Images...)
	if len(result.ImageData) > 0 {
		images = append(images, result.ImageData)
	}
	firstImage := len(m.images)
	for index, image := range images {
		if len(image) == 0 {
			continue
		}
		source := result.FallbackText
		if index < len(result.ImageSources) {
			source = result.ImageSources[index]
		}
		m.images = append(m.images, image)
		m.imageSources = append(m.imageSources, source)
	}

	if len(m.images) > firstImage {
		m.mediaHistory = strings.TrimSpace(m.mediaHistory + "\n" + result.MediaHistory)
		fallback := strings.TrimSpace(result.FallbackText)
		if fallback == "" {
			fallback = strings.TrimSpace(result.Text)
		}
		if toolName == "screenshot_web" {
			fallback = ""
		}
		if fallback != "" {
			if m.fallbackText != "" {
				m.fallbackText += "\n\n"
			}
			m.fallbackText += fallback
		}
		presentation.mediaInline = presentation.mediaInline || result.InlineImages
	}
	if result.Caption != "" {
		presentation.mediaCaption = strings.TrimSpace(presentation.mediaCaption + "\n\n" + result.Caption)
	}
	hasDelivery = len(m.images) > firstImage || result.Caption != ""
	if result.OnDelivered != nil {
		presentation.mediaCommits = append(presentation.mediaCommits, result.OnDelivered)
	}
	if result.OnDeliveryFailed != nil {
		presentation.mediaFailures = append(presentation.mediaFailures, result.OnDeliveryFailed)
	}
	presentation.toolFixedPresentation = presentation.toolFixedPresentation || result.FixedPresentation
	return hasDelivery
}

func (m *toolMediaDelivery) hasContent(presentation *thinkPresentation) bool {
	return len(m.images) > 0 || presentation.mediaCaption != ""
}

func (m *toolMediaDelivery) utterance(event *entity.SensoryEvent, text string) *entity.Utterance {
	return &entity.Utterance{
		Platform:     event.Platform,
		IsGroup:      event.IsGroup,
		ChannelID:    event.ChannelID,
		QuoteMsgID:   event.RawMessageID,
		Text:         text,
		MediaHistory: m.mediaHistory,
		HistoryText:  m.historyText,
		FallbackText: m.fallbackText,
		Images:       m.images,
		ImageSources: m.imageSources,
	}
}

func (m *toolMediaDelivery) fallbackUtterance(event *entity.SensoryEvent) *entity.Utterance {
	return m.utterance(event, "图片已准备好，但总结和回答没能完成。要我重新处理并补上吗？")
}
