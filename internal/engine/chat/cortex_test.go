package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/provider/llm"
	"daidai/internal/tool"
)

type mockLLMWithRenderTool struct {
	round              int
	calledWithMessages []core.ChatMessage
}

func (m *mockLLMWithRenderTool) Name() string { return "mock_llm" }

func (m *mockLLMWithRenderTool) Chat(ctx context.Context, messages []core.ChatMessage, opts *core.ChatOptions) (string, error) {
	return "mock reply", nil
}

func (m *mockLLMWithRenderTool) ChatWithTools(ctx context.Context, messages []core.ChatMessage, tools []core.ToolDefinition, opts *core.ChatOptions) (*core.ChatResponse, error) {
	m.calledWithMessages = messages
	m.round++
	if m.round == 1 {
		// 模拟大模型看到引用后第一轮调用 render_card
		return &core.ChatResponse{
			Content: "",
			ToolCalls: []core.ToolCall{
				{
					ID:   "call_render_1",
					Type: "function",
					Function: core.ToolCallFunction{
						Name:      "render_card",
						Arguments: "{\"content\": \"# 引用代码\\n\\n```go\\nfunc hello() {}\\n```\"}",
					},
				},
			},
		}, nil
	}
	// 卡片生成后模型第二轮给出配文
	return &core.ChatResponse{
		Content: "这是你要的代码卡片～",
	}, nil
}

func TestCortex_ThinkWithQuoteMarkdown(t *testing.T) {
	mockLLM := &mockLLMWithRenderTool{}
	persona := entity.NewDefaultPersona()
	hands := tool.NewHandsWithConfig(tool.HandsConfig{EnableNixSearch: true})

	cortex := NewCortex(CortexConfig{
		LLM:     mockLLM,
		Persona: persona,
		Hands:   hands,
	})

	event := &entity.SensoryEvent{
		Platform:   "qq",
		ChannelID:  "10001",
		SenderID:   "10001",
		SenderName: "死党",
		Role:       entity.RolePartner,
		Content:    "转为图片",
		ReplyContext: &entity.SensoryEvent{
			Content: "package main\nfunc main() {}",
		},
		RawMessageID: "msg_quote_001",
		OccurredAt:   time.Now(),
	}

	utter, err := cortex.Think(context.Background(), event)
	if err != nil {
		t.Fatalf("cortex think failed: %v", err)
	}

	// 验证发给 LLM 的最新一条 User 消息是否包含标准的 Markdown 引用块
	if len(mockLLM.calledWithMessages) == 0 {
		t.Fatalf("expected llm to be called")
	}
	var lastUserMsg *core.ChatMessage
	for i := len(mockLLM.calledWithMessages) - 1; i >= 0; i-- {
		if mockLLM.calledWithMessages[i].Role == core.RoleUser {
			lastUserMsg = &mockLLM.calledWithMessages[i]
			break
		}
	}
	if lastUserMsg == nil {
		t.Fatalf("expected user message in calledWithMessages")
	}
	expectedQuote := "> package main\n> func main() {}"
	if !strings.Contains(lastUserMsg.Content, expectedQuote) {
		t.Fatalf("expected user message to contain quote block %q, got: %s", expectedQuote, lastUserMsg.Content)
	}

	// 验证最终产出物携带了卡片二进制图且保留了 Platform
	if utter == nil {
		t.Fatalf("expected utterance")
	}
	if utter.Platform != "qq" {
		t.Errorf("expected utterance platform 'qq', got %q", utter.Platform)
	}
	if len(utter.CardData) == 0 && len(utter.Images) == 0 {
		t.Fatalf("expected utterance to contain rendered card data")
	}
}

type mockLLMWithSwitchAndTicket struct {
	round    int
	toolName string
	toolArgs string
	lastMsgs []core.ChatMessage
}

