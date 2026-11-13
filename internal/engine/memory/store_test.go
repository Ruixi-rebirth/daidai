package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"daidai/internal/db"
	"daidai/internal/entity"
)

func TestMemoryEngine_LexicalRecallAndCommit(t *testing.T) {
	tmpDir := t.TempDir()
	dbMgr := db.NewManager(tmpDir)
	defer dbMgr.Close()

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

	history := engine.WorkingContext(ctx, event, 10)
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
	searched, err := SearchMessagesByBM25(ctx, dbMgr, "qq", "8888", "那只橘猫为什么在键盘上面", 5)
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
	defer dbMgr.Close()

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
	hits, err := SearchMessagesByBM25(ctx, dbMgr, "qq", "9999", "容器网络方案", 5)
	if err != nil || len(hits) != 1 || !strings.Contains(hits[0].Text, "Cilium") {
		t.Fatalf("expected zero-lexical-overlap synonym query to recall Cilium message via vector+RRF, got: %+v (err=%v)", hits, err)
	}
}
