package chat

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/provider/llm"
)

type liveGroupModel struct {
	core.LLMProvider
	content string
}

func (m *liveGroupModel) ChatWithTools(ctx context.Context, messages []core.ChatMessage, tools []core.ToolDefinition, options *core.ChatOptions) (*core.ChatResponse, error) {
	response, err := m.LLMProvider.ChatWithTools(ctx, messages, tools, options)
	if response != nil {
		m.content = response.Content
	}
	return response, err
}

// Native read-only decision using synthetic group messages and temporary state.
func TestGroupDecisionLive(t *testing.T) {
	path := os.Getenv("DAIDAI_LIVE_TEST_CONFIG")
	if path == "" {
		t.Skip("live configuration not supplied")
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal("cannot load live configuration")
	}
	cortex, _, _, event := expressionFixture(t)
	model := &liveGroupModel{LLMProvider: llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: cfg.LLM.BaseURL, APIKey: cfg.LLM.APIKey, Model: cfg.LLM.Model})}
	cortex.llm = model
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	_, err = cortex.Think(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	var decision groupDecision
	if err = json.Unmarshal([]byte(model.content), &decision); err != nil {
		t.Fatal("model did not return a structured group decision")
	}
	switch decision.Mode {
	case "skip", "reply", "chatter", "dream":
	default:
		t.Fatal("invalid group decision mode")
	}
	t.Logf("group decision mode=%s", decision.Mode)
}
