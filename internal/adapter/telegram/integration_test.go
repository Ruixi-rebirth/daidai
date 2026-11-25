package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
	"daidai/internal/kernel"
	"daidai/internal/media"
)

func testChannel(t *testing.T) *Channel {
	t.Helper()
	database := db.NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	c := New(Config{OwnerUsername: "owner", Token: "private-token"}, database, nil, entity.NewImmuneSystem(""))
	c.bot = user{ID: 100, Username: "TestBot", FirstName: "呆呆"}
	if err := c.initQuotes(context.Background()); err != nil {
		t.Fatal(err)
	}
	inbox, err := media.NewInbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.Inbox = inbox
	return c
}
func response(value string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(value)), Header: make(http.Header)}
}
func incoming(id int64, text string) *message {
	m := &message{ID: id, From: &user{ID: 42, Username: "owner", FirstName: "Owner"}, Text: text}
	m.Chat.ID = 42
	m.Chat.Type = "private"
	return m
}
func TestAttachmentsAreDeferredAndQuotesRetainMeaning(t *testing.T) {
	c := testChannel(t)
	c.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("metadata handling downloaded an attachment")
		return nil, nil
	})
	m := incoming(1, "")
	m.Photo = []file{{ID: "small"}, {ID: "large"}}
	var event *entity.SensoryEvent
	handle := func(e *entity.SensoryEvent) { event = e }
	if err := c.handle(context.Background(), m, handle); err != nil {
		t.Fatal(err)
	}
	if len(event.Images) != 1 || event.Images[0].URL != "telegram-file:large" {
		t.Fatalf("bad image metadata: %+v", event)
	}
	scope := media.AttachmentScope("telegram", false, "42", "tg:42")
	pending, _ := c.Inbox.PendingKind(scope, core.AttachmentImage)
	if len(pending) != 1 {
		t.Fatal("attachment was not retained for confirmation")
	}
	next := incoming(2, "描述这张图")
	next.Reply = m
	if err := c.handle(context.Background(), next, handle); err != nil {
		t.Fatal(err)
	}
	if event.ReplyContext == nil || event.ReplyContext.Content == "" || len(event.ReplyContext.Images) != 1 || event.ReplyContext.ContentComplete {
		t.Fatalf("pure photo quote lost its meaning: %+v", event.ReplyContext)
	}
	card := &quoteRecord{Content: "完整卡片正文", Kind: "text_card", Complete: true}
	if err := c.storeQuote(context.Background(), "42", 50, card); err != nil {
		t.Fatal(err)
	}
	next = incoming(3, "解释第二点")
	next.Reply = &message{ID: 50, Photo: []file{{ID: "rendered"}}}
	if err := c.handle(context.Background(), next, handle); err != nil {
		t.Fatal(err)
	}
	if event.ReplyContext.Content != "完整卡片正文" || !event.ReplyContext.ContentComplete || len(event.ReplyContext.Images) != 0 {
		t.Fatal("card source was not restored")
	}
	nested := incoming(4, "刚才的解释是什么意思")
	nested.Reply = &message{ID: 3, Text: "解释第二点"}
	if err := c.handle(context.Background(), nested, handle); err != nil {
		t.Fatal(err)
	}
	if event.ReplyContext.ReplyContext == nil || event.ReplyContext.ReplyContext.Content != "完整卡片正文" {
		t.Fatal("nested quote lost")
	}
	foreign, err := c.resolveQuote(context.Background(), next.Reply, "99", false, 0)
	if err != nil || foreign.Content == "完整卡片正文" {
		t.Fatal("quote escaped its chat scope")
	}
}

