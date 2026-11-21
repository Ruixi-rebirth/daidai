package entity

import (
	"strings"
	"testing"
)

func TestCompilePromptScopesGroupRules(t *testing.T) {
	persona := NewDefaultPersona()
	privatePrompt := persona.CompilePromptFor(&SensoryEvent{Platform: "qq", Role: RolePartner}, true)
	groupPrompt := persona.CompilePromptFor(&SensoryEvent{Platform: "qq", IsGroup: true, Role: RolePartner}, true)
	groupRule := "群聊单点聚焦与单线流动"

	if strings.Contains(privatePrompt, groupRule) {
		t.Fatal("private conversation prompt should not include group-only rules")
	}
	if !strings.Contains(groupPrompt, groupRule) {
		t.Fatal("group conversation prompt should include group-only rules")
	}
	guestPrompt := persona.CompilePromptFor(&SensoryEvent{Role: RoleGuest}, true)
	noToolsPrompt := persona.CompilePromptFor(&SensoryEvent{Role: RolePartner}, false)
	for _, prompt := range []string{guestPrompt, noToolsPrompt, groupPrompt} {
		if strings.Contains(prompt, "系统状态和工程操作以工具实际结果为准") {
			t.Fatal("operational evidence policy belongs to the chat policy, not persona")
		}
	}
}
