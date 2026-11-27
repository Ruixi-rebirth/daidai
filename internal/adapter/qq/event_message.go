package qq

import (
	"strings"

	"daidai/internal/core"
)

type qqInboundMessage struct {
	Data           map[string]any
	EventType      string
	ConversationID string
	UserID         string
	SenderName     string
	Content        string
	MessageID      string
	IsGroup        bool
	BotMentioned   bool
}

func parseQQInboundMessage(payload map[string]any) (qqInboundMessage, bool) {
	d, ok := payload["d"].(map[string]any)
	if !ok {
		return qqInboundMessage{}, false
	}
	eventType, _ := payload["t"].(string)

	var (
		convID       string
		userID       string
		senderName   string
		content      string
		msgID        string
		isGroup      bool
		botMentioned bool
	)

	msgID, _ = d["id"].(string)
	rawContent, _ := d["content"].(string)
	if qqMessageElementType(d) == 102 {
		if subElements, ok := d["msg_elements"].([]any); ok && len(subElements) > 0 {
			historyText := extractQQChatHistoryText(subElements)
			if historyText != "" {
				title, _ := d["title"].(string)
				title = strings.TrimSpace(title)
				if title != "" && !strings.HasPrefix(title, "[") {
					title = "[" + title + "]"
				}
				if title != "" {
					historyText = title + "\n" + historyText
				}
				if rawContent == "" || rawContent == "[聊天记录]" {
					rawContent = historyText
				} else {
					rawContent = rawContent + "\n\n" + historyText
				}
			}
		}
	}
	content = resolveQQMentions(rawContent, d)

	switch eventType {
	case "GROUP_AT_MESSAGE_CREATE": // 群聊中被 @ 的消息
		isGroup = true
		if mentions, ok := d["mentions"].([]any); ok && len(mentions) > 0 {
			botMentioned = qqMessageMentionsBot(d)
		} else {
			botMentioned = true
		}
		convID, _ = d["group_openid"].(string)
		if author, ok := d["author"].(map[string]any); ok {
			userID, _ = author["member_openid"].(string)
			senderName, _ = author["username"].(string)
		}
	case "GROUP_MESSAGE_CREATE": // 群聊全量消息；仅唤醒 @ 机器人的消息
		isGroup = true
		botMentioned = qqMessageMentionsBot(d)
		convID, _ = d["group_openid"].(string)
		if author, ok := d["author"].(map[string]any); ok {
			userID, _ = author["member_openid"].(string)
			senderName, _ = author["username"].(string)
		}

	case "C2C_MESSAGE_CREATE": // 好友私聊单聊消息
		isGroup = false
		if author, ok := d["author"].(map[string]any); ok {
			userID, _ = author["user_openid"].(string)
			senderName, _ = author["username"].(string)
		}
		convID = userID

	default:
		return qqInboundMessage{}, false
	}
	senderName = core.NormalizeDisplayName(senderName, userID)
	return qqInboundMessage{
		Data:           d,
		EventType:      eventType,
		ConversationID: convID,
		UserID:         userID,
		SenderName:     senderName,
		Content:        content,
		MessageID:      msgID,
		IsGroup:        isGroup,
		BotMentioned:   botMentioned,
	}, true
}