func TestGroupAccessAndCommandTargets(t *testing.T) {
	c := testChannel(t)
	var events []*entity.SensoryEvent
	handle := func(e *entity.SensoryEvent) { events = append(events, e) }
	m := incoming(1, "普通发言")
	m.Chat.Type = "supergroup"
	m.Chat.ID = -100
	m.From.ID = 9
	m.From.Username = "group_member"
	m.ThreadID = 7
	if err := c.handle(context.Background(), m, handle); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !events[0].GroupMessageObserved || events[0].Role != entity.RoleGuest || events[0].ChannelID != "-100/7" {
		t.Fatal("ordinary group message was not isolated and recorded")
	}
	m.ID = 2
	m.Text = "/help@TestBot"
	if err := c.handle(context.Background(), m, handle); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Content != "/help" || events[1].GroupMessageObserved {
		t.Fatal("qualified slash command was not routed")
	}
	m.ID = 3
	m.Text = "/help@OtherBot"
	if err := c.handle(context.Background(), m, handle); err != nil {
		t.Fatal(err)
	}
	// Replying to another human while casually mentioning BotName must stay observed (not addressed).
	m.ID = 4
	m.Text = "你看呆呆刚才发的图"
	m.Reply = &message{ID: 1, Text: "普通发言", From: &user{ID: 88, FirstName: "OtherHuman"}}
	if err := c.handle(context.Background(), m, handle); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || !events[2].GroupMessageObserved {
		t.Fatalf("replying to another human while mentioning bot name should not hijack conversation: %+v", events)
	}
	m.Reply = nil
	m.From.IsBot = true
	m.Text = "呆呆你好"
	if err := c.handle(context.Background(), m, handle); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatal("responded to another bot or its command")
	}
}

func TestFileResolutionIsBoundedAndDoesNotLeakToken(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		size, limit      int64
		wantErr          bool
	}{
		{"valid", "documents/test.txt", "hello", 5, 10, false},
		{"declared oversize", "documents/test.txt", "hello", 100, 10, true},
		{"stream oversize", "documents/test.txt", "too much data", 0, 4, true},
		{"traversal", "../token", "", 0, 10, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testChannel(t)
			c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/getFile") {
					return response(fmt.Sprintf(`{"ok":true,"result":{"file_path":%q,"file_size":%d}}`, tc.path, tc.size)), nil
				}
				if !strings.HasPrefix(r.URL.Path, "/file/botprivate-token/documents/") {
					t.Fatalf("unsafe file URL: %s", r.URL.Path)
				}
				return response(tc.body), nil
			})
			data, err := c.FetchFile(context.Background(), "telegram-file:opaque-id", tc.limit)
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected result: %s %v", data, err)
			}
			if err != nil && strings.Contains(err.Error(), c.cfg.Token) {
				t.Fatal("token leaked")
			}
		})
	}
}

func TestAlbumReceiptsRestoreRenderedTextAndRealImages(t *testing.T) {
	c := testChannel(t)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 32, 32))); err != nil {
		t.Fatal(err)
	}
	calls := 0
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if !strings.HasSuffix(r.URL.Path, "/sendMediaGroup") {
			t.Fatalf("expected native album, got %s", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := r.MultipartForm.RemoveAll(); err != nil {
				t.Error(err)
			}
		}()
		if r.FormValue("chat_id") != "42" || r.FormValue("message_thread_id") != "7" {
			t.Fatal("topic destination lost")
		}
		var items []map[string]any
		if err := json.Unmarshal([]byte(r.FormValue("media")), &items); err != nil {
			t.Fatal(err)
		}
		if len(items) != 2 || items[0]["caption"] != "这是图片" {
			t.Fatal("album caption or count incorrect")
		}
		if _, ok := items[0]["caption_entities"]; !ok {
			t.Fatal("album caption formatting lost")
		}
		return response(`{"ok":true,"result":[{"message_id":10,"photo":[{"file_id":"card"}]},{"message_id":11,"photo":[{"file_id":"real"}]}]}`), nil
	})
	u := &entity.Utterance{ChannelID: "42/7", QuoteMsgID: "2", Text: "**这是图片**", Images: [][]byte{encoded.Bytes(), encoded.Bytes()}, ImageSources: []string{"卡片原文", ""}}
	if err := c.Transmit(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("caption sent as redundant separate message")
	}
	card, err := c.resolveQuote(context.Background(), &message{ID: 10}, "42/7", false, 0)
	if err != nil || card.Content != "卡片原文" || !card.ContentComplete || card.ContentKind != "text_card" {
		t.Fatal("card receipt lost source")
	}
	real, err := c.resolveQuote(context.Background(), &message{ID: 11}, "42/7", false, 0)
	if err != nil || real.ContentComplete || len(real.Images) != 1 || real.Images[0].URL != "telegram-file:real" {
		t.Fatal("real image incorrectly treated as text card")
	}
}

