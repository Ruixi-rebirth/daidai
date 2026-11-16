package qq

import (
	"context"
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
	defer dbMgr.Close()

	cache1 := newMsgCache(10, dbMgr)
	testMsgID := "msg_123456"
	testContent := "func main() {\n\tprintln(\"Hello Daidai\")\n}"
	cache1.Set(testMsgID, testContent, "private", "user_123")

	// 模拟进程重启，重新从 SQLite 加载
	cache2 := newMsgCache(10, dbMgr)
	hit := cache2.Get(testMsgID, "private", "user_123")
	if hit != testContent {
		t.Fatalf("expected cache hit %q, got %q", testContent, hit)
	}
}

func TestQQChannel_MessageReferenceResolutionAndZeroPrefix(t *testing.T) {
	tmpDir := t.TempDir()
	dbMgr := db.NewManager(tmpDir)
	defer dbMgr.Close()
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
