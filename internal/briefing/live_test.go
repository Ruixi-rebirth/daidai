package briefing

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/provider/llm"
)

// Opt-in integration check. It fetches public sources and edits one issue but
// never sends a bot message or creates a subscription in the running database.
func TestBriefingLive(t *testing.T) {
	path := os.Getenv("DAIDAI_LIVE_TEST_CONFIG")
	if path == "" {
		t.Skip("live configuration not supplied")
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal("cannot load live configuration")
	}
	model := &recordingEditor{LLMProvider: llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: cfg.LLM.BaseURL, APIKey: cfg.LLM.APIKey, Model: cfg.LLM.Model})}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	store := testStore(t)
	svc, err := New(ctx, store, cfg.Briefing, model, entity.NewChannelRegistry())
	if err != nil {
		t.Fatal(err)
	}
	e, err := svc.generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("edition=%s items=%d basic=%v sources=%+v", e.ID, len(e.Items), e.Basic, e.SourceStates)
	if output := os.Getenv("DAIDAI_BRIEFING_PREVIEW_DIR"); output != "" {
		if err = os.MkdirAll(output, 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(output, "editor-response.txt"), []byte(model.response), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if e.Basic {
		t.Fatal("live editing fell back to basic view")
	}
	if len(e.Items) == 0 {
		t.Fatal("no live items selected; inspect source and editorial quality")
	}
	images, _, err := svc.Present(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("rendered pages=%d", len(images))
	if output := os.Getenv("DAIDAI_BRIEFING_PREVIEW_DIR"); output != "" {
		if err = os.MkdirAll(output, 0700); err != nil {
			t.Fatal(err)
		}
		b, err := json.MarshalIndent(e, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(output, "edition.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(output, "edition.md"), []byte(e.Text()), 0600); err != nil {
			t.Fatal(err)
		}
		for index, image := range images {
			if err = os.WriteFile(filepath.Join(output, fmt.Sprintf("page-%d.png", index+1)), image, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

type recordingEditor struct {
	core.LLMProvider
	response string
}

func (m *recordingEditor) Chat(ctx context.Context, messages []core.ChatMessage, opts *core.ChatOptions) (string, error) {
	text, err := m.LLMProvider.Chat(ctx, messages, opts)
	m.response = text
	return text, err
}
