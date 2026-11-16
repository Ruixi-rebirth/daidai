package qq

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
)

type qqTestTransport func(*http.Request) (*http.Response, error)

func (f qqTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestQQMediaQuoteCompletenessAfterLongReport(t *testing.T) {
	for _, group := range []bool{false, true} {
		for _, length := range []int{100, core.MaxUserMessageRunes + 100} {
			cache := newMsgCache(10, nil)
			client := NewClient("test", "test")
			client.httpClient.Transport = qqTestTransport(func(req *http.Request) (*http.Response, error) {
				body := `{"id":"sent-report"}`
				if strings.HasSuffix(req.URL.Path, "/files") {
					body = `{"file_info":"uploaded-image"}`
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})
			chatType := "private"
			if group {
				chatType = "group"
			}
			client.SetOnMessageSent(func(msgID, refIndex, targetType, targetID string, metadata QuoteMetadata) {
				cache.SetTypedAsync(msgID, metadata.Text, chatType, targetID, metadata.Kind, metadata.TextComplete)
			})
			channel := &QQChannel{client: client}
			body := strings.Repeat("文", length)
			if err := channel.Transmit(context.Background(), &entity.Utterance{
				ChannelID: "recipient", IsGroup: group, Proactive: true,
				Text: "日报", CardData: []byte("image"), FallbackText: body,
			}); err != nil {
				t.Fatal(err)
			}
			record, found := cache.GetRecord("sent-report", chatType, "recipient")
			if !found || record.TextComplete != (length <= core.MaxUserMessageRunes) {
				t.Fatalf("group=%v length=%d: found=%v complete=%v", group, length, found, record.TextComplete)
			}
			if length <= core.MaxUserMessageRunes && record.Content != body {
				t.Fatal("complete report lost its original body")
			}
		}
	}
}

func startTestQQChannel(t *testing.T, channel *QQChannel) <-chan *entity.SensoryEvent {
	t.Helper()
	events := make(chan *entity.SensoryEvent, 1)
	if err := channel.Start(context.Background(), func(event *entity.SensoryEvent) {
		events <- event
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := channel.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return events
}

func awaitTestQQEvent(t *testing.T, events <-chan *entity.SensoryEvent) *entity.SensoryEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(3 * time.Second):
		t.Fatal("expected sensory event dispatched")
		return nil
	}
}

func TestMsgCache_PersistenceAndHit(t *testing.T) {
	tmpDir := t.TempDir()
	dbMgr := db.NewManager(tmpDir)
	t.Cleanup(func() {
		if err := dbMgr.Close(); err != nil {
			t.Error(err)
		}
	})

	cache1 := newMsgCache(10, dbMgr)
	testMsgID := "msg_123456"
	testContent := "func main() {\n\tprintln(\"Hello Daidai\")\n}"
	cache1.Set(testMsgID, testContent, "private", "user_123")

	// 模拟进程重启，重新从 SQLite 加载
	cache2 := newMsgCache(10, dbMgr)
	for _, cache := range []*msgCache{cache1, cache2} {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := cache.Close(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	hit := cache2.Get(testMsgID, "private", "user_123")
	if hit != testContent {
		t.Fatalf("expected cache hit %q, got %q", testContent, hit)
	}
}

func TestQuoteCacheFallsBackToScopedHistory(t *testing.T) {
	manager := db.NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := manager.LogInboundMessage(context.Background(), "qq", "group", "group-a", "user", "", "原始正文", "message-id"); err != nil {
		t.Fatal(err)
	}
	cache := newMsgCache(1, manager)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := cache.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	record, found := cache.GetRecord("message-id", "group", "group-a")
	if !found || record.Content != "原始正文" || record.Direction != "inbound" {
		t.Fatalf("history fallback failed: record=%v found=%v", record, found)
	}
	if _, found := cache.GetRecord("message-id", "group", "group-b"); found {
		t.Fatal("quoted history escaped its conversation")
	}
}

func TestQQChannel_MessageReferenceResolutionAndZeroPrefix(t *testing.T) {
	tmpDir := t.TempDir()
	dbMgr := db.NewManager(tmpDir)
	t.Cleanup(func() {
		if err := dbMgr.Close(); err != nil {
			t.Error(err)
		}
	})
	cache := newMsgCache(10, dbMgr)

	referencedID := "ref_msg_888"
	referencedCode := "package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"Daidai\") }"
	cache.Set(referencedID, referencedCode, "group", "GROUP_OPENID_RAW_123")

	channel := &QQChannel{
		msgCache: cache,
		dedup:    newDedupCache(100),
	}

	events := startTestQQChannel(t, channel)

	// 模拟腾讯推送 GROUP_AT_MESSAGE_CREATE 且携带 message_reference
	payload := map[string]any{
		"t": "GROUP_AT_MESSAGE_CREATE",
		"d": map[string]any{
			"id":           "msg_current_999",
			"content":      "转为图片",
			"group_openid": "GROUP_OPENID_RAW_123",
			"author": map[string]any{
				"member_openid": "MEMBER_OPENID_RAW_456",
			},
			"message_reference": map[string]any{
				"message_id": referencedID,
			},
		},
	}

	channel.dispatchEvent(context.Background(), payload)

	// 等待防抖窗口触发
	capturedEvent := awaitTestQQEvent(t, events)

	if capturedEvent == nil {
		t.Fatalf("expected sensory event dispatched")
	}
	if !capturedEvent.IsGroup {
		t.Errorf("expected IsGroup=true")
	}
	if capturedEvent.ChannelID != "GROUP_OPENID_RAW_123" {
		t.Errorf("expected zero-prefix ChannelID 'GROUP_OPENID_RAW_123', got %q", capturedEvent.ChannelID)
	}
	if capturedEvent.SenderID != "MEMBER_OPENID_RAW_456" {
		t.Errorf("expected zero-prefix SenderID 'MEMBER_OPENID_RAW_456', got %q", capturedEvent.SenderID)
	}
	if capturedEvent.ReplyContext == nil {
		t.Fatalf("expected ReplyContext resolved from message_reference")
	}
	if capturedEvent.ReplyContext.Content != referencedCode {
		t.Fatalf("expected ReplyContext content %q, got %q", referencedCode, capturedEvent.ReplyContext.Content)
	}
}

func TestQQChannel_SensoryEventWithQuoteMarkdown(t *testing.T) {
	channel := &QQChannel{
		dedup: newDedupCache(100),
	}

	events := startTestQQChannel(t, channel)

	testIncoming := &entity.SensoryEvent{
		Platform:   "qq",
		ChannelID:  "123",
		SenderID:   "123",
		SenderName: "好友",
		Content:    "转为图片",
		ReplyContext: &entity.SensoryEvent{
			Content: "const Pi = 3.14159",
		},
	}

	channel.IngestRaw(testIncoming)

	// 等待防抖窗口触发 (测试环境合并)
	capturedEvent := awaitTestQQEvent(t, events)

	if capturedEvent == nil {
		t.Fatalf("expected sensory event passed to superagent")
	}
	if capturedEvent.ReplyContext == nil {
		t.Fatalf("expected sensory event to retain ReplyContext")
	}
	if capturedEvent.ReplyContext.Content != "const Pi = 3.14159" {
		t.Fatalf("expected reply content 'const Pi = 3.14159', got: %s", capturedEvent.ReplyContext.Content)
	}
}

func TestCleanQQContent_StripMentionTags(t *testing.T) {
	got := cleanQQContent("<@!1906288578> /status")
	if got != "/status" {
		t.Fatalf("expected '/status', got %q", got)
	}
	got2 := cleanQQContent("  <@!123456>  你好呀 <@!654321> ")
	if got2 != "你好呀" {
		t.Fatalf("expected '你好呀', got %q", got2)
	}
}

func TestQQChannel_V2MsgElementsQuoteResolution(t *testing.T) {
	channel := &QQChannel{
		dedup: newDedupCache(100),
	}

	events := startTestQQChannel(t, channel)

	// 模拟官方 v2 原生引用消息包 (message_type: 103, msg_elements)
	payload := map[string]any{
		"t": "GROUP_AT_MESSAGE_CREATE",
		"d": map[string]any{
			"id":           "msg_v2_quote_103",
			"content":      "这个方案怎么实现？",
			"group_openid": "GROUP_103",
			"message_type": 103,
			"author": map[string]any{
				"member_openid": "USER_103",
				"username":      "群友小王",
			},
			"msg_elements": []any{
				map[string]any{
					"message_type": 103,
					"content":      "建议采用事件驱动架构与 Actor 模型结合",
				},
			},
		},
	}

	channel.dispatchEvent(context.Background(), payload)

	// 等待防抖窗口触发
	capturedEvent := awaitTestQQEvent(t, events)

	if capturedEvent == nil {
		t.Fatalf("expected sensory event to be captured")
	}
	if capturedEvent.SenderName != "群友小王" {
		t.Fatalf("expected SenderName '群友小王', got %q", capturedEvent.SenderName)
	}
	if capturedEvent.ReplyContext == nil {
		t.Fatalf("expected ReplyContext resolved from msg_elements")
	}
	expectedQuoted := "建议采用事件驱动架构与 Actor 模型结合"
	if capturedEvent.ReplyContext.Content != expectedQuoted {
		t.Fatalf("expected ReplyContext %q, got %q", expectedQuoted, capturedEvent.ReplyContext.Content)
	}
}

func TestQQSevenPageBriefingKeepsClickableIndexAndPartialState(t *testing.T) {
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct{ failIndex, longURL bool }{{}, {true, false}, {false, true}, {true, true}} {
		failIndex := scenario.failIndex
		client := NewClient("test", "test")
		var sent []string
		client.httpClient.Transport = qqTestTransport(func(req *http.Request) (*http.Response, error) {
			body, status := `{}`, 200
			header := make(http.Header)
			switch {
			case strings.HasSuffix(req.URL.Path, "/prepare"):
				body = `{"upload_id":"upload","presigned_url":"https://upload.example.com/image"}`
			case req.Method == http.MethodPut:
				header.Set("ETag", "etag")
			case strings.HasSuffix(req.URL.Path, "/complete"):
				body = `{"image_id":"material","url":"qqbot-media://image/v1/material"}`
				if scenario.longURL {
					body = `{"image_id":"material","url":"qqbot-media://image/v1/` + strings.Repeat("x", 2100) + `"}`
				}
			case strings.HasSuffix(req.URL.Path, "/messages"):
				if !strings.Contains(req.URL.Path, "/groups/") {
					t.Error("group briefing routed to private chat")
				}
				var payload struct{ Markdown struct{ Content string } }
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				sent = append(sent, payload.Markdown.Content)
				body = `{"id":"sent"}`
				if failIndex && len(sent) == 2 {
					body, status = `{"code":403}`, 403
				}
			}
			return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		images := make([][]byte, 7)
		sources := make([]string, 7)
		for n := range images {
			images[n] = pngData.Bytes()
			sources[n] = fmt.Sprintf("第%d页正文", n+1)
		}
		links := 100
		if scenario.longURL {
			links = 70
		}
		index := strings.Repeat("[01 原文](https://example.com/original)\n", links)
		channel := &QQChannel{client: client}
		err := channel.Transmit(context.Background(), &entity.Utterance{ChannelID: "group", IsGroup: true, Proactive: true, Images: images, ImageSources: sources, InlineImages: true, FallbackText: "完整早报正文", Text: index})
		if len(sent) == 0 || (!failIndex && strings.Count(strings.Join(sent, "\n"), "qqbot-media://") != 7) {
			t.Fatalf("images were truncated: %v", err)
		}
		if !scenario.longURL && strings.Count(sent[0], "qqbot-media://") != 7 {
			t.Fatal("short references should share one message")
		}
		for _, payload := range sent {
			if len(payload) > 4000 {
				t.Fatal("message exceeds platform length limit")
			}
		}
		if failIndex {
			var partial *entity.PartialDeliveryError
			if !errors.As(err, &partial) || !strings.Contains(partial.DeliveredText, "第1页正文") || partial.UndeliveredText == "" || strings.Contains(partial.UndeliveredText, "第1页正文") {
				t.Fatalf("wrong partial state: %v", err)
			}
			if scenario.longURL && (!strings.Contains(partial.UndeliveredText, "第7页正文") || strings.Contains(partial.DeliveredText, "第7页正文")) {
				t.Fatal("rejected image batch was marked delivered")
			}
		} else if err != nil || strings.Count(strings.Join(sent, "\n"), "[01 原文](https://example.com/original)") != links {
			t.Fatalf("index was lost: %v", err)
		}
		if scenario.longURL && !failIndex && len(sent) <= 5 {
			t.Fatal("proactive batches were artificially capped")
		}
	}
}

func TestQQUsesPerPageSourcesForQuotesAndPartialDelivery(t *testing.T) {
	client := NewClient("test", "test")
	count := 0
	var metadata []QuoteMetadata
	client.SetOnMessageSent(func(_, _, _, _ string, m QuoteMetadata) { metadata = append(metadata, m) })
	client.httpClient.Transport = qqTestTransport(func(req *http.Request) (*http.Response, error) {
		body := `{"file_info":"uploaded"}`
		status := 200
		if !strings.HasSuffix(req.URL.Path, "/files") {
			count++
			body = `{"id":"sent"}`
			if count == 2 {
				status = 403
				body = `{"code":403}`
			}
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	channel := &QQChannel{client: client}
	err := channel.Transmit(context.Background(), &entity.Utterance{ChannelID: "recipient", Proactive: true, Images: [][]byte{[]byte("one"), []byte("two")}, ImageSources: []string{"第一页原文", "第二页原文"}, FallbackText: "完整原文"})
	var partial *entity.PartialDeliveryError
	if !errors.As(err, &partial) {
		t.Fatalf("expected partial failure, got %v", err)
	}
	if !strings.Contains(partial.DeliveredText, "第一页原文") || strings.Contains(partial.DeliveredText, "第二页原文") || !strings.Contains(partial.UndeliveredText, "第二页原文") {
		t.Fatalf("wrong partial semantics: %+v", partial)
	}
	if len(metadata) == 0 || metadata[0].Text != "第一页原文" || metadata[0].Kind != qqQuoteTextCard {
		t.Fatalf("wrong quote source: %+v", metadata)
	}
}
