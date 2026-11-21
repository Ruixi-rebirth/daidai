package memory

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
)

type factTestProvider struct {
	core.LLMProvider
	response string
	calls    atomic.Int32
}

func (p *factTestProvider) Chat(context.Context, []core.ChatMessage, *core.ChatOptions) (string, error) {
	p.calls.Add(1)
	return p.response, nil
}

type factTestEmbedder struct {
	stall   bool
	entered chan struct{}
}

func (e *factTestEmbedder) Embed(ctx context.Context, _ string) ([]float32, error) {
	if e.stall {
		if e.entered != nil {
			select {
			case e.entered <- struct{}{}:
			default:
			}
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []float32{1, 0}, nil
}

func newFactTestEngine(t *testing.T, embedder core.EmbeddingProvider, response string) (*db.Manager, *MemoryEngine, *factTestProvider) {
	t.Helper()
	store := db.NewManager(t.TempDir())
	provider := &factTestProvider{response: response}
	engine := NewMemoryEngine(provider)
	engine.SetDBManager(store)
	engine.embedder = embedder
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := engine.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store, engine, provider
}

func factTestEvent(t *testing.T, engine *MemoryEngine, content string) *entity.SensoryEvent {
	t.Helper()
	event := &entity.SensoryEvent{Platform: "qq", ChannelID: "private", SenderID: "user", Content: content}
	if err := engine.RecordInbound(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestFactExtractionHistoryStopsBeforeMergedTurn(t *testing.T) {
	store, engine, _ := newFactTestEngine(t, nil, "")
	prior := factTestEvent(t, engine, "我平时用 NixOS")
	first := factTestEvent(t, engine, "我平时用 NixOS")
	merged := factTestEvent(t, engine, "最近在试新版")
	merged.HistoryRecordIDs = append(first.HistoryRecordIDs, merged.HistoryRecordIDs...)
	factTestEvent(t, engine, "这是之后发的消息")

	history := factExtractionHistory(context.Background(), store, merged)
	if len(history) != 1 || history[0].Text != prior.Content || !history[0].FromCurrentUser {
		t.Fatalf("expected only the message before the merged turn, got %+v", history)
	}
	merged.HistoryRecordIDs = nil
	if history := factExtractionHistory(context.Background(), store, merged); len(history) != 0 {
		t.Fatalf("read unbounded history without a turn boundary: %+v", history)
	}
}

func TestRepeatedFactAddsEvidenceWithoutDuplicatingFact(t *testing.T) {
	store, engine, _ := newFactTestEngine(t, nil, `{"mutations":[{"action":"ADD","fact":"喜欢乌龙茶"}]}`)
	for range 2 {
		event := factTestEvent(t, engine, "我喜欢乌龙茶")
		engine.consolidateUserFacts(context.Background(), event, "")
	}
	facts, err := store.ListVisibleFacts(context.Background(), "qq", "user", "private", false)
	if err != nil || len(facts) != 1 {
		t.Fatalf("facts=%v error=%v", facts, err)
	}
	sources, _, err := store.PageVisibleFactSources(context.Background(), "qq", "user", "private", false, facts[0].ID, 1, 100)
	if err != nil || len(sources) != 2 {
		t.Fatalf("expected both supporting messages, sources=%v error=%v", sources, err)
	}
}

func TestMergedTurnRetainsEveryOriginalSourceMessage(t *testing.T) {
	store, engine, _ := newFactTestEngine(t, nil, `{"mutations":[{"action":"ADD","fact":"喜欢乌龙茶"}]}`)
	first := factTestEvent(t, engine, "我喜欢乌龙茶")
	last := factTestEvent(t, engine, "尤其是晚饭后")
	last.Content = first.Content + "\n" + last.Content
	last.HistoryRecordIDs = []int64{first.LastHistoryRecordID(), last.LastHistoryRecordID()}
	engine.consolidateUserFacts(context.Background(), last, "")
	facts, err := store.ListVisibleFacts(context.Background(), "qq", "user", "private", false)
	if err != nil || len(facts) != 1 {
		t.Fatalf("facts=%v error=%v", facts, err)
	}
	sources, _, err := store.PageVisibleFactSources(context.Background(), "qq", "user", "private", false, facts[0].ID, 1, 100)
	if err != nil || len(sources) != 2 {
		t.Fatalf("merged fact lost the original evidence: sources=%v error=%v", sources, err)
	}
}

func TestModelAddDoesNotOverwriteSimilarFact(t *testing.T) {
	store, engine, provider := newFactTestEngine(t, &factTestEmbedder{}, `{"mutations":[{"action":"ADD","fact":"喜欢乌龙茶"}]}`)
	engine.consolidateUserFacts(context.Background(), factTestEvent(t, engine, "我喜欢乌龙茶"), "")
	provider.response = `{"mutations":[{"action":"ADD","fact":"常用 Neovim"}]}`
	engine.consolidateUserFacts(context.Background(), factTestEvent(t, engine, "我还常用 Neovim"), "")
	facts, err := store.ListVisibleFacts(context.Background(), "qq", "user", "private", false)
	if err != nil || len(facts) != 2 {
		t.Fatalf("explicit ADD overwrote an independent fact: facts=%v error=%v", facts, err)
	}
}

func TestFactExtractionDoesNotWaitForEmbeddingBackfill(t *testing.T) {
	store, engine, provider := newFactTestEngine(t, &factTestEmbedder{stall: true}, `{"mutations":[{"action":"ADD","fact":"常用 Neovim"}]}`)
	if err := store.ApplyFactMutations(context.Background(), "qq", "user", []db.FactMutation{{Action: "ADD", Fact: "喜欢乌龙茶"}}); err != nil {
		t.Fatal(err)
	}
	event := factTestEvent(t, engine, "我还常用 Neovim")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	engine.consolidateUserFacts(ctx, event, "")
	facts, err := store.ListVisibleFacts(context.Background(), "qq", "user", "private", false)
	if err != nil || len(facts) != 2 || provider.calls.Load() != 1 {
		t.Fatalf("embedding prevented fact storage: facts=%v chat calls=%d error=%v", facts, provider.calls.Load(), err)
	}
}

func TestModelExplicitUpdatePreservesFactIDAndEvidence(t *testing.T) {
	store, engine, provider := newFactTestEngine(t, nil, `{"mutations":[{"action":"ADD","fact":"喜欢咖啡"}]}`)
	engine.consolidateUserFacts(context.Background(), factTestEvent(t, engine, "我喜欢咖啡"), "")
	facts, err := store.ListVisibleFacts(context.Background(), "qq", "user", "private", false)
	if err != nil || len(facts) != 1 {
		t.Fatalf("facts=%v error=%v", facts, err)
	}
	id := facts[0].ID
	provider.response = fmt.Sprintf(`{"mutations":[{"action":"UPDATE","fact_id":%d,"fact":"现在喜欢乌龙茶"}]}`, id)
	engine.consolidateUserFacts(context.Background(), factTestEvent(t, engine, "我现在改喝乌龙茶了"), "")
	facts, err = store.ListVisibleFacts(context.Background(), "qq", "user", "private", false)
	if err != nil || len(facts) != 1 || facts[0].ID != id || facts[0].Content != "现在喜欢乌龙茶" {
		t.Fatalf("explicit update failed: facts=%v error=%v", facts, err)
	}
}

func TestCommitStoresFactsBeforeSlowIndexingAndStopsCleanly(t *testing.T) {
	entered := make(chan struct{}, 1)
	store, engine, _ := newFactTestEngine(t, &factTestEmbedder{stall: true, entered: entered}, `{"mutations":[{"action":"ADD","fact":"喜欢乌龙茶"}]}`)
	engine.Commit(context.Background(), factTestEvent(t, engine, "我喜欢乌龙茶"), "好的，记住你的偏好了。")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("background indexing did not start")
	}
	facts, err := store.ListVisibleFacts(context.Background(), "qq", "user", "private", false)
	if err != nil || len(facts) != 1 {
		t.Fatalf("fact was not stored before indexing: facts=%v error=%v", facts, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := engine.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown did not cancel background indexing: %v", err)
	}
}

func TestUserFactLockWaitCanBeCancelled(t *testing.T) {
	_, engine, _ := newFactTestEngine(t, nil, "")
	unlock, err := engine.lockUser(context.Background(), "qq", "user")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		release, err := engine.lockUser(ctx, "qq", "user")
		if release != nil {
			release()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected deadline while waiting, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter remained blocked on the user's fact lock")
	}
	engine.userLocksMu.Lock()
	defer engine.userLocksMu.Unlock()
	if entry := engine.userLocks[userLockKey{platform: "qq", userID: "user"}]; entry == nil || entry.refs != 1 {
		t.Fatalf("cancelled waiter leaked its lock reference: %v", entry)
	}
}

func TestFactMutationsCannotModifyUnknownOrOtherGroupFacts(t *testing.T) {
	event := &entity.SensoryEvent{Platform: "qq", IsGroup: true, ChannelID: "group-a", SenderID: "user", HistoryRecordIDs: []int64{42}}
	known := []*core.Fact{
		{ID: 1, ScopeType: "group", ScopeID: "group-a", Content: "旧事实"},
		{ID: 2, ScopeType: "group", ScopeID: "group-b", Content: "别的群的事实"},
	}
	mutations := []db.FactMutation{
		{Action: "DELETE", FactID: 999},
		{Action: "DELETE", FactID: 2},
		{Action: "UPDATE", FactID: 1, Fact: "新事实", ScopeType: "global"},
	}
	got := normalizeFactMutations(event, known, mutations)
	if len(got) != 1 || got[0].FactID != 1 || got[0].ScopeType != "group" || got[0].ScopeID != "group-a" || len(got[0].SourceMessageIDs) != 1 || got[0].SourceMessageIDs[0] != 42 || got[0].ExpectedFact != "旧事实" {
		t.Fatalf("untrusted model mutation escaped its known scope: %v", got)
	}
}
