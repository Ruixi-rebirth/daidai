package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"daidai/internal/db"
	"daidai/internal/entity"
)

func TestMemoryEngine_LexicalRecallAndCommit(t *testing.T) {
	tmpDir := t.TempDir()
	dbMgr := db.NewManager(tmpDir)
	t.Cleanup(func() {
		if err := dbMgr.Close(); err != nil {
			t.Error(err)
		}
	})

	ctx := context.Background()
	engine := NewMemoryEngine(nil)
	engine.SetDBManager(dbMgr)

	// 存入初始事实 (通过 SQLite 事务)
	_ = dbMgr.ApplyFactMutations(ctx, "qq", "1001", []db.FactMutation{
		{Action: "ADD", Fact: "喜欢喝乌龙茶和拿铁咖啡"},
		{Action: "ADD", Fact: "平时在写 Golang 与 Rust 后端"},
	})
	_ = dbMgr.ApplyFactMutations(ctx, "qq", "1002", []db.FactMutation{
		{Action: "ADD", Fact: "喜欢打羽毛球"},
	})

	// 检索与“茶”或“咖啡”相关的事实 (相关性优先排在首位)
	results, err := engine.RecallProfile(ctx, "qq", "1001", "请问这位朋友平时喜欢喝点什么？", 2)
	if err != nil {
		t.Fatalf("RecallProfile failed: %v", err)
	}
	if len(results) == 0 || results[0].Content != "喜欢喝乌龙茶和拿铁咖啡" {
		t.Fatalf("expected tea preference ranked first, got: %+v", results)
	}

	// 验证 RecallEpisodic 保底召回且带有 [ID: x] 标记
	episodic := engine.RecallEpisodic(ctx, &entity.SensoryEvent{
		Platform: "qq",
		SenderID: "1001",
		Content:  "今晚吃什么好呢",
	})
	if !strings.Contains(episodic, "[ID:") || !strings.Contains(episodic, "乌龙茶") || !strings.Contains(episodic, "Golang") {
		t.Fatalf("expected RecallEpisodic to include ID-tagged profile facts, got: %s", episodic)
	}

	// 验证用户隔离：1001 不应查出 1002 的羽毛球事实
	for _, f := range results {
		if strings.Contains(f.Content, "羽毛球") {
			t.Errorf("unexpected cross-user fact leak: %+v", f)
		}
	}

	// 验证 Commit 同步入库（包含 VisionSummary）与 WorkingContext 即时读取（零前缀）
	now := time.Now()
	event := &entity.SensoryEvent{
		Platform:      "qq",
		ChannelID:     "8888",
		IsGroup:       true,
		SenderID:      "1001",
		SenderName:    "阿宅",
		Content:       "大家看看这张图",
		VisionSummary: "一只橘猫趴在键盘上",
		OccurredAt:    now,
	}
	engine.Commit(ctx, event, "好胖的橘猫呀")

	nextEvent := *event
	nextEvent.HistoryRecordIDs = nil
	history := engine.WorkingContext(ctx, &nextEvent, 10)
	if len(history) != 2 {
		t.Fatalf("expected 2 working context messages immediately after Commit, got %d", len(history))
	}
	expectedUserMsg := "[阿宅]: 大家看看这张图\n[图片内容: 一只橘猫趴在键盘上]"
	if history[0].Content != expectedUserMsg {
		t.Errorf("unexpected user message in group history: %q, want %q", history[0].Content, expectedUserMsg)
	}
	if history[1].Content != "好胖的橘猫呀" {
		t.Errorf("unexpected assistant message in group history: %q", history[1].Content)
	}

	// 验证跨 2 小时闲置断档后：WorkingContext 自动截断，同时 RecallEpisodic 自动注入上一场对话历史脉络摘要
	laterEvent := &entity.SensoryEvent{
		Platform:   "qq",
		ChannelID:  "8888",
		IsGroup:    true,
		SenderID:   "1001",
		SenderName: "阿宅",
		Content:    "刚才那只猫可爱吗",
		OccurredAt: now.Add(3 * time.Hour),
	}
	laterWorking := engine.WorkingContext(ctx, laterEvent, 10)
	if len(laterWorking) != 0 {
		t.Fatalf("expected active WorkingContext to be empty after 3h idle gap, got %d", len(laterWorking))
	}
	laterEpisodic := engine.RecallEpisodic(ctx, laterEvent)
	if !strings.Contains(laterEpisodic, "上一场对话历史脉络") || !strings.Contains(laterEpisodic, "好胖的橘猫呀") {
		t.Fatalf("expected RecallEpisodic to include previous session digest after 3h idle gap, got:\n%s", laterEpisodic)
	}

	// 验证无空格连续中文句子（二元词组拆解 + BM25）能召回历史消息
	searched, err := SearchMessagesInChatByBM25(ctx, dbMgr, "qq", "group", "8888", "那只橘猫为什么在键盘上面", 5)
	if err != nil || len(searched) == 0 {
		t.Fatalf("expected SearchMessagesByBM25 to match unspaced Chinese query, got len=%d err=%v", len(searched), err)
	}
}

