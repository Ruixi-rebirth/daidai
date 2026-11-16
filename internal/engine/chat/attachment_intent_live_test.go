package chat

import (
	"context"
	"os"
	"testing"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/provider/llm"
)

// Set DAIDAI_LIVE_TEST_CONFIG to opt into real model requests using that config's
// API provider. Only synthetic attachment metadata is sent, never file contents.
func TestAttachmentIntentLive(t *testing.T) {
	path := os.Getenv("DAIDAI_LIVE_TEST_CONFIG")
	if path == "" {
		t.Skip("live configuration not supplied")
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal("cannot load live configuration")
	}
	model := llm.NewOpenAIProvider(llm.OpenAIConfig{BaseURL: cfg.LLM.BaseURL, APIKey: cfg.LLM.APIKey, Model: cfg.LLM.Model})
	cortex := NewCortex(CortexConfig{LLM: model})
	image := &core.Attachment{Name: "image.png", MimeType: "image/png"}
	cases := []struct {
		name, text       string
		current, pending []*core.Attachment
		want             attachmentDisposition
	}{
		{"followup_read", "看看说的啥", nil, []*core.Attachment{image}, attachmentProcess},
		{"followup_look", "看看", nil, []*core.Attachment{image}, attachmentProcess},
		{"current_read", "这是什么意思", []*core.Attachment{image}, nil, attachmentProcess},
		{"upload_only", "", []*core.Attachment{image}, nil, attachmentAsk},
		{"unrelated", "你好", nil, []*core.Attachment{image}, attachmentIgnore},
		{"ambiguous", "看看", nil, []*core.Attachment{image, image}, attachmentAsk},
		{"specific_file", "帮我处理", []*core.Attachment{{Name: "main.go", MimeType: "text/plain"}}, nil, attachmentAsk},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cortex.attachmentIntent(context.Background(), &entity.SensoryEvent{Platform: "qq", Content: tc.text, Images: tc.current, PendingImages: tc.pending})
			if err != nil {
				t.Fatal("classification failed")
			}
			t.Logf("decision=%s current=%v pending=%v", got.Disposition, got.CurrentImages, got.PendingImages)
			if got.Disposition != tc.want {
				t.Errorf("want %s, got %s", tc.want, got.Disposition)
			}
			if tc.want == attachmentProcess {
				selected := got.CurrentImages
				if len(tc.pending) > 0 {
					selected = got.PendingImages
				}
				if len(selected) != 1 || selected[0] != 0 {
					t.Errorf("expected the single matching image, got %v", selected)
				}
			}
		})
	}
}