func TestPollingPersistsOffsetAndStops(t *testing.T) {
	c := testChannel(t)
	polled := make(chan int64, 2)
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			return response(`{"ok":true,"result":{"id":100,"username":"TestBot"}}`), nil
		case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
			return response(`{"ok":true,"result":{"url":""}}`), nil
		case strings.HasSuffix(r.URL.Path, "/setMyCommands"):
			return response(`{"ok":true,"result":true}`), nil
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			var payload struct {
				Offset int64 `json:"offset"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			polled <- payload.Offset
			if payload.Offset == 0 {
				return response(`{"ok":true,"result":[{"update_id":50,"message":{"message_id":1,"from":{"id":42,"username":"owner"},"chat":{"id":42,"type":"private"},"text":"/help"}}]}`), nil
			}
			<-r.Context().Done()
			return nil, r.Context().Err()
		default:
			return nil, fmt.Errorf("unexpected request")
		}
	})
	handled := make(chan struct{}, 1)
	if err := c.Start(context.Background(), func(*entity.SensoryEvent) { handled <- struct{}{} }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cleanupCancel()
		if err := c.Stop(cleanupCtx); err != nil {
			t.Error(err)
		}
	})
	select {
	case <-handled:
	case <-ctx.Done():
		t.Fatal("poll did not dispatch command")
	}
	<-polled
	select {
	case offset := <-polled:
		if offset != 51 {
			t.Fatalf("wrong next offset %d", offset)
		}
	case <-ctx.Done():
		t.Fatal("poll did not advance")
	}
	saved, err := c.database.GetRuntimeState(context.Background(), "telegram", "update_offset")
	if err != nil || saved != "51" {
		t.Fatalf("offset not persisted: %s %v", saved, err)
	}
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if c.Ready() {
		t.Fatal("stopped channel reports ready")
	}
}

func TestStopWaitsForActiveHandler(t *testing.T) {
	c := testChannel(t)
	started, release := make(chan struct{}), make(chan struct{})
	c.queue = kernel.NewDebounceManager(kernel.DebounceConfig{}, func(*entity.SensoryEvent) { close(started); <-release })
	c.cancel = func() {}
	c.done = make(chan struct{})
	close(c.done)
	c.queue.Push(&entity.SensoryEvent{Platform: "telegram", ChannelID: "42", Content: "/help"})
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := c.Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop returned before handler finished: %v", err)
	}
	close(release)
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReferencedFilesDoNotAccumulateOrBecomePendingUploads(t *testing.T) {
	c := testChannel(t)
	original := incoming(1, "")
	original.Chat.ID = -100
	original.Chat.Type = "group"
	original.From = &user{ID: 9, Username: "member"}
	original.Document = &file{ID: "shared-file", Name: "report.txt"}
	var event *entity.SensoryEvent
	handle := func(e *entity.SensoryEvent) { event = e }
	if err := c.handle(context.Background(), original, handle); err != nil {
		t.Fatal(err)
	}
	previous := original
	for id := int64(2); id <= 4; id++ {
		next := incoming(id, "呆呆总结引用文件")
		next.Chat.ID = -100
		next.Chat.Type = "group"
		next.Reply = previous
		if err := c.handle(context.Background(), next, handle); err != nil {
			t.Fatal(err)
		}
		if len(event.Files) != 1 || !event.Files[0].Quoted {
			t.Fatalf("reference files accumulated: %+v", event.Files)
		}
		record, err := c.resolveQuote(context.Background(), &message{ID: id}, "-100", true, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(record.Files) != 0 {
			t.Fatal("quoted file persisted as a current upload")
		}
		scope := media.AttachmentScope("telegram", true, "-100", "tg:42")
		pending, _ := c.Inbox.PendingKind(scope, core.AttachmentFile)
		if len(pending) != 0 {
			t.Fatal("someone else's quoted file entered requester's pending uploads")
		}
		previous = &message{ID: id, Text: next.Text, From: next.From}
	}
}
