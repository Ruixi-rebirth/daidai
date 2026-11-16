package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
	"daidai/internal/kernel"
	"daidai/internal/media"
)

type Config struct {
	Token         string
	OwnerUsername string
}
type user struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	IsBot     bool   `json:"is_bot"`
}
type message struct {
	ThreadID int64 `json:"message_thread_id"`
	Quote    *struct {
		Text string `json:"text"`
	} `json:"quote"`
	ID   int64 `json:"message_id"`
	From *user `json:"from"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Text     string   `json:"text"`
	Caption  string   `json:"caption"`
	Reply    *message `json:"reply_to_message"`
	Photo    []file   `json:"photo"`
	Document *file    `json:"document"`
	Audio    *file    `json:"audio"`
	Voice    *file    `json:"voice"`
	Video    *file    `json:"video"`
	Date     int64    `json:"date"`
}
type file struct {
	ID   string `json:"file_id"`
	Name string `json:"file_name"`
	MIME string `json:"mime_type"`
	Size int64  `json:"file_size"`
}
type update struct {
	ID      int64    `json:"update_id"`
	Message *message `json:"message"`
}
type Channel struct {
	groups   map[string]*groupState
	stopping bool
	Inbox    *media.Inbox
	cfg      Config
	database *db.Manager
	http     *http.Client
	cancel   context.CancelFunc
	done     chan struct{}
	ready    atomic.Bool
	bot      user
	mu       sync.Mutex
	recorder func(context.Context, *entity.SensoryEvent) error
	immune   *entity.ImmuneSystem
	queue    *kernel.DebounceManager
}

func New(cfg Config, database *db.Manager, recorder func(context.Context, *entity.SensoryEvent) error, immune *entity.ImmuneSystem) *Channel {
	return &Channel{cfg: cfg, database: database, recorder: recorder, immune: immune, http: &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Channel) Name() string { return "telegram" }
func (c *Channel) Ready() bool  { return c.ready.Load() }
func (c *Channel) call(ctx context.Context, method string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+c.cfg.Token+"/"+method, body)
	if err != nil {
		return fmt.Errorf("telegram request construction failed")
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("telegram %s transport failed", method)
	}
	defer func() { _ = resp.Body.Close() }()
	var envelope struct {
		OK         bool            `json:"ok"`
		Result     json.RawMessage `json:"result"`
		Code       int             `json:"error_code"`
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("telegram %s response could not be decoded", method)
	}
	if !envelope.OK {
		err := &apiError{method: method, code: envelope.Code, status: resp.StatusCode}
		if envelope.Code >= 400 && envelope.Code < 500 {
			delay := time.Duration(0)
			if envelope.Code == 429 {
				delay = max(time.Duration(envelope.Parameters.RetryAfter)*time.Second, time.Minute)
			}
			return &entity.DeliveryRejection{Err: err, After: delay}
		}
		return err
	}
	if out != nil {
		return json.Unmarshal(envelope.Result, out)
	}
	return nil
}
func (c *Channel) jsonCall(ctx context.Context, method string, payload any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.call(ctx, method, bytes.NewReader(b), "application/json", out)
}
func (c *Channel) Start(ctx context.Context, handler func(*entity.SensoryEvent)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		return fmt.Errorf("telegram already started")
	}
	if err := c.jsonCall(ctx, "getMe", map[string]any{}, &c.bot); err != nil {
		return err
	}
	var webhook struct {
		URL string `json:"url"`
	}
	if err := c.jsonCall(ctx, "getWebhookInfo", map[string]any{}, &webhook); err != nil {
		return err
	}
	if webhook.URL != "" {
		return fmt.Errorf("telegram bot has an existing webhook; polling cannot start")
	}
	if err := c.jsonCall(ctx, "setMyCommands", map[string]any{"commands": entity.CommandMenu("telegram")}, nil); err != nil {
		return fmt.Errorf("register Telegram command menu: %w", err)
	}
	if err := c.initQuotes(ctx); err != nil {
		return err
	}
	pollCtx, cancel := context.WithCancel(ctx)
	c.queue = kernel.NewDebounceManager(kernel.DebounceConfig{MergeWindow: 2 * time.Second}, handler)
	c.cancel = cancel
	c.stopping = false
	c.done = make(chan struct{})
	go c.poll(pollCtx, handler)
	return nil
}
func (c *Channel) poll(ctx context.Context, handler func(*entity.SensoryEvent)) {
	defer close(c.done)
	defer c.ready.Store(false)
	saved, _ := c.database.GetRuntimeState(ctx, "telegram", "update_offset")
	offset, _ := strconv.ParseInt(saved, 10, 64)
	for ctx.Err() == nil {
		var updates []update
		err := c.jsonCall(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 25, "allowed_updates": []string{"message"}}, &updates)
		if err != nil {
			c.ready.Store(false)
			if ctx.Err() != nil {
				return
			}
			slog.Warn("telegram: polling failed", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		c.ready.Store(true)
		for _, u := range updates {
			if ctx.Err() != nil {
				return
			}
			if err := c.handle(ctx, u.Message, handler); err != nil {
				slog.Warn("telegram: update handling failed", "updateID", u.ID, "err", err)
				var apiErr *apiError
				if !errors.As(err, &apiErr) || (apiErr.code != 400 && apiErr.code != 403) {
					select {
					case <-ctx.Done():
						return
					case <-time.After(3 * time.Second):
					}
					break
				}
			}
			offset = u.ID + 1
			if err := c.database.SetRuntimeState(ctx, "telegram", "update_offset", strconv.FormatInt(offset, 10)); err != nil {
				slog.Warn("telegram: offset persistence failed", "err", err)
			}
		}
	}
}
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
	addressed := !group || strings.Contains(strings.ToLower(body), "@"+strings.ToLower(c.bot.Username)) || strings.Contains(body, "呆呆") || (m.Reply != nil && m.Reply.From != nil && m.Reply.From.ID == c.bot.ID)
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
	if c.Inbox != nil && addressed {
		scope := media.AttachmentScope(event.Platform, group, chat, event.SenderID)
		event.PendingImages, event.OmittedPendingImages = c.Inbox.PendingKind(scope, core.AttachmentImage)
		event.PendingFiles, event.OmittedPendingFiles = c.Inbox.PendingKind(scope, core.AttachmentFile)
		c.Inbox.RememberPending(scope, currentAttachments)
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
	if source == "" {
		source = text.Text
	}
	c.remember(ctx, chat, sent.ID, source)
	return nil
}
func (c *Channel) Stop(ctx context.Context) error {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.stopping = true
	for _, group := range c.groups {
		if group.timer != nil {
			group.timer.Stop()
		}
	}
	c.groups = nil
	c.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		if c.queue != nil {
			if err := c.queue.Shutdown(ctx); err != nil {
				return err
			}
		}
		c.mu.Lock()
		c.cancel = nil
		c.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type apiError struct {
	method       string
	code, status int
}

func (e *apiError) Error() string {
	return fmt.Sprintf("telegram %s rejected (status %d, code %d)", e.method, e.status, e.code)
}

// VerifyOwnerPrivate resolves the existing address and rechecks the configured
// username. The numeric address is never an authorization binding.
func (c *Channel) VerifyOwnerPrivate(ctx context.Context, address string) bool {
	if c.cfg.OwnerUsername == "" || strings.Contains(address, "/") {
		return false
	}
	var chat struct {
		ID             int64 `json:"id"`
		Type, Username string
	}
	if c.jsonCall(ctx, "getChat", map[string]string{"chat_id": address}, &chat) != nil {
		return false
	}
	return chat.Type == "private" && strconv.FormatInt(chat.ID, 10) == address && strings.EqualFold(chat.Username, strings.TrimPrefix(strings.TrimSpace(c.cfg.OwnerUsername), "@"))
}
