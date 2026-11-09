package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"daidai/internal/core"
)

func TestOpenAIChatRequest_Serialization(t *testing.T) {
	req := openAIChatRequest{
		Model: "deepseek-chat",
		Messages: []openAIMessage{
			{Role: "user", Content: "你好"},
		},
		Tools: []core.ToolDefinition{
			{
				Type: "function",
				Function: core.ToolFunctionDetail{
					Name:        "render_card",
					Description: "渲染卡片长图",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"content": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal openAIChatRequest: %v", err)
	}

	raw := string(data)
	if !strings.Contains(raw, `"tools":`) {
		t.Errorf("expected json to contain 'tools' field")
	}
	if !strings.Contains(raw, `"render_card"`) {
		t.Errorf("expected json to contain tool name 'render_card'")
	}
}