type mockEmbedder struct{}

func (m *mockEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if strings.Contains(text, "Cilium") || strings.Contains(text, "容器网络") {
		return []float32{0.95, 0.05, 0.0}, nil
	}
	return []float32{0.0, 0.95, 0.05}, nil
}

func TestMemoryEngine_HybridBM25AndVectorRRF(t *testing.T) {
	tmpDir := t.TempDir()
	dbMgr := db.NewManager(tmpDir)
	t.Cleanup(func() {
		if err := dbMgr.Close(); err != nil {
			t.Error(err)
		}
	})

	ctx := context.Background()
	engine := NewMemoryEngine(nil)
	engine.SetDBManager(dbMgr)
	engine.SetEmbedder(&mockEmbedder{})
	defer SetDefaultEmbedder(nil)

	msgID, err := dbMgr.LogMessage(ctx, "qq", "group", "9999", "1001", "阿宅", "我们线上集群全部切到了 Cilium 和 eBPF", false, "", "")
	if err != nil {
		t.Fatalf("LogMessage failed: %v", err)
	}
	vec, _ := (&mockEmbedder{}).Embed(ctx, "Cilium eBPF")
	_ = dbMgr.UpdateMessageEmbedding(ctx, "qq", msgID, vec, embeddingIdentity(&mockEmbedder{}))

	// 查询词“容器网络方案”与原文“Cilium 和 eBPF”零字面重合，验证稠密向量 + RRF 双路混合成功召回
	hits, err := SearchMessagesInChatByBM25(ctx, dbMgr, "qq", "group", "9999", "容器网络方案", 5)
	if err != nil || len(hits) != 1 || !strings.Contains(hits[0].Text, "Cilium") {
		t.Fatalf("expected zero-lexical-overlap synonym query to recall Cilium message via vector+RRF, got: %+v (err=%v)", hits, err)
	}
}

