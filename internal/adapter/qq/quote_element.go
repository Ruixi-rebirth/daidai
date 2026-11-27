package qq

import (
	"mime"
	"path/filepath"
	"strings"

	"daidai/internal/core"
	"daidai/internal/entity"
)

const quoteAttachmentPlaceholder = "[引用消息仅包含图片或文件附件]"

func qqQuotedMessageContext(element map[string]any, depth int) *entity.SensoryEvent {
	if element == nil || depth >= maxQQQuoteDepth {
		return nil
	}

	content := qqQuotedElementContent(element)
	authorName, authorID := qqQuotedElementAuthor(element)
	sourceMessageID := qqQuotedElementID(element)
	images, files, omittedImages, omittedFiles := qqQuotedElementAttachments(element, content, sourceMessageID)
	if content == "" && (len(images)+len(files) > 0 || omittedImages+omittedFiles > 0) {
		content = quoteAttachmentPlaceholder
	}

	quoted := &entity.SensoryEvent{
		Content:       content,
		ContentKind:   qqQuotedContentKind(content, images, files, omittedImages, omittedFiles),
		SenderName:    authorName,
		SenderID:      authorID,
		RawMessageID:  sourceMessageID,
		Images:        images,
		Files:         files,
		OmittedImages: omittedImages,
		OmittedFiles:  omittedFiles,
	}
	if content == "" && len(images)+len(files) == 0 {
		if sourceMessageID != "" {
			return quoted
		}
		return nil
	}
	if nested := qqReferencedMessageElement(element); nested != nil {
		quoted.ReplyContext = qqQuotedMessageContext(nested, depth+1)
	}
	return quoted
}

func qqQuotedElementContent(element map[string]any) string {
	content, _ := element["content"].(string)
	content = entity.SanitizeQuotedMessageContent(content)
	subElements, _ := element["msg_elements"].([]any)
	if len(subElements) == 0 {
		return content
	}
	historyText := extractQQChatHistoryText(subElements)
	if historyText == "" {
		return content
	}
	title, _ := element["title"].(string)
	title = strings.TrimSpace(title)
	if title != "" && !strings.HasPrefix(title, "[") {
		title = "[" + title + "]"
	}
	if title != "" {
		historyText = title + "\n" + historyText
	}
	if content == "" || content == "[聊天记录]" || content == "[转发的聊天记录]" {
		return historyText
	}
	return content + "\n\n" + historyText
}

func qqQuotedElementAuthor(element map[string]any) (name, id string) {
	author, _ := element["author"].(map[string]any)
	if author == nil {
		return "", ""
	}
	name, _ = author["username"].(string)
	for _, field := range []string{"member_openid", "user_openid", "id"} {
		if value, _ := author[field].(string); value != "" {
			id = value
			break
		}
	}
	return core.NormalizeDisplayName(name, id), id
}

func qqQuotedElementID(element map[string]any) string {
	id, _ := element["msg_idx"].(string)
	if id == "" {
		id, _ = element["id"].(string)
	}
	return id
}

func qqQuotedElementAttachments(element map[string]any, content, sourceMessageID string) (images, files []*core.Attachment, omittedImages, omittedFiles int) {
	for _, imageURL := range qqQuotedImageURLs(content) {
		images = append(images, &core.Attachment{
			Kind: core.AttachmentImage, URL: imageURL, MimeType: "image/jpeg",
			Name: "引用图片", SourceMessageID: sourceMessageID, Quoted: true,
		})
	}
	attachments, _ := element["attachments"].([]any)
	for _, item := range attachments {
		attachment, _ := item.(map[string]any)
		if attachment == nil {
			continue
		}
		fileURL, _ := attachment["url"].(string)
		name, _ := attachment["filename"].(string)
		if name == "" {
			name, _ = attachment["name"].(string)
		}
		name = core.TruncateRunes(strings.TrimSpace(name), 160, "…")
		mimeType, _ := attachment["content_type"].(string)
		mimeType = normalizeQQAttachmentMIME(mimeType)
		if mimeType == "" || mimeType == "application/octet-stream" || mimeType == "binary/octet-stream" {
			mimeType = normalizeQQAttachmentMIME(mime.TypeByExtension(filepath.Ext(name)))
		}
		kind := core.AttachmentFile
		if strings.HasPrefix(mimeType, "image/") {
			kind = core.AttachmentImage
		}
		if fileURL == "" || len(fileURL) > core.MaxAttachmentURLBytes {
			if kind == core.AttachmentImage {
				omittedImages++
			} else {
				omittedFiles++
			}
			continue
		}
		quoted := &core.Attachment{
			Kind: kind, URL: fileURL, MimeType: core.TruncateRunes(mimeType, 128, ""),
			Name: name, SourceMessageID: sourceMessageID, Quoted: true,
		}
		if kind == core.AttachmentImage {
			if !qqContainsImageURL(images, fileURL) {
				images = append(images, quoted)
			}
		} else {
			files = append(files, quoted)
		}
	}
	return images, files, omittedImages, omittedFiles
}

func normalizeQQAttachmentMIME(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(value, ";", 2)[0]))
}

func qqContainsImageURL(images []*core.Attachment, target string) bool {
	for _, image := range images {
		if image != nil && image.URL == target {
			return true
		}
	}
	return false
}

func qqQuotedContentKind(content string, images, files []*core.Attachment, omittedImages, omittedFiles int) string {
	hasImages := len(images)+omittedImages > 0
	hasFiles := len(files)+omittedFiles > 0
	hasText := content != "" && content != quoteAttachmentPlaceholder
	switch {
	case hasImages && hasFiles, (hasImages || hasFiles) && hasText:
		return qqQuoteMixed
	case hasImages:
		return qqQuoteImage
	case hasFiles:
		return qqQuoteFile
	default:
		return qqQuoteText
	}
}
