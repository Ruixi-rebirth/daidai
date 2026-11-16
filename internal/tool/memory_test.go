package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"daidai/internal/db"
	"daidai/internal/entity"
)

func TestMemorySourcesCanPageThroughAllOriginalMessages(t *testing.T) {
	store := db.NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.Background()
	var ids []int64
	for i := range 30 {
		id, err := store.LogInboundMessage(ctx, "qq", "private", "chat", "user", "", fmt.Sprintf("evidence-%02d", i), "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := store.ApplyFactMutations(ctx, "qq", "user", []db.FactMutation{{Action: "ADD", Fact: "测试事实", SourceMessageIDs: ids}}); err != nil {
		t.Fatal(err)
	}
	facts, err := store.ListVisibleFacts(ctx, "qq", "user", "chat", false)
	if err != nil || len(facts) != 1 {
		t.Fatalf("facts=%v error=%v", facts, err)
	}
	tool := NewMemoryTool(store)
	ctx = entity.WithSensoryEvent(ctx, &entity.SensoryEvent{Platform: "qq", ChannelID: "chat", SenderID: "user"})
	seen := make(map[int]bool)
	for page := 1; page <= 6; page++ {
		args, _ := json.Marshal(memoryArgs{Action: "source", FactID: facts[0].ID, Page: page})
		result, err := tool.Execute(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 30 {
			if strings.Contains(result.Text, fmt.Sprintf("evidence-%02d", i)) {
				if seen[i] {
					t.Fatalf("evidence %d appeared again on page %d", i, page)
				}
				seen[i] = true
			}
		}
	}
	if len(seen) != 30 {
		t.Fatalf("only %d of 30 sources could be viewed", len(seen))
	}
	args, _ := json.Marshal(memoryArgs{Action: "source", FactID: facts[0].ID, PageDirection: "next"})
	result, err := tool.Execute(ctx, args)
	if err != nil || !strings.Contains(result.Text, "最后一页") {
		t.Fatalf("last-page boundary failed: result=%v error=%v", result, err)
	}
	args, _ = json.Marshal(memoryArgs{Action: "source", FactID: facts[0].ID, Page: 7})
	result, err = tool.Execute(ctx, args)
	if err != nil || !strings.Contains(result.Text, "没有第 7 页") {
		t.Fatalf("explicit out-of-range page failed: result=%v error=%v", result, err)
	}
	otherUser := entity.WithSensoryEvent(context.Background(), &entity.SensoryEvent{Platform: "qq", ChannelID: "chat", SenderID: "other-user"})
	args, _ = json.Marshal(memoryArgs{Action: "source", FactID: facts[0].ID, Page: 1})
	result, err = tool.Execute(otherUser, args)
	if err != nil || strings.Contains(result.Text, "evidence-") || !strings.Contains(result.Text, "没有可查看") {
		t.Fatalf("another user could inspect sources: result=%v error=%v", result, err)
	}
}

func TestMemoryAddIsAdditiveAndRetainsRepeatedEvidence(t *testing.T) {
	store := db.NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	tool := NewMemoryTool(store)
	ctx := context.Background()
	for _, fact := range []string{"周末喜欢去公园跑步", "周末喜欢去公园跑步", "周末喜欢去公园跑步和野餐"} {
		id, err := store.LogInboundMessage(ctx, "qq", "private", "chat", "user", "", fact, "")
		if err != nil {
			t.Fatal(err)
		}
		event := &entity.SensoryEvent{Platform: "qq", ChannelID: "chat", SenderID: "user", Content: fact, HistoryRecordIDs: []int64{id}}
		args, err := json.Marshal(memoryArgs{Action: "add", Fact: fact})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tool.Execute(entity.WithSensoryEvent(ctx, event), args); err != nil {
			t.Fatal(err)
		}
	}
	facts, err := store.ListVisibleFacts(ctx, "qq", "user", "chat", false)
	if err != nil || len(facts) != 2 || facts[0].Content != "周末喜欢去公园跑步" {
		t.Fatalf("ADD modified an existing fact: facts=%v error=%v", facts, err)
	}
	sources, _, err := store.PageVisibleFactSources(ctx, "qq", "user", "chat", false, facts[0].ID, 1, 100)
	if err != nil || len(sources) != 2 {
		t.Fatalf("repeated evidence was lost: sources=%v error=%v", sources, err)
	}
}