func TestWorkingContextStopsBeforeCurrentTurnEvenWithQueuedMessages(t *testing.T) {
	mgr := db.NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := mgr.Close(); err != nil {
			t.Error(err)
		}
	})
	engine := NewMemoryEngine(nil)
	engine.SetDBManager(mgr)
	ctx := context.Background()
	makeEvent := func(id, content string) *entity.SensoryEvent {
		return &entity.SensoryEvent{Platform: "telegram", ChannelID: "group", IsGroup: true, SenderID: "tg:1", SenderName: "用户", RawMessageID: id, Content: content, OccurredAt: time.Now()}
	}
	previous := makeEvent("1", "先前的消息")
	current := makeEvent("2", "当前请求")
	mergedEnd := makeEvent("3", "本轮追加内容")
	for _, event := range []*entity.SensoryEvent{previous, current, mergedEnd} {
		if err := engine.RecordInbound(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	// Enough later messages to crowd earlier history out of an unbounded query.
	for i := 0; i < 20; i++ {
		event := makeEvent(fmt.Sprint(i+4), "后来的排队消息")
		if err := engine.RecordInbound(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	history := engine.WorkingContext(ctx, current, 2)
	if len(history) != 1 || !strings.Contains(history[0].Content, "先前的消息") {
		t.Fatalf("current or future message leaked into history: %+v", history)
	}
	mergedEnd.HistoryRecordIDs = []int64{current.LastHistoryRecordID(), mergedEnd.LastHistoryRecordID()}
	history = engine.WorkingContext(ctx, mergedEnd, 2)
	if len(history) != 1 || !strings.Contains(history[0].Content, "先前的消息") {
		t.Fatalf("merged turn duplicated in history: %+v", history)
	}
	if err := mgr.ResetSessionHistoryInChat("telegram", "group", "group"); err != nil {
		t.Fatal(err)
	}
	if history = engine.WorkingContext(ctx, current, 2); len(history) != 0 {
		t.Fatal("history cutoff bypassed reset boundary")
	}
}

func TestRecallProfileActivatesSemanticRerankAbovePromptLimit(t *testing.T) {
	mgr := db.NewManager(t.TempDir())
	t.Cleanup(func() { _ = mgr.Close() })

	ctx := context.Background()
	engine := NewMemoryEngine(nil)
	engine.SetDBManager(mgr)
	emb := &mockEmbedder{}
	engine.SetEmbedder(emb)
	defer SetDefaultEmbedder(nil)

	idModel := embeddingIdentity(emb)
	otherVec, _ := emb.Embed(ctx, "普通爱好")
	targetVec, _ := emb.Embed(ctx, "Cilium eBPF")

	// Insert 15 unrelated facts + 1 target semantic fact (#4 in insertion order, outside oldest-3 baseline if pushed down, or #14 so newest bias doesn't save it if 15..16 come after).
	var mutations []db.FactMutation
	for i := 1; i <= 15; i++ {
		if i == 8 {
			mutations = append(mutations, db.FactMutation{
				Action: "ADD", Fact: "线上网络使用 Cilium 与 eBPF", ScopeType: "global",
				Embedding: targetVec, EmbeddingModel: idModel,
			})
		}
		mutations = append(mutations, db.FactMutation{
			Action: "ADD", Fact: fmt.Sprintf("日常记录条目编号 %d", i), ScopeType: "global",
			Embedding: otherVec, EmbeddingModel: idModel,
		})
	}
	if err := mgr.ApplyFactMutations(ctx, "qq", "owner", mutations); err != nil {
		t.Fatal(err)
	}

	ev := &entity.SensoryEvent{Platform: "qq", SenderID: "owner", Content: "容器网络方案怎么选"}
	recalled, err := engine.RecallProfileForEvent(ctx, ev, ev.Content, maxPromptFacts)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range recalled {
		if strings.Contains(f.Content, "Cilium") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected semantic vector reranking to recall Cilium fact within %d prompt limit when user has 16 facts (< 40), got: %+v", maxPromptFacts, recalled)
	}
}

func TestCommitSkipsSlashCommandsAndCleansEphemeralControlTurns(t *testing.T) {
	mgr := db.NewManager(t.TempDir())
	t.Cleanup(func() { _ = mgr.Close() })
	ctx := context.Background()
	engine := NewMemoryEngine(nil)
	engine.SetDBManager(mgr)

	// 1. Unknown slash command reply must not leave an orphan bot row in messages.
	slashEv := &entity.SensoryEvent{Platform: "qq", ChannelID: "chat-1", SenderID: "owner", Content: "/unknown_cmd"}
	if err := engine.RecordInbound(ctx, slashEv); err != nil {
		t.Fatal(err)
	}
	engine.Commit(ctx, slashEv, "未知指令回复")

	// 2. Natural-language ephemeral control turn (e.g. switch_model / switch_workspace) cleans up pre-logged inbound row and skips bot reply.
	ctrlEv := &entity.SensoryEvent{Platform: "qq", ChannelID: "chat-1", SenderID: "owner", RawMessageID: "m-ctrl", Content: "把公用模型调到 low"}
	if err := engine.RecordInbound(ctx, ctrlEv); err != nil {
		t.Fatal(err)
	}
	if len(ctrlEv.HistoryRecordIDs) != 1 {
		t.Fatalf("expected pre-logged inbound row before Think, got %v", ctrlEv.HistoryRecordIDs)
	}
	ctrlEv.EphemeralControlTurn = true
	engine.Commit(ctx, ctrlEv, "调好了，公用模型已经是 low")

	recents, err := mgr.RecentMessages(ctx, "qq", "private", "chat-1", 10)
	if err != nil || len(recents) != 0 {
		t.Fatalf("expected zero polluted command/control messages in DB, got %+v (err=%v)", recents, err)
	}
}

func TestGroupRecipientTrackingQuoteAnchorsAndInterjectionScope(t *testing.T) {
	mgr := db.NewManager(t.TempDir())
	t.Cleanup(func() { _ = mgr.Close() })
	ctx := context.Background()
	engine := NewMemoryEngine(nil)
	engine.SetDBManager(mgr)
	now := time.Now()

	// 1. Alice asks Daidai -> Daidai answers Alice (ReplyToUID = alice)
	aliceEv := &entity.SensoryEvent{
		Platform: "qq", IsGroup: true, ChannelID: "g-99",
		SenderID: "u-alice", SenderName: "Alice", Content: "帮我看下 Nix 编译报错",
		RawMessageID: "m1", OccurredAt: now,
	}
	engine.Commit(ctx, aliceEv, "把完整报错贴出来看看")

	// 2. Unaddressed room-wide interjection / chatter must NOT bind ReplyToUID to Bob
	bobChatter := &entity.SensoryEvent{
		Platform: "qq", IsGroup: true, ChannelID: "g-99",
		SenderID: "u-bob", SenderName: "Bob", Content: "今天天气真不错",
		RawMessageID: "m2", GroupInterjection: true, OccurredAt: now.Add(10 * time.Second),
	}
	engine.Commit(ctx, bobChatter, "适合晒太阳")

	// 3. Bob quotes Alice's message with a short reaction ("笑死")
	bobQuote := &entity.SensoryEvent{
		Platform: "qq", IsGroup: true, ChannelID: "g-99",
		SenderID: "u-bob", SenderName: "Bob", Content: "笑死",
		ReplyContext: &entity.SensoryEvent{SenderName: "Alice", Content: "帮我看下 Nix 编译报错"},
		RawMessageID: "m3", GroupMessageObserved: true, OccurredAt: now.Add(20 * time.Second),
	}
	if err := engine.RecordInbound(ctx, bobQuote); err != nil {
		t.Fatal(err)
	}

	// 4. When Bob now talks to Daidai:
	// - Alice's bot reply is marked "[对 Alice说]: ..."
	// - Room-wide interjection has empty ReplyToUID (not bound to Bob's peer thread)
	// - Bob's quote preserves "[回复 Alice: 「帮我看下 Nix 编译报错」] 笑死"
	bobTurn := &entity.SensoryEvent{
		Platform: "qq", IsGroup: true, ChannelID: "g-99",
		SenderID: "u-bob", SenderName: "Bob", Content: "今晚吃啥",
		RawMessageID: "m4", OccurredAt: now.Add(30 * time.Second),
	}
	working := engine.WorkingContext(ctx, bobTurn, 10)
	var all strings.Builder
	for _, m := range working {
		all.WriteString(m.Content + "\n")
	}
	got := all.String()
	if !strings.Contains(got, "[对 Alice说]: 把完整报错贴出来看看") {
		t.Fatalf("expected bot reply to other member to be labeled [对 Alice说], got:\n%s", got)
	}
	if !strings.Contains(got, "[Bob]: [回复 Alice: 「帮我看下 Nix 编译报错」] 笑死") {
		t.Fatalf("expected quoted short reaction to retain compact quote anchor, got:\n%s", got)
	}

	stored, err := mgr.RecentMessages(ctx, "qq", "group", "g-99", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range stored {
		if msg.IsBot && msg.Text == "适合晒太阳" && msg.ReplyToUID != "" {
			t.Fatalf("unaddressed group interjection should have empty ReplyToUID, got %q", msg.ReplyToUID)
		}
	}
}

func TestRecallEpisodic_RecallsRelevantOlderGroupMessages(t *testing.T) {
	ctx := context.Background()
	mgr := db.NewManager(t.TempDir())
	defer func() { _ = mgr.Close() }()
	engine := NewMemoryEngine(nil)
	engine.SetDBManager(mgr)

	if _, err := mgr.LogMessage(ctx, "qq", "group", "g-denial", "u1", "群友", "原来 denial 是个用 Rust 和 Smithay 写的 Wayland 合成器，外壳用 Flutter 做", true, "", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 45; i++ {
		if _, err := mgr.LogMessage(ctx, "qq", "group", "g-denial", "u2", "路人", "今天中午吃什么好呢", false, "", ""); err != nil {
			t.Fatal(err)
		}
	}

	episodic := engine.RecallEpisodic(ctx, &entity.SensoryEvent{
		Platform:   "qq",
		IsGroup:    true,
		ChannelID:  "g-denial",
		SenderID:   "u1",
		SenderName: "群友",
		Content:    "arm linux最好用的桌面是不是 denial mobile",
		OccurredAt: time.Now(),
	})
	if !strings.Contains(episodic, "Wayland 合成器") || !strings.Contains(episodic, "denial") {
		t.Fatalf("expected RecallEpisodic to surface older denial context beyond 40-message window, got:\n%s", episodic)
	}
}
