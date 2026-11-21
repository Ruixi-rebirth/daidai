package db

import (
	"context"
	"testing"
	"time"
)

func TestConversationDraftReadsStayWithinOwnerAndExpiry(t *testing.T) {
	ctx := context.Background()
	store := NewManager(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	active := ConversationDraft{
		ID: "active", Platform: "qq", ChatType: "group", ConversationID: "group", RequesterID: "alice",
		Goal: "续写目标", ContextJSON: `{"messages":[]}`, UpdatedAt: time.Now(),
	}
	expired := active
	expired.ID, expired.UpdatedAt = "expired", active.UpdatedAt.Add(-8*24*time.Hour)
	for _, draft := range []ConversationDraft{active, expired} {
		if err := store.SaveConversationDraft(ctx, draft); err != nil {
			t.Fatal(err)
		}
	}

	summaries, err := store.ListConversationDraftSummaries(ctx, "qq", "group", "group", "alice")
	if err != nil || len(summaries) != 1 || summaries[0].ID != active.ID || summaries[0].Goal != active.Goal {
		t.Fatalf("unexpected summaries: %+v, %v", summaries, err)
	}
	got, err := store.GetConversationDraft(ctx, "qq", "group", "group", "alice", active.ID)
	if err != nil || got == nil || got.ContextJSON != active.ContextJSON {
		t.Fatalf("unexpected draft: %+v, %v", got, err)
	}
	for _, query := range []struct{ platform, chatType, convID, owner, id string }{
		{"qq", "group", "group", "alice", expired.ID},
		{"qq", "group", "group", "bob", active.ID},
		{"qq", "group", "other", "alice", active.ID},
		{"qq", "private", "group", "alice", active.ID},
		{"telegram", "group", "group", "alice", active.ID},
	} {
		got, err := store.GetConversationDraft(ctx, query.platform, query.chatType, query.convID, query.owner, query.id)
		if err != nil || got != nil {
			t.Fatalf("out-of-scope draft lookup %+v returned %+v, %v", query, got, err)
		}
	}

	conflict := active
	conflict.RequesterID = "bob"
	if err := store.SaveConversationDraft(ctx, conflict); err == nil {
		t.Fatal("conflicting owner was reported as saved")
	}

	const nextContext = `{"messages":[{"Content":"下一段"}]}`
	if err := store.UpdateConversationDraft(ctx, active, nextContext, active.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteConversationDraft(ctx, active); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetConversationDraft(ctx, "qq", "group", "group", "alice", active.ID)
	if err != nil || current == nil || current.ContextJSON != nextContext {
		t.Fatalf("old delivery removed a newer draft: %+v, %v", current, err)
	}
	if err := store.UpdateConversationDraft(ctx, active, active.ContextJSON, time.Now()); err == nil {
		t.Fatal("stale write overwrote newer progress")
	}
	if err := store.DeleteConversationDraft(ctx, *current); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateConversationDraft(ctx, *current, nextContext, time.Now()); err == nil {
		t.Fatal("continuation recreated a completed draft")
	}
}

func TestResetConversationClearsDraftsOnlyInSelectedChat(t *testing.T) {
	ctx := context.Background()
	store := NewManager(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	drafts := []ConversationDraft{
		{ID: "one", Platform: "qq", ChatType: "group", ConversationID: "same", RequesterID: "alice"},
		{ID: "two", Platform: "qq", ChatType: "group", ConversationID: "same", RequesterID: "bob"},
		{ID: "three", Platform: "qq", ChatType: "private", ConversationID: "same", RequesterID: "alice"},
		{ID: "four", Platform: "qq", ChatType: "group", ConversationID: "other", RequesterID: "alice"},
	}
	for _, draft := range drafts {
		draft.ContextJSON, draft.UpdatedAt = "{}", time.Now()
		if err := store.SaveConversationDraft(ctx, draft); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ResetConversation("qq", "group", "same"); err != nil {
		t.Fatal(err)
	}
	for i, draft := range drafts {
		got, err := store.GetConversationDraft(ctx, draft.Platform, draft.ChatType, draft.ConversationID, draft.RequesterID, draft.ID)
		if err != nil || (got == nil) != (i < 2) {
			t.Fatalf("reset affected wrong draft %q: %v, %v", draft.ID, got, err)
		}
	}
}
