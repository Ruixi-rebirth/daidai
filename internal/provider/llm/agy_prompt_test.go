package llm

import (
	"testing"

	"daidai/internal/core"
)

func TestStripAGYToolEchoKeepsUserFacingMarkdown(t *testing.T) {
	messages := []core.ChatMessage{
		{Role: core.RoleTool, Name: "read_link", Content: "工具正文\n第二行"},
		{Role: core.RoleTool, Name: "search_web", Content: "搜索结果"},
	}
	const answer = "下面是原样保留的示例：\n\n[工具 使用说明](https://example.com)\n\n~~~text\n[工具 read_link 执行结果]: 工具正文\n第二行\n~~~"
	echoes := formatAGYToolResult(messages[0]) + "\n\n" + formatAGYToolResult(messages[1])
	for _, tc := range []struct{ input, want string }{
		{answer, answer},
		{answer + "\n\n" + echoes, answer},
		{echoes, ""},
		{answer + "\n\n[工具 read_link 执行结果]: 这段是独立说明", answer + "\n\n[工具 read_link 执行结果]: 这段是独立说明"},
	} {
		if got := stripAGYToolEcho(tc.input, messages); got != tc.want {
			t.Fatalf("got %q, want %q", got, tc.want)
		}
	}
}

func TestAGYToolEnvelopeMustBeEntireReply(t *testing.T) {
	const payload = `{"tool_calls":[{"name":"render_card","arguments":{}}]}`
	const fenced = "```json\n" + payload + "\n```"
	provider := &AGYMindProvider{}
	for _, tc := range []struct {
		text string
		want int
	}{
		{payload, 1},
		{fenced, 1},
		{"示例：\n" + fenced, 0},
		{fenced + "\n以上只是样例", 0},
		{"前缀 " + payload, 0},
		{"```json\n" + payload, 0},
		{"```text\n先前代码\n```\n" + fenced, 0},
	} {
		if got := len(provider.extractToolCalls(tc.text)); got != tc.want {
			t.Fatalf("tool calls=%d, want %d for %q", got, tc.want, tc.text)
		}
	}
}

func TestAGYToolEnvelopePreservesArgumentsForValidation(t *testing.T) {
	provider := &AGYMindProvider{}
	calls := provider.extractToolCalls(`{"tool_calls":[{"name":"render_card","arguments":null},{"name":"render_card"},{"name":"render_card","arguments":{}}]}`)
	if len(calls) != 3 || calls[0].Function.Arguments != "null" || calls[1].Function.Arguments != "" || calls[2].Function.Arguments != "{}" {
		t.Fatalf("AGY changed the model's arguments: %+v", calls)
	}
}

func TestAGYRejectsToolsAbsentFromItsPrompt(t *testing.T) {
	provider := &AGYMindProvider{}
	tools := agyHostTools([]core.ToolDefinition{
		{Function: core.ToolFunctionDetail{Name: "search_web"}},
		{Function: core.ToolFunctionDetail{Name: "render_card"}},
	})
	result := AGYResult{Response: `{"tool_calls":[{"name":"search_web","arguments":{}}]}`}
	if response, err := provider.chatResponse(result, "AGY", nil, tools); err == nil || response != nil {
		t.Fatalf("hidden tool escaped the provider: %+v, %v", response, err)
	}
}
