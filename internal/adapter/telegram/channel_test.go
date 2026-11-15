package telegram

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"daidai/internal/db"
	"daidai/internal/entity"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOwnerBindingPinsNumericIdentity(t *testing.T) {
	database := db.NewManager(t.TempDir())
	defer database.Close()
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
	if err != nil || bound != "42" {
		t.Fatalf("binding not persisted: %q %v", bound, err)
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
	if len(events) != 2 || events[1].Role != entity.RoleGuest {
		t.Fatal("renamed foreign user acquired pinned owner's rights")
	}
	m.From = &user{ID: 42, Username: "renamed_owner"}
	m.Chat.Type = "private"
	m.Chat.ID = 42
	if err := c.handle(context.Background(), m, handle); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[2].Role != entity.RolePartner {
		t.Fatal("owner lost access after username change")
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
