package telegram

import (
	"context"
	"fmt"
	"log/slog"

	"daidai/internal/entity"
)

func (c *Channel) remember(ctx context.Context, chat string, id int64, body string) {
	if body == "" {
		return
	}
	err := c.storeQuote(ctx, chat, id, &quoteRecord{Content: body, Kind: "text", Complete: true, SenderName: c.bot.FirstName})
	if err != nil {
		slog.Warn("telegram: reply source persistence failed", "err", err)
	}
}
func (c *Channel) sendText(ctx context.Context, chat, text string, reply int64, source string) error {
	if source == "" {
		source = text
	}
	return c.sendFormattedText(ctx, chat, formatMarkdown(text), reply, source)
}
func (c *Channel) sendFormattedText(ctx context.Context, chat string, text formattedText, reply int64, source string) error {
	payload, err := destination(chat)
	if err != nil {
		return err
	}
	payload["text"] = text.Text
	if len(text.Entities) > 0 {
		payload["entities"] = text.Entities
	}
	if reply > 0 {
		payload["reply_parameters"] = map[string]any{"message_id": reply, "allow_sending_without_reply": true}
	}
	var sent message
	if err := c.jsonCall(ctx, "sendMessage", payload, &sent); err != nil {
		return err
	}
	if sent.ID <= 0 {
		return &entity.PartialDeliveryError{Err: fmt.Errorf("telegram text receipt has no message ID")}
	}
	if source == "" {
		source = text.Text
	}
	c.remember(ctx, chat, sent.ID, source)
	return nil
}
