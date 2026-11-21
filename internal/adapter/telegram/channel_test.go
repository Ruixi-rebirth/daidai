package telegram

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"daidai/internal/db"
	"daidai/internal/entity"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOwnerAuthorizationFollowsConfiguredUsername(t *testing.T) {
	database := db.NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	immune := entity.NewImmuneSystem("qq-owner")
	c := New(Config{OwnerUsername: "Ruixi_rebirth"}, database, nil, immune)
	if err := c.initQuotes(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := &message{ID: 1, From: &user{ID: 42, Username: "ruixi_REBIRTH"}, Text: "你好"}
	m.Chat.ID = 42
	m.Chat.Type = "private"
	var events []*entity.SensoryEvent
	handle := func(e *entity.SensoryEvent) { events = append(events, e) }
	if err := c.handle(context.Background(), m, handle); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Role != entity.RolePartner || events[0].SenderID != "tg:42" {
		t.Fatalf("owner event not authorized: %#v", events)
	}
	bound, err := database.GetRuntimeState(context.Background(), "telegram", "owner_id")
	if err != nil || bound != "" {
		t.Fatalf("unexpected persistent binding: %q %v", bound, err)
	}
	if immune.ResolveRole("tg:42") != entity.RolePartner || immune.ResolveRole("42") == entity.RolePartner {
		t.Fatal("owner identity was not platform scoped")
	}
	m.From = &user{ID: 99, Username: "Ruixi_rebirth"}
	m.Chat.Type = "group"
	m.Chat.ID = -100
	m.Text = "呆呆你好"
	if err := c.handle(context.Background(), m, handle); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Role != entity.RolePartner {
		t.Fatal("configured username did not receive owner rights")
	}
	m.From = &user{ID: 42, Username: "renamed_owner"}
	m.Chat.Type = "group"
	m.Chat.ID = -100
	if err := c.handle(context.Background(), m, handle); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[2].Role != entity.RoleGuest {
		t.Fatal("renamed account retained owner access")
	}
	if immune.ResolveRole("tg:42") != entity.RoleGuest {
		t.Fatal("renamed account retained privileged tool access")
	}
}

func TestTransportFailureDoesNotExposeBotToken(t *testing.T) {
	c := New(Config{Token: "secret-token"}, nil, nil, nil)
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) { return nil, errors.New(r.URL.String()) })
	err := c.jsonCall(context.Background(), "getMe", map[string]any{}, nil)
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestGroupVerificationUsesGroupAndPreservesTopicRouting(t *testing.T) {
	c := New(Config{Token: "secret"}, nil, nil, nil)
	response := `{"ok":true,"result":{"id":-100,"type":"supergroup"}}`
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
	})
	if valid, err := c.VerifyGroup(context.Background(), "-100/42"); !valid || err != nil {
		t.Fatal("subscribed forum topic rejected")
	}
	response = `{"ok":true,"result":{"id":-101,"type":"supergroup"}}`
	if valid, err := c.VerifyGroup(context.Background(), "-100/42"); valid || err != nil {
		t.Fatal("different group admitted")
	}
	response = `{"ok":true,"result":{"id":123,"type":"private"}}`
	if valid, err := c.VerifyGroup(context.Background(), "123"); valid || err != nil {
		t.Fatal("non-group destination admitted")
	}
	if valid, err := c.VerifyGroup(context.Background(), "invalid/42"); valid || err != nil {
		t.Fatal("invalid group address admitted")
	}
}

func TestProactiveOwnerVerificationRechecksUsername(t *testing.T) {
	c := New(Config{Token: "secret", OwnerUsername: "Ruixi_rebirth"}, nil, nil, nil)
	response := `{"ok":true,"result":{"id":123,"type":"private","username":"ruixi_rebirth"}}`
	c.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
	})
	if valid, err := c.VerifyOwnerPrivate(context.Background(), "123"); !valid || err != nil {
		t.Fatal("matching username rejected")
	}
	response = `{"ok":true,"result":{"id":123,"type":"private","username":"someone_else"}}`
	if valid, err := c.VerifyOwnerPrivate(context.Background(), "123"); valid || err != nil {
		t.Fatal("numeric address became authorization")
	}
	response = `{"ok":false,"error_code":429,"parameters":{"retry_after":600}}`
	err := c.jsonCall(context.Background(), "sendMessage", nil, nil)
	var rejection *entity.DeliveryRejection
	if !errors.As(err, &rejection) || rejection.After != 10*time.Minute {
		t.Fatalf("rate-limit classification lost: %v", err)
	}
}
