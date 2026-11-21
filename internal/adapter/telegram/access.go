package telegram

import (
	"context"
	"strconv"
	"strings"
)

func (c *Channel) VerifyOwnerPrivate(ctx context.Context, address string) (bool, error) {
	if c.cfg.OwnerUsername == "" || strings.Contains(address, "/") {
		return false, nil
	}
	var chat struct {
		ID             int64 `json:"id"`
		Type, Username string
	}
	if err := c.jsonCall(ctx, "getChat", map[string]string{"chat_id": address}, &chat); err != nil {
		return false, err
	}
	return chat.Type == "private" && strconv.FormatInt(chat.ID, 10) == address && strings.EqualFold(chat.Username, strings.TrimPrefix(strings.TrimSpace(c.cfg.OwnerUsername), "@")), nil
}

// VerifyGroup confirms that an explicitly subscribed destination is a group.
// Topic suffixes are preserved for sending, while getChat addresses the group.
func (c *Channel) VerifyGroup(ctx context.Context, address string) (bool, error) {
	target, err := destination(address)
	if err != nil {
		return false, nil
	}
	chatID := target["chat_id"].(int64)
	var chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	}
	if err := c.jsonCall(ctx, "getChat", map[string]int64{"chat_id": chatID}, &chat); err != nil {
		return false, err
	}
	return chat.ID == chatID && (chat.Type == "group" || chat.Type == "supergroup"), nil
}
