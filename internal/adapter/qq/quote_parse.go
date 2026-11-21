package qq

import (
	"fmt"
	"net/url"
	"strings"

	"daidai/internal/core"
	"daidai/internal/entity"
)

const maxQQQuoteDepth = 4

func qqQuoteTextComplete(content string) bool {
	for _, marker := range []string{
		"…[消息已截断]",
		"…[引用正文已截断]",
		"\n[消息过长，后续内容已截断]",
		"\n[引用消息过长，后续内容已截断]",
		"\n[视觉摘要过长，后续内容已截断]",
		"\n[原消息过长，已截短以保留图片内容]",
		"\n[原消息过长，已截短以保留附件信息]",
	} {
		if strings.Contains(content, marker) {
			return false
		}
	}
	return true
}

func qqMessageElementType(element map[string]any) int {
	switch value := element["message_type"].(type) {
	case float64:
		return int(value)
	case int:
		return value
	default:
		return 0
	}
}

func qqQuotedImageURLs(content string) []string {
	var imageURLs []string
	seen := make(map[string]struct{})
	for remaining := content; ; {
		start := strings.Index(remaining, "https://")
		if start < 0 {
			break
		}
		remaining = remaining[start:]
		end := strings.IndexAny(remaining, " \t\r\n<>\"'")
		candidate := remaining
		if end >= 0 {
			candidate = remaining[:end]
		}
		candidate = strings.TrimRight(candidate, ").,;:!?]}`")
		if _, err := url.ParseRequestURI(candidate); err == nil && core.ValidateTrustedMediaURL(candidate) == nil {
			if _, exists := seen[candidate]; !exists {
				seen[candidate] = struct{}{}
				imageURLs = append(imageURLs, candidate)
			}
		}
		if end < 0 {
			break
		}
		remaining = remaining[end:]
		if remaining == "" {
			break
		}
		remaining = remaining[1:]
	}
	return imageURLs
}

func qqReferencedMessageIndex(message map[string]any) string {
	scene, _ := message["message_scene"].(map[string]any)
	extensions, _ := scene["ext"].([]any)
	for _, extension := range extensions {
		value, _ := extension.(string)
		const prefix = "ref_msg_idx="
		if strings.HasPrefix(value, prefix) {
			return strings.TrimPrefix(value, prefix)
		}
	}
	return ""
}

func qqReferenceCacheKey(message map[string]any) string {
	if index := qqReferencedMessageIndex(message); index != "" {
		return index
	}
	if reference, ok := message["message_reference"].(map[string]any); ok {
		if index, _ := reference["message_id"].(string); index != "" {
			return index
		}
	}
	if quote := qqReferencedMessageElement(message); quote != nil {
		if index, _ := quote["msg_idx"].(string); index != "" {
			return index
		}
		if id, _ := quote["id"].(string); id != "" {
			return id
		}
	}
	return ""
}

// qqReferencedMessageElement uses QQ's ref_msg_idx when available. A single
// quote element is a safe fallback; choosing the first type-103 element is not,
// because forwarded/parallel messages can also contain multiple elements.
func qqReferencedMessageElement(message map[string]any) map[string]any {
	elements, _ := message["msg_elements"].([]any)
	if len(elements) == 0 {
		return nil
	}
	refIndex := qqReferencedMessageIndex(message)
	var onlyQuote map[string]any
	quoteCount := 0
	for _, value := range elements {
		element, _ := value.(map[string]any)
		if element == nil {
			continue
		}
		mType := qqMessageElementType(element)
		if mType != 103 && mType != 102 {
			continue
		}
		quoteCount++
		onlyQuote = element
		index, _ := element["msg_idx"].(string)
		if refIndex != "" && index == refIndex {
			return element
		}
	}
	if quoteCount == 1 {
		return onlyQuote
	}
	return nil
}

func qqQuoteElementCount(message map[string]any) int {
	elements, _ := message["msg_elements"].([]any)
	count := 0
	for _, value := range elements {
		element, _ := value.(map[string]any)
		if element != nil {
			mType := qqMessageElementType(element)
			if mType == 103 || mType == 102 {
				count++
			}
		}
	}
	return count
}

// extractQQChatHistoryText 从 QQ 官方 102 消息（聊天记录/合并转发）中递归提取多条对话文本与作者
func extractQQChatHistoryText(elements []any) string {
	return extractQQChatHistoryTextAtDepth(elements, 0)
}

func extractQQChatHistoryTextAtDepth(elements []any, depth int) string {
	if len(elements) == 0 {
		return ""
	}
	if depth >= maxQQQuoteDepth {
		return "[嵌套聊天记录超过读取层级]"
	}
	var sb strings.Builder
	idx := 1
	for _, item := range elements {
		elem, ok := item.(map[string]any)
		if !ok || elem == nil {
			continue
		}
		text, _ := elem["content"].(string)
		text = strings.TrimSpace(text)
		author := ""
		if a, ok := elem["author"].(map[string]any); ok {
			author, _ = a["username"].(string)
		}
		if author == "" {
			author, _ = elem["name"].(string)
		}
		if author == "" {
			author, _ = elem["sender_name"].(string)
		}
		author = strings.TrimSpace(author)

		if text == "" {
			if atts, ok := elem["attachments"].([]any); ok && len(atts) > 0 {
				text = "[包含图片或文件附件]"
			}
		}

		if sub, ok := elem["msg_elements"].([]any); ok && len(sub) > 0 {
			subText := extractQQChatHistoryTextAtDepth(sub, depth+1)
			if subText != "" {
				if text != "" {
					text += "\n" + subText
				} else {
					text = subText
				}
			}
		}
		if text == "" && author == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		fmt.Fprintf(&sb, "=== 消息 %d ===\n", idx)
		if text != "" {
			fmt.Fprintf(&sb, "[消息内容] %s\n", text)
		}
		if author != "" {
			fmt.Fprintf(&sb, "[发送者] %s", author)
		}
		idx++
	}
	return sb.String()
}

func qqQuotedFiles(event *entity.SensoryEvent) ([]*core.Attachment, int) {
	var files []*core.Attachment
	seen := make(map[string]struct{})
	omitted := 0
	for current, depth := event, 0; current != nil && depth < 4; current, depth = current.ReplyContext, depth+1 {
		for _, attachment := range current.Files {
			if attachment == nil || attachment.URL == "" {
				continue
			}
			if _, exists := seen[attachment.URL]; exists {
				continue
			}
			seen[attachment.URL] = struct{}{}
			if len(files) < core.MaxIncomingFilesPerRequest {
				files = append(files, attachment)
			} else {
				omitted++
			}
		}
	}
	return files, omitted
}
