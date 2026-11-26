package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"daidai/internal/core"
	"daidai/internal/entity"
)

type sequentialWebModel struct {
	core.LLMProvider
	round int
}

func (*sequentialWebModel) Name() string { return "sequential-web" }
func (m *sequentialWebModel) ChatWithTools(_ context.Context, messages []core.ChatMessage, _ []core.ToolDefinition, _ *core.ChatOptions) (*core.ChatResponse, error) {
	m.round++
	name := "screenshot_web"
	if m.round == 2 {
		name = "read_link"
	}
	if m.round <= 2 {
		return &core.ChatResponse{ToolCalls: []core.ToolCall{{ID: fmt.Sprint(m.round), Type: "function", Function: core.ToolCallFunction{Name: name, Arguments: `{"url":"https://example.com/article"}`}}}}, nil
	}
	for _, message := range messages {
		if message.Role == core.RoleTool && message.Name == "read_link" && strings.Contains(message.Content, "正文依据") {
			return &core.ChatResponse{Content: "网页总结和问题答案均基于已读取的正文依据。"}, nil
		}
	}
	return nil, fmt.Errorf("page body was never read")
}

type sequentialWebHands struct {
	entity.Hands
	mu    sync.Mutex
	calls []string
}

func (*sequentialWebHands) Definitions() []core.ToolDefinition {
	return []core.ToolDefinition{
		{Type: "function", Function: core.ToolFunctionDetail{Name: "screenshot_web"}},
		{Type: "function", Function: core.ToolFunctionDetail{Name: "read_link"}},
	}
}
func (h *sequentialWebHands) Execute(_ context.Context, name string, _ json.RawMessage) (*core.ToolResult, error) {
	h.mu.Lock()
	h.calls = append(h.calls, name)
	h.mu.Unlock()
	if name == "screenshot_web" {
		return &core.ToolResult{Text: "截图已生成", ImageData: []byte("fixture image")}, nil
	}
	return &core.ToolResult{Text: "正文依据"}, nil
}

func TestScreenshotDoesNotPreventLaterPageReadingAndAnswer(t *testing.T) {
	hands := &sequentialWebHands{}
	cortex := NewCortex(CortexConfig{LLM: &sequentialWebModel{}, Hands: hands})
	utterance, err := cortex.Think(context.Background(), &entity.SensoryEvent{
		Platform: "telegram", ChannelID: "chat", SenderID: "owner", Role: entity.RolePartner,
		Content: "截取这个网页，再总结正文并回答我的问题：https://example.com/article",
	})
	if err != nil || utterance == nil {
		t.Fatalf("workflow failed: utterance=%v error=%v", utterance, err)
	}
	if len(hands.calls) != 2 || hands.calls[1] != "read_link" || len(utterance.Images) != 1 || !strings.Contains(utterance.Text, "正文依据") {
		t.Fatalf("image interrupted remaining work: calls=%v images=%d text=%q", hands.calls, len(utterance.Images), utterance.Text)
	}
}

func TestWebToolsCanReadMoreThanTwoPagesInOneBatch(t *testing.T) {
	hands := &sequentialWebHands{}
	cortex := NewCortex(CortexConfig{Hands: hands})
	ctx := entity.WithSensoryEvent(context.Background(), &entity.SensoryEvent{Role: entity.RolePartner})
	var calls []core.ToolCall
	for i, path := range []string{"a", "b", "c"} {
		calls = append(calls, core.ToolCall{ID: fmt.Sprint(i), Function: core.ToolCallFunction{
			Name: "read_link", Arguments: fmt.Sprintf(`{"url":"https://example.com/%s"}`, path),
		}})
	}
	_, err := cortex.executeToolBatch(ctx, hands.Definitions(), calls, 0, nil, &toolMediaDelivery{}, &thinkPresentation{})
	if err != nil || len(hands.calls) != len(calls) {
		t.Fatalf("expected all %d calls to run, got %v with error %v", len(calls), hands.calls, err)
	}
}
