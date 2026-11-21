package chat

import (
	"strings"
	"testing"

	"daidai/internal/entity"
)

func TestBuildSystemPromptIncludesQuoteGuidanceOnlyWhenQuoted(t *testing.T) {
	cortex := NewCortex(CortexConfig{})
	cortex.persona = entity.NewDefaultPersona()
	event := &entity.SensoryEvent{Role: entity.RolePartner}

	withoutQuote, _ := cortex.buildSystemPrompt(event, chatRequestContext{})
	if strings.Contains(withoutQuote, "引用内容用于理解本轮指代") {
		t.Fatal("quote-specific guidance should not be sent for an unquoted message")
	}
	if !strings.Contains(withoutQuote, "current_request 是本轮消息") {
		t.Fatal("system prompt should identify the current request")
	}
	if strings.Count(withoutQuote, "只有工具结果明确证明") != 1 {
		t.Fatal("operational evidence policy should have a single source")
	}

	withQuote, _ := cortex.buildSystemPrompt(event, chatRequestContext{
		QuotedMessage: &quotedMessageContext{Content: "引用正文"},
	})
	if !strings.Contains(withQuote, "引用内容用于理解本轮指代") {
		t.Fatal("quoted messages should receive quote-specific guidance")
	}
}

func TestBuildSystemPromptDescribesGroupSpeakerFormatWithoutRepeatingBehaviorRules(t *testing.T) {
	cortex := NewCortex(CortexConfig{})
	cortex.persona = entity.NewDefaultPersona()
	event := &entity.SensoryEvent{Role: entity.RolePartner, IsGroup: true}

	prompt, _ := cortex.buildSystemPrompt(event, chatRequestContext{})
	if !strings.Contains(prompt, "【群聊消息格式】近期群聊历史以 [成员名] 标记") {
		t.Fatal("group prompt should explain how speakers are marked")
	}
	if strings.Contains(prompt, "不把群友的经历或问题归到当前成员") {
		t.Fatal("speaker attribution behavior should remain in the persona rule, not be repeated in the format note")
	}
}
