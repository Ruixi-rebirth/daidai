package entity

import (
	"strings"
	"testing"
	"time"
)

func TestCompilePromptScopesGroupAndPartnerRules(t *testing.T) {
	persona := NewDefaultPersona()
	privatePrompt := persona.CompilePromptFor(&SensoryEvent{Platform: "qq", Role: RolePartner}, true)
	groupPrompt := persona.CompilePromptFor(&SensoryEvent{Platform: "qq", IsGroup: true, Role: RolePartner}, true)
	groupRule := persona.GroupRules[0].Text
	partnerRule := persona.PartnerRules[0].Text

	if strings.Contains(privatePrompt, groupRule) {
		t.Fatal("private conversation prompt should not include group-only rules")
	}
	if !strings.Contains(groupPrompt, groupRule) {
		t.Fatal("group conversation prompt should include group-only rules")
	}
	if !strings.Contains(privatePrompt, partnerRule) || !strings.Contains(groupPrompt, partnerRule) {
		t.Fatal("partner prompt should include best-friend partner rapport rule")
	}
	if !strings.Contains(groupPrompt, "[脸:X]") {
		t.Fatal("messaging channel prompt should include sticker instructions")
	}
	if strings.Contains(privatePrompt, "补一句可以换行") || !strings.Contains(privatePrompt, "写在同一行里") || !strings.Contains(privatePrompt, "不加生硬的句号") {
		t.Fatal("casual chat expression rule should instruct single-line IM phrasing without trailing formal periods")
	}
	if strings.Contains(privatePrompt, "不必每句硬接梗") || !strings.Contains(privatePrompt, "敏锐网感") {
		t.Fatal("persona should preserve internet meme literacy rather than dampening playful banter")
	}
	if strings.Contains(groupPrompt, "（如‘？’") || strings.Contains(groupPrompt, "‘你好怪’）") {
		t.Fatal("persona prompt must not provide canned robotic catchphrases like 你好怪 or leading question mark templates")
	}

	voice := persona.CompileGroupVoicePrompt()
	for _, rule := range persona.Rules {
		if rule.Heading == personaExpressionHeading && (!strings.Contains(voice, rule.Text) || !strings.Contains(groupPrompt, rule.Text)) {
			t.Fatal("group replies and interjections should use the same conversational style")
		}
	}

	guestPrompt := persona.CompilePromptFor(&SensoryEvent{Platform: "qq", Role: RoleGuest}, true)
	if strings.Contains(guestPrompt, partnerRule) {
		t.Fatal("guest prompt must not include partner-only rapport rules")
	}
	cliPrompt := persona.CompilePromptFor(&SensoryEvent{Platform: "cli", Role: RolePartner}, false)
	if strings.Contains(cliPrompt, persona.StickerRule.Text) {
		t.Fatal("non-messaging CLI prompt should not advertise sticker tags")
	}
	noToolsPrompt := persona.CompilePromptFor(&SensoryEvent{Role: RolePartner}, false)
	for _, prompt := range []string{guestPrompt, noToolsPrompt, groupPrompt} {
		if strings.Contains(prompt, "系统状态和工程操作以工具实际结果为准") {
			t.Fatal("operational evidence policy belongs to the chat policy, not persona")
		}
	}
}

func TestCircadianRhythmIsStableWithinWindowAndShiftsAcrossDay(t *testing.T) {
	t1 := time.Date(2026, 11, 25, 3, 10, 0, 0, personaZone)
	t2 := time.Date(2026, 11, 25, 5, 50, 0, 0, personaZone)
	evening := time.Date(2026, 11, 25, 20, 30, 0, 0, personaZone)

	late1 := CircadianRhythm(t1)
	late2 := CircadianRhythm(t2)
	eve := CircadianRhythm(evening)

	if late1 != late2 {
		t.Fatalf("expected stable rhythm within same 3-hour window, got %q vs %q", late1, late2)
	}
	if !strings.Contains(late1, "深夜") || !strings.Contains(eve, "夜晚") || late1 == eve {
		t.Fatalf("expected distinct phase rhythms across day, got late=%q evening=%q", late1, eve)
	}
}
