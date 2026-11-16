package tool

import (
	"context"
	"encoding/json"
	"testing"

	"daidai/internal/db"
	"daidai/internal/entity"
)

func TestMemoryAddIsAdditiveAndRetainsRepeatedEvidence(t *testing.T) {
	store := db.NewManager(t.TempDir())
	defer store.Close()
	tool := NewMemoryTool(store)
	ctx := context.Background()
	for _, fact := range []string{"周末喜欢去公园跑步", "周末喜欢去公园跑步", "周末喜欢去公园跑步和野餐"} {
		id, err := store.LogInboundMessage(ctx, "qq", "private", "chat", "user", "", fact, "")
		if err != nil {
			t.Fatal(err)
		}
		event := &entity.SensoryEvent{Platform: "qq", ChannelID: "chat", SenderID: "user", Content: fact, HistoryRecordID: id}
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
	sources, err := store.ListVisibleFactSources(ctx, "qq", "user", "chat", false, facts[0].ID)
	if err != nil || len(sources) != 2 {
		t.Fatalf("repeated evidence was lost: sources=%v error=%v", sources, err)
	}
}
