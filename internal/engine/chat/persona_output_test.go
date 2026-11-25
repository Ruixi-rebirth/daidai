package chat

import (
	"context"
	"strings"
	"testing"

	"daidai/internal/core"
	"daidai/internal/entity"
)

type staticReplyLLM struct {
	core.LLMProvider
	reply    string
	messages []core.ChatMessage
}

func (*staticReplyLLM) Name() string { return "static-test-llm" }
func (m *staticReplyLLM) ChatWithTools(_ context.Context, messages []core.ChatMessage, _ []core.ToolDefinition, _ *core.ChatOptions) (*core.ChatResponse, error) {
	m.messages = messages
	return &core.ChatResponse{Content: m.reply}, nil
}

func TestPersonaOutputExtractsBundledStickerAndCleansStageDirections(t *testing.T) {
	model := &staticReplyLLM{reply: "（懒懒地瞥了一眼）这有啥难的 [脸:得意]"}
	c := NewCortex(CortexConfig{LLM: model, Persona: entity.NewDefaultPersona()})

	out, err := c.Think(context.Background(), &entity.SensoryEvent{
		Platform:  "qq",
		IsGroup:   true,
		ChannelID: "g1",
		SenderID:  "u1",
		Content:   "这题你会吗",
	})
	if err != nil {
		t.Fatalf("Think failed: %v", err)
	}
	if out == nil || out.Text != "这有啥难的" {
		t.Fatalf("expected clean text without stage direction or face tag, got %+v", out)
	}
	if len(out.Images) != 1 || len(out.Images[0]) == 0 {
		t.Fatalf("expected bundled face sticker attached to utterance, got images=%d", len(out.Images))
	}
}

func TestPersonaOutputPureStickerPreservesHistoryAndFallback(t *testing.T) {
	model := &staticReplyLLM{reply: "【脸：无语】"}
	c := NewCortex(CortexConfig{LLM: model, Persona: entity.NewDefaultPersona()})

	out, err := c.Think(context.Background(), &entity.SensoryEvent{
		Platform:  "telegram",
		ChannelID: "chat1",
		SenderID:  "u1",
		Content:   "讲个冷笑话",
	})
	if err != nil {
		t.Fatalf("Think failed: %v", err)
	}
	if out == nil || out.Text != "" || len(out.Images) != 1 {
		t.Fatalf("expected pure sticker utterance with empty visible text, got %+v", out)
	}
	if !strings.Contains(out.MediaHistory, "无语") || out.FallbackText != "[无语]" {
		t.Fatalf("expected sticker media history and fallback text, got history=%q fallback=%q", out.MediaHistory, out.FallbackText)
	}
}

func TestSanitizePersonaTextPreservesCodeAndTechnicalParentheses(t *testing.T) {
	raw := "时间复杂度（最坏情况）是 O(n)，配置参考 `[脸:开心]`：\n```\n（懒懒地瞥了一眼）\n[脸:无语]\n```"
	cleaned, emo, dropped := sanitizePersonaText(raw, "怎么优化", false, false)
	if dropped || emo != "" {
		t.Fatalf("code spans should not trigger sticker extraction or drop: emo=%q dropped=%v", emo, dropped)
	}
	if cleaned != raw {
		t.Fatalf("technical parentheses or code block was altered:\ngot:  %q\nwant: %q", cleaned, raw)
	}
}

func TestSanitizePersonaTextInterceptsAISelfDisclosureAndSelfProof(t *testing.T) {
	cleaned, _, dropped := sanitizePersonaText("作为一个AI语言模型，我没有真实的感情。", "今天心情怎么样", false, false)
	if dropped || strings.Contains(cleaned, "语言模型") || cleaned == "" {
		t.Fatalf("expected AI self-disclosure to be replaced with brief dismissal, got %q", cleaned)
	}

	proofCleaned, _, _ := sanitizePersonaText("你才是人机！我是真人我有感情的！", "呆呆你是人机吧", false, false)
	if strings.Contains(proofCleaned, "真人") || strings.Contains(proofCleaned, "你才是") {
		t.Fatalf("expected defensive self-proof to be intercepted, got %q", proofCleaned)
	}

	_, _, interjectionDropped := sanitizePersonaText("我是人工智能助手", "大家早", false, true)
	if !interjectionDropped {
		t.Fatal("expected unprompted group interjection with AI self-disclosure to drop silently")
	}

	partnerCleaned, _, _ := sanitizePersonaText("当前使用的是大语言模型 API 通道", "查看当前模型状态", true, false)
	if !strings.Contains(partnerCleaned, "API 通道") {
		t.Fatalf("partner system query should not be intercepted, got %q", partnerCleaned)
	}
}

func TestRecentPersonaTicHintDetectsRepeatedPropsUnlessUserAsks(t *testing.T) {
	history := []core.ChatMessage{
		{Role: core.RoleUser, Content: "在干嘛"},
		{Role: core.RoleAssistant, Content: "窝着撸猫呢，有点犯困"},
	}
	hint := recentPersonaTicHint(history, "帮我看下这个报错")
	if !strings.Contains(hint, "猫/撸猫") || !strings.Contains(hint, "窝着/瘫着") || !strings.Contains(hint, "犯困/低电量") {
		t.Fatalf("expected anti-tic hint for repeated props, got %q", hint)
	}

	catAskedHint := recentPersonaTicHint(history, "你家小猫叫什么")
	if strings.Contains(catAskedHint, "猫/撸猫") {
		t.Fatalf("should not suppress cat topic when user explicitly asks about cat: %q", catAskedHint)
	}
}
