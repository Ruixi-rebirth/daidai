package qq

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"daidai/internal/entity"
)

func TestQQTransmit_OnDemandMessageReferenceGroupAndPrivate(t *testing.T) {
	var payloads []map[string]any
	client := NewClient("app", "secret")
	client.accessToken = "token"
	client.httpClient.Transport = qqTestTransport(func(req *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"id":"sent-1"}`)),
		}, nil
	})

	ch := &QQChannel{client: client}

	// 1. Adjacent group reply (no newer message) -> omit message_reference.
	ch.recordLastInboundMsgID(true, "group-1", "inbound-msg-41")
	if err := ch.Transmit(context.Background(), &entity.Utterance{
		Platform:       "qq",
		IsGroup:        true,
		ChannelID:      "group-1",
		QuoteMsgID:     "inbound-msg-41",
		MentionUserIDs: []string{"user-1"},
		Text:           "在呢，咋啦",
	}); err != nil {
		t.Fatalf("adjacent group Transmit failed: %v", err)
	}
	if _, hasRef := payloads[0]["message_reference"]; hasRef {
		t.Fatalf("adjacent group reply should omit message_reference, got %#v", payloads[0]["message_reference"])
	}

	// 2. Interleaved group reply (newer message arrived) -> attach message_reference on first part only.
	payloads = nil
	ch.recordLastInboundMsgID(true, "group-1", "inbound-msg-42")
	ch.recordLastInboundMsgID(true, "group-1", "inbound-msg-43")
	longText := strings.Repeat("这是一段较长的回复内容。", 150) // > 1200 runes -> 2 chunks
	if err := ch.Transmit(context.Background(), &entity.Utterance{
		Platform:       "qq",
		IsGroup:        true,
		ChannelID:      "group-1",
		QuoteMsgID:     "inbound-msg-42",
		MentionUserIDs: []string{"user-1"},
		Text:           longText,
	}); err != nil {
		t.Fatalf("interleaved group Transmit failed: %v", err)
	}
	if len(payloads) < 2 {
		t.Fatalf("expected multi-part message, got %d payloads", len(payloads))
	}
	ref, ok := payloads[0]["message_reference"].(map[string]any)
	if !ok || ref["message_id"] != "inbound-msg-42" || ref["ignore_get_message_error"] != true {
		t.Fatalf("expected message_reference on first part, got %#v", payloads[0]["message_reference"])
	}
	if _, hasRef := payloads[1]["message_reference"]; hasRef {
		t.Fatalf("expected no message_reference on subsequent parts, got %#v", payloads[1]["message_reference"])
	}

	// 3. Private chat: adjacent omits reference, interleaved attaches reference.
	payloads = nil
	ch.recordLastInboundMsgID(false, "user-1", "pm-1")
	if err := ch.Transmit(context.Background(), &entity.Utterance{
		Platform:   "qq",
		IsGroup:    false,
		ChannelID:  "user-1",
		QuoteMsgID: "pm-1",
		Text:       "收到",
	}); err != nil {
		t.Fatalf("adjacent private Transmit failed: %v", err)
	}
	if _, hasRef := payloads[0]["message_reference"]; hasRef {
		t.Fatalf("adjacent private reply should omit message_reference, got %#v", payloads[0]["message_reference"])
	}

	payloads = nil
	ch.recordLastInboundMsgID(false, "user-1", "pm-2")
	if err := ch.Transmit(context.Background(), &entity.Utterance{
		Platform:   "qq",
		IsGroup:    false,
		ChannelID:  "user-1",
		QuoteMsgID: "pm-1",
		Text:       "这是回你上一条的",
	}); err != nil {
		t.Fatalf("interleaved private Transmit failed: %v", err)
	}
	ref, ok = payloads[0]["message_reference"].(map[string]any)
	if !ok || ref["message_id"] != "pm-1" || ref["ignore_get_message_error"] != true {
		t.Fatalf("interleaved private reply should attach message_reference, got %#v", payloads[0]["message_reference"])
	}
}

func TestQQTransmit_GroupReplyRetriesWithoutMessageReferenceOn4xxRejection(t *testing.T) {
	var payloads []map[string]any
	client := NewClient("app", "secret")
	client.accessToken = "token"
	client.httpClient.Transport = qqTestTransport(func(req *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, body)
		if _, hasRef := body["message_reference"]; hasRef {
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"code":304023,"message":"message_reference not supported"}`)),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"id":"sent-fallback"}`)),
		}, nil
	})

	ch := &QQChannel{client: client}
	ch.recordLastInboundMsgID(true, "group-1", "inbound-msg-99")
	ch.recordLastInboundMsgID(true, "group-1", "inbound-msg-100")
	err := ch.Transmit(context.Background(), &entity.Utterance{
		Platform:       "qq",
		IsGroup:        true,
		ChannelID:      "group-1",
		QuoteMsgID:     "inbound-msg-99",
		MentionUserIDs: []string{"user-1"},
		Text:           "回复内容",
	})
	if err != nil {
		t.Fatalf("expected fallback retry to succeed, got err: %v", err)
	}
	if len(payloads) != 2 {
		t.Fatalf("expected 2 requests (with ref + fallback), got %d", len(payloads))
	}
	if _, hasRef := payloads[0]["message_reference"]; !hasRef {
		t.Fatal("first attempt should include message_reference")
	}
	if _, hasRef := payloads[1]["message_reference"]; hasRef {
		t.Fatal("fallback attempt should omit message_reference")
	}
	if payloads[1]["msg_id"] != "inbound-msg-99" {
		t.Fatalf("fallback attempt must preserve msg_id, got %#v", payloads[1]["msg_id"])
	}
}

func TestQQTransmit_AnonymousGroupInterjectionOmitsMessageReference(t *testing.T) {
	var payloads []map[string]any
	client := NewClient("app", "secret")
	client.accessToken = "token"
	client.httpClient.Transport = qqTestTransport(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		payloads = append(payloads, body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"id":"sent-interject"}`)),
		}, nil
	})

	ch := &QQChannel{client: client}
	ch.recordLastInboundMsgID(true, "group-1", "inbound-msg-7")
	ch.recordLastInboundMsgID(true, "group-1", "inbound-msg-8")
	err := ch.Transmit(context.Background(), &entity.Utterance{
		Platform:              "qq",
		IsGroup:               true,
		ChannelID:             "group-1",
		QuoteMsgID:            "inbound-msg-7",
		GroupInterjection:     true,
		AnonymousInterjection: true,
		Text:                  "确实挺冷的",
	})
	if err != nil {
		t.Fatalf("Transmit failed: %v", err)
	}
	if len(payloads) != 1 {
		t.Fatalf("expected 1 payload, got %d", len(payloads))
	}
	if _, hasRef := payloads[0]["message_reference"]; hasRef {
		t.Fatalf("anonymous group interjection should not attach message_reference, got %#v", payloads[0]["message_reference"])
	}
}

