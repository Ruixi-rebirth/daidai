package chat

import (
	"reflect"
	"strings"
	"testing"

	"daidai/internal/core"
)

func TestFitPromptContextDropsWholeOldTurn(t *testing.T) {
	messages := []core.ChatMessage{
		{Role: core.RoleSystem, Content: "系统指令"},
		{Role: core.RoleUser, Content: strings.Repeat("旧", maxPromptPayloadRunes)},
		{Role: core.RoleAssistant, Content: "旧答案"},
		{Role: core.RoleUser, Content: "近期问题"},
		{Role: core.RoleAssistant, Content: "近期答案"},
		{Role: core.RoleUser, Content: "当前问题"},
		{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{ID: "call"}}},
		{Role: core.RoleTool, ToolCallID: "call", Content: "本轮工具结果"},
	}

	want := append([]core.ChatMessage{messages[0]}, messages[3:]...)
	if got := fitPromptContext(messages, 1, 5, nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected trimmed context (length %d)", len(got))
	}
	if got := fitPromptContext(messages, len(messages)+1, 5, nil); !reflect.DeepEqual(got, messages) {
		t.Fatal("invalid history bounds changed the prompt")
	}
}
