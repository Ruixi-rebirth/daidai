package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"daidai/internal/db"
	"daidai/internal/entity"
)

func TestUserRelationsScopeIsolationAndEpisodicContinuity(t *testing.T) {
	store, engine, provider := newFactTestEngine(t, nil, `{"mutations":[],"emotion":"疲惫","impression":"平时爱折腾 Linux，说话挺真诚的。"}`)
	ctx := context.Background()
	now := time.Now()

	for i := 0; i < 3; i++ {
		ev := &entity.SensoryEvent{
			Platform:   "qq",
			IsGroup:    true,
			ChannelID:  "group-a",
			SenderID:   "u100",
			SenderName: "阿宅",
			Content:    "今天修内核模块修得好累啊",
			OccurredAt: now.Add(time.Duration(i) * time.Minute),
		}
		engine.Commit(ctx, ev, "歇会儿呗")
	}
	engine.asyncWG.Wait()

	relA, err := store.GetVisibleUserRelation(ctx, "qq", "u100", "group-a", true)
	if err != nil || relA == nil {
		t.Fatalf("expected group-a relation to be saved, got rel=%+v err=%v", relA, err)
	}
	if relA.InteractionCount != 3 || !strings.Contains(relA.Impression, "折腾 Linux") || relA.RecentEmotion != "疲惫" {
		t.Fatalf("unexpected group-a relation state: %+v (calls=%d)", relA, provider.calls.Load())
	}

	// Private chat and another group must NOT see group-a's impression or emotion.
	if relPrivate, err := store.GetVisibleUserRelation(ctx, "qq", "u100", "", false); err != nil || relPrivate != nil {
		t.Fatalf("group relation leaked into private scope: %+v %v", relPrivate, err)
	}
	if relB, err := store.GetVisibleUserRelation(ctx, "qq", "u100", "group-b", true); err != nil || relB != nil {
		t.Fatalf("group-a relation leaked into group-b: %+v %v", relB, err)
	}

	// Within 6h, RecallEpisodic injects familiarity, impression, and gentle care for tired familiar user.
	followUp := &entity.SensoryEvent{
		Platform:   "qq",
		IsGroup:    true,
		ChannelID:  "group-a",
		SenderID:   "u100",
		SenderName: "阿宅",
		Content:    "还在看日志",
		OccurredAt: now.Add(30 * time.Minute),
	}
	episodic := engine.RecallEpisodic(ctx, followUp)
	if !strings.Contains(episodic, "熟悉度分寸") || !strings.Contains(episodic, "折腾 Linux") || !strings.Contains(episodic, "疲惫") || !strings.Contains(episodic, "轻轻放软") {
		t.Fatalf("expected familiarity, impression, and care hint in RecallEpisodic, got:\n%s", episodic)
	}

	// After 7h (> 6h TTL), stale emotion hint expires while impression stays.
	sevenHoursLater := *followUp
	sevenHoursLater.OccurredAt = now.Add(7 * time.Hour)
	episodicLater := engine.RecallEpisodic(ctx, &sevenHoursLater)
	if strings.Contains(episodicLater, "疲惫") {
		t.Fatalf("stale 7h emotion should not be injected, got:\n%s", episodicLater)
	}
	if !strings.Contains(episodicLater, "折腾 Linux") {
		t.Fatalf("persistent impression should remain after emotion expires, got:\n%s", episodicLater)
	}
}

func TestBusyGroupPeerThreadAndSameNameDisambiguation(t *testing.T) {
	mgr := db.NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := mgr.Close(); err != nil {
			t.Error(err)
		}
	})
	engine := NewMemoryEngine(nil)
	engine.SetDBManager(mgr)
	ctx := context.Background()
	now := time.Now()

	// u1 and u2 share the same nickname "小明"; u3, u4, u5 interleave noise.
	ev1 := &entity.SensoryEvent{
		Platform: "qq", IsGroup: true, ChannelID: "g1",
		SenderID: "100001", SenderName: "小明", Content: "我的 NixOS flake 锁文件冲突了",
		OccurredAt: now,
	}
	engine.Commit(ctx, ev1, "你先把 flake.lock 冲突块发来看看")

	for idx, noise := range []struct{ id, name, text string }{
		{"200002", "小明", "今晚吃烤肉还是火锅"},
		{"300003", "路人甲", "火锅走起"},
		{"400004", "路人乙", "加我一个"},
	} {
		obs := &entity.SensoryEvent{
			Platform: "qq", IsGroup: true, ChannelID: "g1",
			SenderID: noise.id, SenderName: noise.name, Content: noise.text,
			RawMessageID: string(rune('a' + idx)),
			OccurredAt:   now.Add(time.Duration(idx+1) * time.Minute),
		}
		if err := engine.RecordInbound(ctx, obs); err != nil {
			t.Fatal(err)
		}
	}

	current := &entity.SensoryEvent{
		Platform: "qq", IsGroup: true, ChannelID: "g1",
		SenderID: "100001", SenderName: "小明", Content: "主要是 nixpkgs 输入有两个分支",
		RawMessageID: "cur",
		OccurredAt:   now.Add(5 * time.Minute),
	}
	if err := engine.RecordInbound(ctx, current); err != nil {
		t.Fatal(err)
	}

	working := engine.WorkingContext(ctx, current, 20)
	var joinedWorking strings.Builder
	for _, m := range working {
		joinedWorking.WriteString(m.Content + "\n")
	}
	if !strings.Contains(joinedWorking.String(), "[小明#100001]") || !strings.Contains(joinedWorking.String(), "[小明#200002]") {
		t.Fatalf("expected colliding nicknames to be disambiguated with #shortID, got:\n%s", joinedWorking.String())
	}
	if strings.Contains(joinedWorking.String(), "[路人甲#") {
		t.Fatalf("unique nickname should not carry #shortID noise:\n%s", joinedWorking.String())
	}

	episodic := engine.RecallEpisodic(ctx, current)
	if !strings.Contains(episodic, "本场近期的对话线") || !strings.Contains(episodic, "flake 锁文件冲突") {
		t.Fatalf("expected busy group RecallEpisodic to extract speaker peer_thread, got:\n%s", episodic)
	}
	if strings.Contains(episodic, "烤肉还是火锅") {
		t.Fatalf("other speaker's message leaked into current speaker's peer_thread:\n%s", episodic)
	}
}
