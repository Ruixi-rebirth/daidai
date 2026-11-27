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

func TestQQTransmit_GroupReplyIncludesMessageReferenceOnFirstPartOnly(t *testing.T) {
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
	longText := strings.Repeat("这是一段较长的回复内容。", 150) // > 1200 runes -> 2 chunks
	err := ch.Transmit(context.Background(), &entity.Utterance{
		Platform:       "qq",
		IsGroup:        true,
		ChannelID:      "group-1",
		QuoteMsgID:     "inbound-msg-42",
		MentionUserIDs: []string{"user-1"},
		Text:           longText,
	})
	if err != nil {
		t.Fatalf("Transmit failed: %v", err)
	}
	if len(payloads) < 2 {
		t.Fatalf("expected multi-part message, got %d payloads", len(payloads))
	}

	ref, ok := payloads[0]["message_reference"].(map[string]any)
	if !ok {
		t.Fatalf("expected message_reference on first part, got %#v", payloads[0]["message_reference"])
	}
	if ref["message_id"] != "inbound-msg-42" || ref["ignore_get_message_error"] != true {
		t.Fatalf("unexpected message_reference: %#v", ref)
	}
	if _, hasRef := payloads[1]["message_reference"]; hasRef {
		t.Fatalf("expected no message_reference on subsequent parts, got %#v", payloads[1]["message_reference"])
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
	err := ch.Transmit(context.Background(), &entity.Utterance{
		Platform:          "qq",
		IsGroup:           true,
		ChannelID:         "group-1",
		QuoteMsgID:        "inbound-msg-7",
		GroupInterjection: true,
		Text:              "确实挺冷的",
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
