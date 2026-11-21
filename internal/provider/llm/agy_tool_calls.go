package llm

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"daidai/internal/core"
)

func (p *AGYMindProvider) extractToolCalls(text string) []core.ToolCall {
	text = strings.TrimSpace(text)
	if opening, body, ok := strings.Cut(text, "\n"); ok {
		switch strings.TrimSpace(opening) {
		case "```json", "```":
			var closed bool
			text, closed = strings.CutSuffix(body, "\n```")
			if !closed {
				return nil
			}
		}
	}

	// A tool request occupies the whole response. JSON examples inside prose
	// or following another fenced block are ordinary assistant content.
	var payload cliToolCallPayload
	if err := json.Unmarshal([]byte(text), &payload); err != nil || len(payload.ToolCalls) == 0 {
		return nil
	}

	now := time.Now().UnixNano()
	result := make([]core.ToolCall, 0, len(payload.ToolCalls))
	for i, call := range payload.ToolCalls {
		result = append(result, core.ToolCall{
			ID:   fmt.Sprintf("call_cli_%d_%d", now, i),
			Type: "function",
			Function: core.ToolCallFunction{
				Name:      call.Name,
				Arguments: string(call.Arguments),
			},
		})
	}
	return result
}
