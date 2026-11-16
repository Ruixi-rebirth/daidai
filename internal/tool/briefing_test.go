package tool

import (
	"context"
	"daidai/internal/briefing"
	"daidai/internal/config"
	"daidai/internal/entity"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestBriefingTool(t *testing.T) (*BriefingTool, *briefing.Store) {
	t.Helper()
	store, err := briefing.Open(filepath.Join(t.TempDir(), "briefing.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	service, err := briefing.New(context.Background(), store, config.BriefingConfig{Timezone: "Asia/Singapore", Time: "08:00"}, nil, entity.NewChannelRegistry())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := service.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return NewBriefingTool(service, map[string]string{"qq": "owner", "telegram": "owner"}, false), store
}
func TestBriefingPermissionsAndQuoteDate(t *testing.T) {
	tool, store := newTestBriefingTool(t)
	for _, role := range []entity.UserRole{entity.RoleGuest, entity.RoleCollaborator} {
		ctx := entity.WithSensoryEvent(context.Background(), &entity.SensoryEvent{Platform: "qq", ChannelID: "x", Role: role})
		result, err := tool.Execute(ctx, json.RawMessage(`{}`))
		if err != nil || !strings.Contains(result.Text, "核心搭档") {
			t.Fatal("non-owner was admitted")
		}
	}
	group := entity.WithSensoryEvent(context.Background(), &entity.SensoryEvent{Platform: "qq", ChannelID: "x", Role: entity.RolePartner, IsGroup: true})
	result, err := tool.Execute(group, []byte(`{}`))
	if err != nil || !strings.Contains(result.Text, "私聊") {
		t.Fatal("group request admitted")
	}
	event := &entity.SensoryEvent{Platform: "telegram", ChannelID: "1", Role: entity.RolePartner, ReplyContext: &entity.SensoryEvent{Content: "# 极客早报 · 2026-10-01\n\n截止 08:00"}}
	ctx := entity.WithSensoryEvent(context.Background(), event)
	date, err := tool.resolveDate(ctx, event, "")
	if err != nil || date != "2026-10-01" {
		t.Fatalf("lost quoted issue: %s %v", date, err)
	}
	date, err = tool.resolveDate(ctx, event, "today")
	if err != nil || date != tool.service.Now().Format("2006-01-02") {
		t.Fatal("explicit date did not override quote")
	}
	subs, err := store.Subscriptions(ctx)
	if err != nil || len(subs) != 0 {
		t.Fatal("rejected request created subscription")
	}
}
func TestBriefingSubscriptionIsPerPlatformAndNoImplicitOptIn(t *testing.T) {
	tool, store := newTestBriefingTool(t)
	ctx := entity.WithSensoryEvent(context.Background(), &entity.SensoryEvent{Platform: "telegram", ChannelID: "1", Role: entity.RolePartner})
	if _, err := tool.Execute(ctx, []byte(`{"date":"today"}`)); err != nil {
		t.Fatal(err)
	}
	sub, err := store.Subscription(ctx, "telegram", "1")
	if err != nil || sub == nil || sub.Enabled {
		t.Fatal("view silently subscribed user")
	}
	tool.manage = true
	if _, err := tool.Execute(ctx, []byte(`{"action":"subscribe"}`)); err != nil {
		t.Fatal(err)
	}
	sub, err = store.Subscription(ctx, "telegram", "1")
	if err != nil || !sub.Enabled {
		t.Fatal("subscription not stored")
	}
	other, err := store.Subscription(ctx, "qq", "1")
	if err != nil || other != nil {
		t.Fatal("subscription leaked across platforms")
	}
	if _, err := tool.Execute(ctx, []byte(`{"action":"unsubscribe"}`)); err != nil {
		t.Fatal(err)
	}
	sub, err = store.Subscription(ctx, "telegram", "1")
	if err != nil || sub.Enabled {
		t.Fatal("unsubscribe failed")
	}
}
