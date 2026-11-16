package tool

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/engine/chat"
	"daidai/internal/entity"
	"daidai/internal/groupchat"
	"daidai/internal/provider/llm"
)

// Opt-in native language routing against a temporary database. No channel is
// registered, no real group is subscribed and no message is sent.
func TestGroupExpressionNaturalLanguageLive(t *testing.T) {
	path := os.Getenv("DAIDAI_LIVE_TEST_CONFIG")
	if path == "" {
		t.Skip("live configuration not supplied")
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal("cannot load live configuration")
	}
	database := db.NewManager(t.TempDir())
	t.Cleanup(func() { _ = database.Close() })
	store := &groupchat.Store{Database: database}
	provider := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: cfg.LLM.BaseURL, APIKey: cfg.LLM.APIKey, Model: cfg.LLM.Model})
	router := llm.NewMindRouter(llm.MindRouterConfig{StatePath: filepath.Join(t.TempDir(), "mind.json"), FallbackChannel: "API"})
	router.RegisterChannel(llm.ChannelRegistration{Channel: "API", Provider: provider})
	hands := &HandsRegistry{tools: map[string]core.Tool{}}
	hands.Register(NewGroupExpressionTool(store))
	cortex := chat.NewCortex(chat.CortexConfig{LLM: router, Hands: hands})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for n, text := range []string{"在这个群开启碎碎念，做梦先保持关闭。", "让这个群也可以偶尔做梦，碎碎念开关别改。"} {
		_, err = cortex.Think(ctx, &entity.SensoryEvent{Platform: "telegram", ChannelID: "fixture-group", SenderID: "fixture-owner", IsGroup: true, Role: entity.RolePartner, Content: text, OccurredAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		state, err := store.Get(ctx, "telegram", "fixture-group")
		if err != nil || !state.Chatter || state.Dream != (n == 1) {
			t.Fatalf("wrong group switches: %+v %v", state, err)
		}
	}
}