func TestQQTransmit_ReplyGroupInterjectionQuotesOnDrift(t *testing.T) {
	var payloads []map[string]any
	client := NewClient("app", "secret")
	client.accessToken = "token"
	client.httpClient.Transport = qqTestTransport(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		payloads = append(payloads, body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"id":"sent-interject-reply"}`)),
		}, nil
	})

	ch := &QQChannel{client: client}
	ch.recordLastInboundMsgID(true, "group-1", "inbound-msg-7")
	ch.recordLastInboundMsgID(true, "group-1", "inbound-msg-8")
	err := ch.Transmit(context.Background(), &entity.Utterance{
		Platform:          "qq",
		IsGroup:           true,
		ChannelID:         "group-1",
		QuoteMsgID:        "inbound-msg-7",
		GroupInterjection: true,
		Text:              "Phosh 和 Plasma Mobile 确实最顺手",
	})
	if err != nil {
		t.Fatalf("Transmit failed: %v", err)
	}
	if len(payloads) != 1 {
		t.Fatalf("expected 1 payload, got %d", len(payloads))
	}
	ref, _ := payloads[0]["message_reference"].(map[string]any)
	if ref == nil || ref["message_id"] != "inbound-msg-7" {
		t.Fatalf("reply group interjection should attach message_reference when newer messages arrived, got %#v", payloads[0]["message_reference"])
	}
}
