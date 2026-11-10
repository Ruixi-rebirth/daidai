package llm

import (
	"context"
	"strings"
	"testing"

	"daidai/internal/core"
)

func TestFormatAGYUsageOutput(t *testing.T) {
	rawJSON := []byte(`{
		"conversation_id": "",
		"status": "SUCCESS",
		"response": "Fallback text",
		"command": {
			"name": "usage",
			"data": {
				"description": "Group share description",
				"groups": [
					{
						"name": "Gemini Models",
						"description": "Models within this group: Gemini Flash, Gemini Pro",
						"buckets": [
							{
								"id": "gemini-weekly",
								"name": "Weekly Limit Remaining",
								"description": "You have used some weekly limit",
								"window": "weekly",
								"remaining_fraction": 0.820467,
								"reset_time": "2026-11-15T08:28:18Z"
							},
							{
								"id": "gemini-5h",
								"name": "Five Hour Limit Remaining",
								"description": "You have used some 5h limit",
								"window": "5h",
								"remaining_fraction": 0.7745,
								"reset_time": "2026-11-10T04:13:05Z"
							}
						]
					},
					{
						"name": "Claude and GPT models",
						"description": "Models within this group: Claude Sonnet, Opus",
						"buckets": [
							{
								"id": "3p-weekly",
								"name": "Weekly Limit Remaining",
								"window": "weekly",
								"remaining_fraction": 1.0,
								"reset_time": "2026-11-17T03:44:15Z"
							}
						]
					}
				]
			}
		}
	}`)

	formatted, err := FormatAGYUsageOutput(rawJSON)
	if err != nil {
		t.Fatalf("unexpected error formatting usage output: %v", err)
	}

	if !strings.Contains(formatted, "Gemini Models") {
		t.Errorf("expected output to contain 'Gemini Models', got: %s", formatted)
	}
	if !strings.Contains(formatted, "82.0%") {
		t.Errorf("expected output to contain '82.0%%', got: %s", formatted)
	}
	if !strings.Contains(formatted, "77.5%") {
		t.Errorf("expected output to contain '77.5%%', got: %s", formatted)
	}
	if !strings.Contains(formatted, "100.0%") {
		t.Errorf("expected output to contain '100.0%%', got: %s", formatted)
	}
}

func TestFormatAGYUsageOutput_Fallback(t *testing.T) {
	rawJSON := []byte(`{
		"conversation_id": "",
		"status": "SUCCESS",
		"response": "Gemini Models\tWeekly Limit Remaining\t82%\t2026-11-15T08:28:18Z",
		"command": {
			"name": "usage",
			"data": {
				"groups": []
			}
		}
	}`)

	formatted, err := FormatAGYUsageOutput(rawJSON)
	if err != nil {
		t.Fatalf("unexpected error in fallback formatting: %v", err)
	}
	if !strings.Contains(formatted, "Gemini Models") || !strings.Contains(formatted, "82%") {
		t.Errorf("expected fallback text, got: %s", formatted)
	}
}

type mockUsageProvider struct {
	usageText string
}

func (m *mockUsageProvider) Name() string { return "mock-agy" }
func (m *mockUsageProvider) Chat(ctx context.Context, messages []core.ChatMessage, opts *core.ChatOptions) (string, error) {
	return "mock-chat", nil
}
func (m *mockUsageProvider) ChatWithTools(ctx context.Context, messages []core.ChatMessage, tools []core.ToolDefinition, opts *core.ChatOptions) (*core.ChatResponse, error) {
	return &core.ChatResponse{Content: "mock-chat"}, nil
}
func (m *mockUsageProvider) GetUsage(ctx context.Context) (string, error) {
	return m.usageText, nil
}

func TestMindRouter_QueryChannelUsage(t *testing.T) {
	router := NewMindRouter(MindRouterConfig{
		StatePath: t.TempDir() + "/active.json",
	})

	router.RegisterChannel(ChannelRegistration{
		Channel: "AGY",
		Provider: &mockUsageProvider{
			usageText: "### 实时配额\n- 剩余 98.7%",
		},
		Models: []ModelInfo{
			{ID: "gemini-3.8-flash", Name: "Gemini Flash"},
		},
	})

	usage, err := router.QueryChannelUsage(context.Background(), "AGY")
	if err != nil {
		t.Fatalf("QueryChannelUsage failed: %v", err)
	}
	if !strings.Contains(usage, "98.7%") {
		t.Errorf("unexpected usage output: %s", usage)
	}

	_, errMissing := router.QueryChannelUsage(context.Background(), "NONEXISTENT")
	if errMissing == nil {
		t.Fatalf("expected error for nonexistent channel")
	}
}
