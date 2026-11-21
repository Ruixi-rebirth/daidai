package chat

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/media"
)

func appendUniqueAttachment(slice []*core.Attachment, item *core.Attachment) []*core.Attachment {
	if item == nil {
		return slice
	}
	for _, existing := range slice {
		if existing == item || (existing != nil && existing.URL != "" && existing.URL == item.URL) {
			return slice
		}
	}
	return append(slice, item)
}

type chatFileContext struct {
	Source    string `json:"source"`
	Name      string `json:"name"`
	MIMEType  string `json:"mime_type,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	Status    string `json:"status"`
	Text      string `json:"text,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (c *Cortex) readFileContext(ctx context.Context, event *entity.SensoryEvent, files []*core.Attachment, pending bool) []chatFileContext {
	var result []chatFileContext
	for _, attachment := range files {
		if attachment == nil {
			continue
		}
		source := "current_message"
		if pending {
			source = "pending"
		} else if attachment.Quoted {
			source = "quoted_message"
		}
		file := chatFileContext{Source: source, Name: attachment.Name, MIMEType: attachment.MimeType, Status: "unavailable"}
		if c.inbox != nil && strings.TrimSpace(attachment.URL) != "" {
			const maxContentReadBytes = 256 * 1024
			fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			content, err := c.inbox.FetchContent(fetchCtx, attachment, maxContentReadBytes)
			cancel()
			if err != nil {
				slog.Warn("cortex: 获取文件附件失败", "name", attachment.Name, "err", err)
			} else {
				file.SizeBytes = content.TotalSize
				if file.SizeBytes <= 0 {
					file.SizeBytes = int64(len(content.Data))
				}
				if media.IsTextContent(content.Data) {
					text := string(content.Data)
					file.Text = core.TruncateRunes(text, 15000, "")
					file.Truncated = content.Truncated || len(file.Text) < len(text)
					file.Status = "text"
					event.ProcessedAttachments = appendUniqueAttachment(event.ProcessedAttachments, attachment)
				} else {
					file.Status = "binary"
				}
			}
		}
		result = append(result, file)
	}
	return result
}
