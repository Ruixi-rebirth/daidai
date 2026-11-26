package telegram

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	"daidai/internal/core"
	"daidai/internal/entity"
)

// A reference retains meaning independently of its transport representation.
// A rendered card and a photograph must not be treated as the same kind of image.
type quoteRecord struct {
	Content    string             `json:"content"`
	Kind       string             `json:"kind"`
	Complete   bool               `json:"complete"`
	SenderID   string             `json:"sender_id,omitempty"`
	SenderName string             `json:"sender_name,omitempty"`
	Images     []*core.Attachment `json:"images,omitempty"`
	Files      []*core.Attachment `json:"files,omitempty"`
	Reply      *quoteRecord       `json:"reply,omitempty"`
}

func (c *Channel) initQuotes(ctx context.Context) error {
	database, err := c.database.GetConn("telegram")
	if err != nil {
		return err
	}
	_, err = database.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS telegram_messages(chat_id TEXT NOT NULL,message_id TEXT NOT NULL,content_json TEXT NOT NULL,PRIMARY KEY(chat_id,message_id))`)
	return err
}

func recordFromEvent(e *entity.SensoryEvent, depth int) *quoteRecord {
	if e == nil || depth >= 4 {
		return nil
	}
	content := entity.SanitizeQuotedMessageContent(e.Content)
	if content == "" && len(e.Images)+len(e.Files) > 0 {
		content = "[引用消息包含附件，尚未读取]"
	}
	return &quoteRecord{Content: content, Kind: e.ContentKind, Complete: e.ContentComplete && len([]rune(e.Content)) <= core.MaxQuotedMessageRunes, SenderID: e.SenderID, SenderName: e.SenderName, Images: e.Images, Files: e.Files, Reply: recordFromEvent(e.ReplyContext, depth+1)}
}

func (r *quoteRecord) event(chat, id string, group bool, depth int) *entity.SensoryEvent {
	if r == nil || depth >= 4 {
		return nil
	}
	e := &entity.SensoryEvent{Platform: "telegram", ChannelID: chat, IsGroup: group, RawMessageID: id, Content: r.Content, ContentKind: r.Kind, ContentComplete: r.Complete, SenderID: r.SenderID, SenderName: r.SenderName, Images: r.Images, Files: r.Files}
	for _, a := range append(append([]*core.Attachment(nil), e.Images...), e.Files...) {
		if a != nil {
			a.Quoted = true
		}
	}
	e.ReplyContext = r.Reply.event(chat, "", group, depth+1)
	return e
}

func (c *Channel) storeQuote(ctx context.Context, chat string, id int64, r *quoteRecord) error {
	database, err := c.database.GetConn("telegram")
	if err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = database.ExecContext(ctx, `INSERT INTO telegram_messages(chat_id,message_id,content_json) VALUES(?,?,?) ON CONFLICT(chat_id,message_id) DO UPDATE SET content_json=excluded.content_json`, chat, strconv.FormatInt(id, 10), string(data))
	if err == nil && c.quoteWrites.Add(1)%64 == 0 {
		_, _ = database.ExecContext(ctx, `DELETE FROM telegram_messages WHERE rowid NOT IN (SELECT rowid FROM telegram_messages ORDER BY rowid DESC LIMIT 2000)`)
	}
	return err
}

func (c *Channel) resolveQuote(ctx context.Context, m *message, chat string, group bool, depth int) (*entity.SensoryEvent, error) {
	if m == nil || depth >= 4 {
		return nil, nil
	}
	database, err := c.database.GetConn("telegram")
	if err != nil {
		return nil, err
	}
	var encoded string
	err = database.QueryRowContext(ctx, `SELECT content_json FROM telegram_messages WHERE chat_id=? AND message_id=?`, chat, strconv.FormatInt(m.ID, 10)).Scan(&encoded)
	if err == nil {
		var r quoteRecord
		if err = json.Unmarshal([]byte(encoded), &r); err != nil {
			return nil, err
		}
		return r.event(chat, strconv.FormatInt(m.ID, 10), group, depth), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	images, files := attachments(m, true)
	content := m.Text
	if content == "" {
		content = m.Caption
	}
	if content == "" && len(images)+len(files) > 0 {
		content = "[引用消息包含附件，尚未读取]"
	}
	r := &quoteRecord{Content: entity.SanitizeQuotedMessageContent(content), Complete: len(images)+len(files) == 0, Kind: "text", Images: images, Files: files}
	if len(images)+len(files) > 0 {
		r.Kind = "media"
	}
	if m.From != nil {
		r.SenderID = "tg:" + strconv.FormatInt(m.From.ID, 10)
		r.SenderName = m.From.FirstName
	}
	e := r.event(chat, strconv.FormatInt(m.ID, 10), group, depth)
	e.ReplyContext, err = c.resolveQuote(ctx, m.Reply, chat, group, depth+1)
	return e, err
}
