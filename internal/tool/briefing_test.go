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
func TestBriefingGroupSubscriptionAndGuestAccessStayInCurrentGroup(t *testing.T) {
	tool, store := newTestBriefingTool(t)
	owner := entity.WithSensoryEvent(context.Background(), &entity.SensoryEvent{Platform: "qq", ChannelID: "group", Role: entity.RolePartner, IsGroup: true})
	guestEvent := &entity.SensoryEvent{Platform: "qq", ChannelID: "group", Role: entity.RoleGuest, IsGroup: true}
	guest := entity.WithSensoryEvent(context.Background(), guestEvent)
	result, err := tool.Execute(guest, []byte(`{}`))
	if err != nil || !strings.Contains(result.Text, "还没有开启") {
		t.Fatalf("unsubscribed group admitted: %v %v", result, err)
	}
	tool.manage = true
	if result, err = tool.Execute(guest, []byte(`{"action":"subscribe"}`)); err != nil || !strings.Contains(result.Text, "核心搭档") {
		t.Fatal("guest could manage subscriptions")
	}
	if _, err = tool.Execute(owner, []byte(`{"action":"subscribe"}`)); err != nil {
		t.Fatal(err)
	}
	sub, err := store.Subscription(owner, "qq", "group", true)
	if err != nil || sub == nil || !sub.Enabled || !sub.IsGroup {
		t.Fatalf("group subscription lost: %+v %v", sub, err)
	}
	private, err := store.Subscription(owner, "qq", "group", false)
	if err != nil || private != nil {
		t.Fatal("group subscription leaked into same-ID private chat")
	}
	on := true
	for _, other := range []briefing.Subscription{
		{Platform: "qq", Recipient: "private-owner-address", Owner: "owner"},
		{Platform: "qq", Recipient: "hidden-other-group", IsGroup: true, Owner: "owner"},
	} {
		if err = store.Touch(owner, other, &on); err != nil {
			t.Fatal(err)
		}
	}
	result, err = tool.Execute(owner, []byte(`{"action":"status"}`))
	if err != nil || strings.Contains(result.Text, "private-owner-address") || strings.Contains(result.Text, "hidden-other-group") {
		t.Fatal("group status exposed another conversation")
	}
	tool.manage = false
	if result, err = tool.Execute(guest, []byte(`{}`)); err != nil || !strings.Contains(result.Text, "尚未生成") {
		t.Fatal("subscribed group guest could not read")
	}
	other := *guestEvent
	other.ChannelID = "other-group"
	if result, err = tool.Execute(entity.WithSensoryEvent(context.Background(), &other), []byte(`{}`)); err != nil || !strings.Contains(result.Text, "还没有开启") {
		t.Fatal("subscription leaked into another group")
	}
	tool.manage = true
	if _, err = tool.Execute(owner, []byte(`{"action":"unsubscribe"}`)); err != nil {
		t.Fatal(err)
	}
	tool.manage = false
	if result, err = tool.Execute(guest, []byte(`{}`)); err != nil || !strings.Contains(result.Text, "还没有开启") {
		t.Fatal("disabled group retained guest access")
	}
}

func TestBriefingSubscriptionIsPerPlatformAndNoImplicitOptIn(t *testing.T) {
	tool, store := newTestBriefingTool(t)
	ctx := entity.WithSensoryEvent(context.Background(), &entity.SensoryEvent{Platform: "telegram", ChannelID: "1", Role: entity.RolePartner})
	if _, err := tool.Execute(ctx, []byte(`{"date":"today"}`)); err != nil {
		t.Fatal(err)
	}
	sub, err := store.Subscription(ctx, "telegram", "1", false)
	if err != nil || sub == nil || sub.Enabled {
		t.Fatal("view silently subscribed user")
	}
	tool.manage = true
	if _, err := tool.Execute(ctx, []byte(`{"action":"subscribe"}`)); err != nil {
		t.Fatal(err)
	}
	sub, err = store.Subscription(ctx, "telegram", "1", false)
	if err != nil || !sub.Enabled {
		t.Fatal("subscription not stored")
	}
	other, err := store.Subscription(ctx, "qq", "1", false)
	if err != nil || other != nil {
		t.Fatal("subscription leaked across platforms")
	}
	if _, err := tool.Execute(ctx, []byte(`{"action":"unsubscribe"}`)); err != nil {
		t.Fatal(err)
	}
	sub, err = store.Subscription(ctx, "telegram", "1", false)
	if err != nil || sub.Enabled {
		t.Fatal("unsubscribe failed")
	}
}
