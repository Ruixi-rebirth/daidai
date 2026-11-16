package tool

import (
	"context"
	"daidai/internal/briefing"
	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/engine/chat"
	"daidai/internal/entity"
	"daidai/internal/provider/llm"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This checks natural-language routing and prepared presentation, without
// calling a message channel or touching live subscription/history databases.
func TestBriefingNaturalLanguageLive(t *testing.T) {
	cfgPath, preview := os.Getenv("DAIDAI_LIVE_TEST_CONFIG"), os.Getenv("DAIDAI_BRIEFING_PREVIEW_DIR")
	if cfgPath == "" || preview == "" {
		t.Skip("live configuration and preview not supplied")
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatal("cannot load live configuration")
	}
	data, err := os.ReadFile(filepath.Join(preview, "edition.json"))
	if err != nil {
		t.Fatal(err)
	}
	var e briefing.Edition
	if err = json.Unmarshal(data, &e); err != nil {
		t.Fatal(err)
	}
	store, err := briefing.Open(filepath.Join(t.TempDir(), "briefing.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err = store.Begin(ctx, e.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = store.Save(ctx, &e); err != nil {
		t.Fatal(err)
	}
	model := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: cfg.LLM.BaseURL, APIKey: cfg.LLM.APIKey, Model: cfg.LLM.Model})
	router := llm.NewMindRouter(llm.MindRouterConfig{StatePath: filepath.Join(t.TempDir(), "mind.json"), FallbackChannel: "API"})
	router.RegisterChannel(llm.ChannelRegistration{Channel: "API", Provider: model})
	svc, err := briefing.New(ctx, store, cfg.Briefing, model, entity.NewChannelRegistry())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := svc.Shutdown(stop); err != nil {
			t.Error(err)
		}
	})
	hands := &HandsRegistry{tools: map[string]core.Tool{}}
	hands.Register(NewBriefingTool(svc, map[string]string{"telegram": "fixture"}, false))
	cortex := chat.NewCortex(chat.CortexConfig{LLM: router, Hands: hands})
	result, err := cortex.Think(ctx, &entity.SensoryEvent{Platform: "telegram", ChannelID: "fixture", SenderID: "fixture", Role: entity.RolePartner, Content: "今天的极客早报，直接给我图和原文入口", OccurredAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || len(result.Images) == 0 || result.Text != e.Index() || len(result.ImageSources) != len(result.Images) || !result.FixedPresentation {
		if result == nil {
			t.Fatal("empty result")
		}
		t.Fatalf("wrong presentation: pages=%d text=%q", len(result.Images), result.Text)
	}
	t.Logf("natural-language request returned %d pages and exact source index", len(result.Images))
	if strings.Contains(result.Text, "完成") || strings.Contains(result.Text, "见图") {
		t.Fatal("unnecessary completion caption leaked")
	}
}
