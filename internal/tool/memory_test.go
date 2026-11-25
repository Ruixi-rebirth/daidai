package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"daidai/internal/core"
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

func TestMemoryProfileRejectsUnmentionedThirdPartyAndScopesMentionedMember(t *testing.T) {
	store := db.NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	tool := NewMemoryTool(store)
	thirdPartyQuestions := []string{
		"你对主人的印象是什么？",
		"你对群主的印象是什么？",
		"谈谈你对管理员的印象",
		"你对他的看法是什么",
	}
	for _, q := range thirdPartyQuestions {
		ctx := entity.WithSensoryEvent(context.Background(), &entity.SensoryEvent{
			Platform:  "qq",
			ChannelID: "group1",
			SenderID:  "user1",
			IsGroup:   true,
			Content:   q,
		})
		args, _ := json.Marshal(memoryArgs{Action: "profile"})
		result, err := tool.Execute(ctx, args)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", q, err)
		}
		if !strings.Contains(result.Text, "明确 @") {
			t.Fatalf("expected @ requirement rejection for query %q, got: %s", q, result.Text)
		}
	}

	ctx := context.Background()
	if err := store.ApplyFactMutations(ctx, "qq", "user2", []db.FactMutation{
		{Action: "ADD", Fact: "在本群爱聊 NixOS", ScopeType: "group", ScopeID: "group1"},
		{Action: "ADD", Fact: "私聊秘密爱好", ScopeType: "global"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveUserRelationInsight(ctx, "qq", "user2", "group1", true, "嘴硬但聊起 NixOS 特别热心。", 12, "", ctxTimeZero()); err != nil {
		t.Fatal(err)
	}

	mentionedCtx := entity.WithSensoryEvent(ctx, &entity.SensoryEvent{
		Platform:       "qq",
		ChannelID:      "group1",
		SenderID:       "user1",
		IsGroup:        true,
		Content:        "@阿宅 你还记得他吗",
		MentionedUsers: []core.GroupParticipant{{ID: "user2", Name: "阿宅"}},
	})
	args, _ := json.Marshal(memoryArgs{Action: "profile", TargetUserID: "user2"})
	result, err := tool.Execute(mentionedCtx, args)
	if err != nil {
		t.Fatalf("unexpected error for mentioned member profile: %v", err)
	}
	if !strings.Contains(result.Text, "嘴硬但聊起 NixOS 特别热心") || !strings.Contains(result.Text, "在本群爱聊 NixOS") {
		t.Fatalf("expected group impression and fact in profile card, got: %s", result.Text)
	}
	if strings.Contains(result.Text, "私聊秘密爱好") {
		t.Fatalf("private fact leaked into group member profile card: %s", result.Text)
	}

	selfQuery := "你对我有什么印象？"
	selfCtx := entity.WithSensoryEvent(context.Background(), &entity.SensoryEvent{
		Platform:  "qq",
		ChannelID: "group1",
		SenderID:  "user1",
		IsGroup:   true,
		Content:   selfQuery,
	})
	selfArgs, _ := json.Marshal(memoryArgs{Action: "profile"})
	selfRes, err := tool.Execute(selfCtx, selfArgs)
	if err != nil {
		t.Fatalf("self query failed: %v", err)
	}
	if selfRes == nil || strings.Contains(selfRes.Text, "明确 @") {
		t.Fatalf("self query %q was unexpectedly rejected: %+v", selfQuery, selfRes)
	}
}

func ctxTimeZero() (zero time.Time) { return zero }