func (m *mockLLMWithSwitchAndTicket) Name() string { return "mock" }
func (m *mockLLMWithSwitchAndTicket) Chat(ctx context.Context, messages []core.ChatMessage, opts *core.ChatOptions) (string, error) {
	return "ok", nil
}
func (m *mockLLMWithSwitchAndTicket) ChatWithTools(ctx context.Context, messages []core.ChatMessage, tools []core.ToolDefinition, opts *core.ChatOptions) (*core.ChatResponse, error) {
	m.round++
	m.lastMsgs = messages
	if m.round == 1 {
		return &core.ChatResponse{
			ToolCalls: []core.ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: core.ToolCallFunction{
						Name:      m.toolName,
						Arguments: m.toolArgs,
					},
				},
			},
		}, nil
	}
	return &core.ChatResponse{Content: "已完成处理"}, nil
}

type dummyWorker struct{}

func (d *dummyWorker) Name() string        { return "agy" }
func (d *dummyWorker) Description() string { return "test worker" }
func (d *dummyWorker) ExecuteTicket(ctx context.Context, t *entity.Ticket) (string, error) {
	return "worker finished: " + t.Goal, nil
}

func TestCortex_GuestBlockedFromSwitchModelAndTicketProgress(t *testing.T) {
	immune := entity.NewImmuneSystem("10001")
	delegates := entity.NewDelegates(&dummyWorker{})

	router := llm.NewMindRouter(llm.MindRouterConfig{
		StatePath:       t.TempDir() + "/mind.json",
		FallbackChannel: "API",
	})
	hands := tool.NewHandsWithConfig(tool.HandsConfig{
		Router:    router,
		Delegates: delegates,
		Immune:    immune,
	})

	// 1. 验证普通群友 (RoleGuest) 试图调用 switch_model 切换模型时被工具内置鉴权拦截
	mockSwitch := &mockLLMWithSwitchAndTicket{
		toolName: "switch_model",
		toolArgs: `{"action":"switch","model":"some-model"}`,
	}
	cortex := NewCortex(CortexConfig{
		LLM:   mockSwitch,
		Hands: hands,
	})
	guestEvent := &entity.SensoryEvent{
		Platform:  "qq",
		ChannelID: "9001",
		SenderID:  "99999",
		Role:      entity.RoleGuest,
		Content:   "把模型切到最大",
	}
	_, err := cortex.Think(context.Background(), guestEvent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	foundPermDenied := false
	for _, msg := range mockSwitch.lastMsgs {
		if msg.Role == core.RoleTool && (strings.Contains(msg.Content, "权限不足") || strings.Contains(msg.Content, "调用已拒绝")) {
			foundPermDenied = true
		}
	}
	if !foundPermDenied {
		t.Fatalf("expected RoleGuest switch_model mutation to be blocked by ImmuneSystem")
	}

	// 2. 验证核心搭档 (RolePartner) 派发毫秒级快工单时不触发冗余进度刷屏
	mockTicket := &mockLLMWithSwitchAndTicket{
		toolName: "delegate_ticket",
		toolArgs: `{"goal":"重构测试模块"}`,
	}
	cortex2 := NewCortex(CortexConfig{
		LLM:   mockTicket,
		Hands: hands,
	})

	var progressCalled string
	partnerEvent := &entity.SensoryEvent{
		Platform:  "qq",
		ChannelID: "10001",
		SenderID:  "10001",
		Role:      entity.RolePartner,
		Content:   "帮我重构测试模块",
		OnProgress: func(text string) {
			progressCalled = text
		},
	}
	_, err = cortex2.Think(context.Background(), partnerEvent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if progressCalled != "" {
		t.Fatalf("expected fast ticket (<3s) not to spam OnProgress, got %q", progressCalled)
	}
}

type longReplyLLM struct {
	mockLLMWithRenderTool
	text string
}

func (m *longReplyLLM) ChatWithTools(context.Context, []core.ChatMessage, []core.ToolDefinition, *core.ChatOptions) (*core.ChatResponse, error) {
	return &core.ChatResponse{Content: m.text}, nil
}
func TestLongReplyRetainsTail(t *testing.T) {
	original := strings.Repeat("长回复正文", 1400) + "\n唯一尾部标记"
	c := NewCortex(CortexConfig{LLM: &longReplyLLM{text: original}, Persona: entity.NewDefaultPersona()})
	result, err := c.Think(context.Background(), &entity.SensoryEvent{Platform: "qq", ChannelID: "owner", SenderID: "owner", Role: entity.RolePartner, Content: "解释代码"})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Text != original {
		t.Fatal("full model response was not preserved")
	}
}
