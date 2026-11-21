package qq

import (
	"context"
	"fmt"
	"strings"

	"daidai/internal/entity"
)

func (c *Client) SendGroupMessage(ctx context.Context, groupOpenID string, content string, msgID string) error {
	return c.sendMessage(ctx, "groups", groupOpenID, content, msgID)
}

func (c *Client) SendGroupMessageWithContext(ctx context.Context, groupOpenID, content, msgID string, metadata QuoteMetadata) error {
	if metadata.Kind == "" {
		metadata.Kind = qqQuoteTextCard
	}
	return c.sendMessageWithContext(ctx, "groups", groupOpenID, content, msgID, metadata)
}

// SendC2CMessage 发送 QQ 私聊 Markdown 消息。
func (c *Client) SendC2CMessage(ctx context.Context, userOpenID string, content string, msgID string) error {
	return c.sendMessage(ctx, "users", userOpenID, content, msgID)
}

func (c *Client) SendC2CMessageWithContext(ctx context.Context, userOpenID, content, msgID string, metadata QuoteMetadata) error {
	if metadata.Kind == "" {
		metadata.Kind = qqQuoteTextCard
	}
	return c.sendMessageWithContext(ctx, "users", userOpenID, content, msgID, metadata)
}

func (c *Client) sendMessage(ctx context.Context, targetType, targetID, content, msgID string) error {
	return c.sendMessageWithContext(ctx, targetType, targetID, content, msgID, QuoteMetadata{Text: content, Kind: qqQuoteText, TextComplete: true})
}

func (c *Client) sendMessageWithContext(ctx context.Context, targetType, targetID, content, msgID string, metadata QuoteMetadata) error {
	url := fmt.Sprintf("%s/v2/%s/%s/messages", apiBaseURL, targetType, targetID)

	mdPayload := map[string]any{
		"msg_type": 2,
		"markdown": map[string]string{
			"content": content,
		},
		"msg_seq": c.nextSeq(),
	}
	if msgID != "" {
		mdPayload["msg_id"] = msgID
	}

	resp, err := c.doPostJSON(ctx, url, mdPayload)
	if err == nil {
		metadata.Text = strings.TrimSpace(metadata.Text)
		if metadata.Kind == "" {
			metadata.Kind = qqQuoteText
		}
		c.notifySent(resp, targetType, targetID, metadata)
		return nil
	}
	return fmt.Errorf("send QQ markdown message: %w", err)
}

// SendGroupMediaMessage 发送 QQ 群聊富媒体消息
func (c *Client) SendGroupMediaMessage(ctx context.Context, groupOpenID string, content string, imageData []byte, msgID string) error {
	return c.sendMediaMessage(ctx, "groups", groupOpenID, content, imageData, msgID, mediaQuoteMetadata(content))
}

// SendC2CMediaMessage 发送 QQ 私聊富媒体消息
func (c *Client) SendC2CMediaMessage(ctx context.Context, userOpenID string, content string, imageData []byte, msgID string) error {
	return c.sendMediaMessage(ctx, "users", userOpenID, content, imageData, msgID, mediaQuoteMetadata(content))
}

func mediaQuoteMetadata(content string) QuoteMetadata {
	kind := qqQuoteImage
	if strings.TrimSpace(content) != "" {
		kind = qqQuoteMixed
	}
	return QuoteMetadata{Text: content, Kind: kind}
}

// SendGroupMediaMessageWithContext associates the full text representation with
// the image message so a later quote can recover the card's content.
func (c *Client) SendGroupMediaMessageWithContext(ctx context.Context, groupOpenID, content string, imageData []byte, msgID string, metadata QuoteMetadata) error {
	if metadata.Kind == "" {
		metadata.Kind = qqQuoteMixed
	}
	return c.sendMediaMessage(ctx, "groups", groupOpenID, content, imageData, msgID, metadata)
}

// SendC2CMediaMessageWithContext associates the full text representation with
// the image message so a later quote can recover the card's content.
func (c *Client) SendC2CMediaMessageWithContext(ctx context.Context, userOpenID, content string, imageData []byte, msgID string, metadata QuoteMetadata) error {
	if metadata.Kind == "" {
		metadata.Kind = qqQuoteMixed
	}
	return c.sendMediaMessage(ctx, "users", userOpenID, content, imageData, msgID, metadata)
}

func (c *Client) sendMediaMessage(ctx context.Context, targetType, targetID, content string, imageData []byte, msgID string, metadata QuoteMetadata) error {
	if metadata.Kind == "" || metadata.Kind == qqQuoteText {
		metadata.Kind = qqQuoteImage
		if strings.TrimSpace(content) != "" {
			metadata.Kind = qqQuoteMixed
		}
	}
	fileInfo, err := c.UploadMedia(ctx, targetType, targetID, 1, imageData)
	if err != nil {
		return &entity.UnsentError{Err: fmt.Errorf("%s image upload error: %w", targetType, err)}
	}

	url := fmt.Sprintf("%s/v2/%s/%s/messages", apiBaseURL, targetType, targetID)
	payload := map[string]any{
		"msg_type": 7,
		// QQ v2 在富媒体消息中仍要求 content 字段，即使消息没有文字。
		"content": content,
		"media": map[string]string{
			"file_info": fileInfo,
		},
		"msg_seq": c.nextSeq(),
	}
	if content == "" {
		payload["content"] = " "
	}
	if msgID != "" {
		payload["msg_id"] = msgID
	}

	resp, err := c.doPostJSON(ctx, url, payload)
	if err == nil {
		metadata.Text = strings.TrimSpace(metadata.Text)
		if metadata.Text == "" && strings.TrimSpace(content) != "" {
			metadata.Text = content
		}
		if metadata.Kind == qqQuoteImage || metadata.Kind == qqQuoteFile {
			metadata.Text = ""
			metadata.TextComplete = false
		}
		c.notifySent(resp, targetType, targetID, metadata)
	}
	return err
}
