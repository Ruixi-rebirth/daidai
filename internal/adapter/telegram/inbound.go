package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/media"
)

func (c *Channel) handle(ctx context.Context, m *message, handler func(*entity.SensoryEvent)) error {
	if m == nil || m.From == nil || m.From.IsBot {
		return nil
	}
	id := strconv.FormatInt(m.From.ID, 10)
	chat := strconv.FormatInt(m.Chat.ID, 10)
	if m.ThreadID > 0 {
		chat += "/" + strconv.FormatInt(m.ThreadID, 10)
	}
	ownerUsername := strings.TrimPrefix(strings.TrimSpace(c.cfg.OwnerUsername), "@")
	isOwner := ownerUsername != "" && m.From.Username != "" && strings.EqualFold(m.From.Username, ownerUsername)
	if c.immune != nil {
		c.immune.SetPartner("tg:"+id, isOwner)
	}
	if !isOwner && m.Chat.Type == "private" {
		username := "未设置"
		if m.From.Username != "" {
			username = "@" + m.From.Username
		}
		slog.Info("telegram: private access denied", "userID", id, "username", m.From.Username)
		return c.sendText(ctx, chat, fmt.Sprintf("暂未开通私聊权限。\n你的 Telegram 用户名：%s\n用户 ID：%s\n请将这些账号信息提供给管理员核对授权。", username, id), m.ID, "")
	}
	body := strings.TrimSpace(m.Text)
	if body == "" {
		body = strings.TrimSpace(m.Caption)
	}
	group := m.Chat.Type != "private"
	addressed := !group || strings.Contains(strings.ToLower(body), "@"+strings.ToLower(c.bot.Username)) || strings.Contains(body, config.BotName()) || (m.Reply != nil && m.Reply.From != nil && m.Reply.From.ID == c.bot.ID)
	// Normalize /command@Bot without stripping suffixes intended for another bot.
	if strings.HasPrefix(body, "/") {
		command, rest, _ := strings.Cut(body, " ")
		if name, target, qualified := strings.Cut(command, "@"); qualified {
			if !strings.EqualFold(target, c.bot.Username) {
				return nil
			}
			body = strings.TrimSpace(name + " " + rest)
			addressed = true
		} else {
			addressed = true
		}
	}
	body = strings.TrimSpace(body)
	if body == "/start" && addressed {
		return c.sendText(ctx, chat, "呆呆已连接。直接和我聊天，或发送 /help 查看命令。", m.ID, "")
	}
	role := entity.RoleGuest
	if isOwner {
		role = entity.RolePartner
	}
	event := &entity.SensoryEvent{Platform: "telegram", ChannelID: chat, IsGroup: group, SenderID: "tg:" + id, SenderName: m.From.FirstName, Role: role, Content: body, RawMessageID: strconv.FormatInt(m.ID, 10), OccurredAt: time.Now(), GroupMessageObserved: !addressed}
	if m.Date > 0 {
		event.OccurredAt = time.Unix(m.Date, 0)
	}
	event.Images, event.Files = attachments(m, false)
	event.ContentKind = "text"
	event.ContentComplete = true
	if len(event.Images)+len(event.Files) > 0 {
		event.ContentKind = "media"
		event.ContentComplete = false
		event.HistoryAttachmentNote = "[附件尚未读取]"
	}
	if m.Reply != nil {
		var err error
		event.ReplyContext, err = c.resolveQuote(ctx, m.Reply, chat, group, 0)
		if err != nil {
			return err
		}
		if m.Quote != nil && strings.TrimSpace(m.Quote.Text) != "" {
			event.ReplyContext.Content = "[用户选中的引用片段]\n" + entity.SanitizeQuotedMessageContent(m.Quote.Text)
			event.ReplyContext.ContentComplete = false
		}
	}
	// Persist the message's own attachments and its reference separately. A
	// flattened processing list must never become the next stored quote node.
	inboundRecord := recordFromEvent(event, 0)
	currentAttachments := append(append([]*core.Attachment(nil), event.Images...), event.Files...)
	for quoted := event.ReplyContext; quoted != nil; quoted = quoted.ReplyContext {
		for _, attachment := range quoted.Files {
			if attachment == nil {
				continue
			}
			exists := false
			for _, current := range event.Files {
				if current != nil && current.URL == attachment.URL && current.SourceMessageID == attachment.SourceMessageID {
					exists = true
					break
				}
			}
			if !exists {
				event.Files = append(event.Files, attachment)
			}
		}
	}
	if c.Inbox != nil {
		scope := media.AttachmentScope(event.Platform, group, chat, event.SenderID)
		if addressed {
			event.PendingImages, event.OmittedPendingImages = c.Inbox.PendingKind(scope, core.AttachmentImage)
			event.PendingFiles, event.OmittedPendingFiles = c.Inbox.PendingKind(scope, core.AttachmentFile)
			c.Inbox.RememberPending(scope, currentAttachments)
		} else if len(event.Files) > 0 {
			c.Inbox.RememberPending(scope, event.Files)
		}
	}
	if body == "" && len(event.Images) == 0 && len(event.Files) == 0 {
		return nil
	}
	if c.recorder != nil {
		if err := c.recorder(ctx, event); err != nil {
			return err
		}
	}
	if !entity.IsSensitiveAuthCommand(body) {
		if err := c.storeQuote(ctx, chat, m.ID, inboundRecord); err != nil {
			return err
		}
	}
	if group {
		c.observeGroup(event, m.Reply != nil && m.Reply.From != nil && m.Reply.From.IsBot)
	}
	if c.queue != nil {
		if !c.queue.Push(event) {
			return fmt.Errorf("telegram conversation queue rejected update")
		}
	} else {
		handler(event)
	}
	return nil
}
