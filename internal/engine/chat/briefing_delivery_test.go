package chat

import (
	"context"
	"daidai/internal/core"
	"daidai/internal/entity"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type preparedViewModel struct {
	core.LLMProvider
	round    int
	toolName string
}

func (*preparedViewModel) Name() string { return "prepared-view" }
func (*preparedViewModel) Chat(context.Context, []core.ChatMessage, *core.ChatOptions) (string, error) {
	return `{"text_only": false}`, nil
}
func (m *preparedViewModel) ChatWithTools(context.Context, []core.ChatMessage, []core.ToolDefinition, *core.ChatOptions) (*core.ChatResponse, error) {
	m.round++
	if m.round == 1 {
		name := m.toolName
		if name == "" {
			name = "get_geek_briefing"
		}
		return &core.ChatResponse{ToolCalls: []core.ToolCall{{ID: "get1", Type: "function", Function: core.ToolCallFunction{Name: name, Arguments: `{}`}}}}, nil
	}
	return &core.ChatResponse{}, nil
}

func TestPreparedMemoryCardDoesNotAskModelToRenderAgain(t *testing.T) {
	model := &preparedViewModel{toolName: "manage_memory"}
	commits := 0
	hands := &preparedViewHands{result: &core.ToolResult{
		Text: "# 记忆画像", FallbackText: "# 记忆画像", ImageData: []byte("memory-png"), ImageOnly: true,
		Images: [][]byte{nil}, ImageSources: []string{"不应保留的原文", "# 记忆画像"},
		OnDelivered: func(context.Context) { commits++ },
	}}
	c := NewCortex(CortexConfig{LLM: model, Hands: hands, Persona: entity.NewDefaultPersona()})
	out, err := c.Think(context.Background(), &entity.SensoryEvent{
		Platform: "qq", ChannelID: "group", IsGroup: true, SenderID: "member", Content: "呆呆你还记得我吗",
	})
	if err != nil {
		t.Fatal(err)
	}
	if model.round != 1 || len(out.Images) != 1 || len(out.ImageSources) != 1 || out.ImageSources[0] != "# 记忆画像" || out.Text != "" || out.FallbackText != "# 记忆画像" {
		t.Fatalf("rounds=%d out=%+v", model.round, out)
	}
	if commits != 0 {
		t.Fatal("view committed before delivery")
	}
	out.OnDelivered(context.Background())
	if commits != 1 {
		t.Fatal("view delivery callback lost")
	}
}

type preparedViewHands struct {
	entity.Hands
	result *core.ToolResult
}

func (*preparedViewHands) Definitions() []core.ToolDefinition {
	return []core.ToolDefinition{
		{Type: "function", Function: core.ToolFunctionDetail{Name: "get_geek_briefing"}},
		{Type: "function", Function: core.ToolFunctionDetail{Name: "manage_memory"}},
		{Type: "function", Function: core.ToolFunctionDetail{Name: "delegate_ticket"}},
	}
}
func (h *preparedViewHands) Execute(context.Context, string, json.RawMessage) (*core.ToolResult, error) {
	return h.result, nil
}

func TestTicketAcknowledgementDoesNotDependOnWording(t *testing.T) {
	model := &preparedViewModel{toolName: "delegate_ticket"}
	const ack = "收到，工单编号 T001。"
	hands := &preparedViewHands{result: &core.ToolResult{Text: ack, DirectReply: true}}
	cortex := NewCortex(CortexConfig{LLM: model, Hands: hands})
	out, err := cortex.Think(context.Background(), &entity.SensoryEvent{
		Platform: "qq", ChannelID: "1", SenderID: "1", Role: entity.RolePartner, Content: "整理项目文档",
	})
	if err != nil || out == nil || out.Text != ack || model.round != 1 {
		t.Fatalf("out=%+v err=%v model calls=%d", out, err, model.round)
	}
}

type preparedViewChannel struct {
	entity.Channel
	fail bool
}

func (*preparedViewChannel) Name() string { return "telegram" }
func (c *preparedViewChannel) Transmit(context.Context, *entity.Utterance) error {
	if c.fail {
		return errors.New("failed")
	}
	return nil
}
func TestPreparedViewsKeepCaptionSourcesAndCommitAfterSend(t *testing.T) {
	for _, pictures := range []bool{false, true} {
		t.Run(map[bool]string{true: "images", false: "text"}[pictures], func(t *testing.T) {
			commits := 0
			result := &core.ToolResult{Text: "view ready", Caption: "原文索引", InlineImages: pictures, OnDelivered: func(context.Context) { commits++ }}
			if pictures {
				result.Images = [][]byte{[]byte("first"), []byte("second")}
				result.ImageSources = []string{"第一页原文", "第二页原文"}
			}
			cortex := NewCortex(CortexConfig{LLM: &preparedViewModel{}, Hands: &preparedViewHands{result: result}})
			utter, err := cortex.Think(context.Background(), &entity.SensoryEvent{Platform: "telegram", ChannelID: "1", Role: entity.RolePartner, Content: "今天的早报"})
			if err != nil {
				t.Fatal(err)
			}
			if utter.Text != "原文索引" || !utter.FixedPresentation {
				t.Fatalf("prepared output lost: %+v", utter)
			}
			if pictures && (len(utter.ImageSources) != 2 || utter.ImageSources[1] != "第二页原文") {
				t.Fatal("page sources lost")
			}
			if commits != 0 {
				t.Fatal("committed before delivery")
			}
			registry := entity.NewChannelRegistry()
			channel := &preparedViewChannel{fail: true}
			registry.Register(channel)
			if registry.Transmit(context.Background(), utter) == nil || commits != 0 {
				t.Fatal("failed send committed")
			}
			channel.fail = false
			if err = registry.Transmit(context.Background(), utter); err != nil || commits != 1 {
				t.Fatal("successful send did not commit")
			}
		})
	}
}

type mixedViewModel struct{ preparedViewModel }

func (m *mixedViewModel) ChatWithTools(ctx context.Context, messages []core.ChatMessage, tools []core.ToolDefinition, opts *core.ChatOptions) (*core.ChatResponse, error) {
	if m.round == 0 {
		response, err := m.preparedViewModel.ChatWithTools(ctx, messages, tools, opts)
		response.ToolCalls = append(response.ToolCalls, core.ToolCall{
			ID: "other", Function: core.ToolCallFunction{Name: "delegate_ticket", Arguments: "{}"},
		})
		return response, err
	}
	m.round++
	for _, message := range messages {
		if message.Role == core.RoleTool && message.Name == "manage_memory" && strings.Contains(message.Content, "已排版的画像正文") {
			return nil, errors.New("prepared view body reached the model")
		}
	}
	return &core.ChatResponse{Content: "另一项任务已排队。"}, nil
}

type mixedViewHands struct{ preparedViewHands }

func (h *mixedViewHands) Execute(ctx context.Context, name string, args json.RawMessage) (*core.ToolResult, error) {
	if name == "delegate_ticket" {
		return &core.ToolResult{Text: "工单已排队", DirectReply: true}, nil
	}
	return h.preparedViewHands.Execute(ctx, name, args)
}

func TestPreparedViewDoesNotSuppressOtherToolResults(t *testing.T) {
	model := &mixedViewModel{preparedViewModel{toolName: "manage_memory"}}
	hands := &mixedViewHands{preparedViewHands{result: &core.ToolResult{Text: "已排版的画像正文", ImageData: []byte("card"), ImageOnly: true}}}
	cortex := NewCortex(CortexConfig{LLM: model, Hands: hands})
	event := &entity.SensoryEvent{Platform: "qq", ChannelID: "chat", SenderID: "owner", Content: "查看印象并整理文档", Role: entity.RolePartner}
	out, err := cortex.Think(context.Background(), event)
	if err != nil || out == nil || len(out.Images) != 1 || out.Text != "另一项任务已排队。" || model.round != 2 {
		t.Fatalf("mixed tool batch lost a result: out=%+v, err=%v, rounds=%d", out, err, model.round)
	}
}

func TestPreparedViewRecoveryKeepsLinkIndexAfterLongBody(t *testing.T) {
	body := strings.Repeat("长", 9000)
	index := "[原文](https://example.com/article)"
	hands := &preparedViewHands{result: &core.ToolResult{
		ImageData: []byte("card"), ImageOnly: true, FallbackText: body, Caption: index,
	}}
	cortex := NewCortex(CortexConfig{Hands: hands})
	call := core.ToolCall{ID: "view", Function: core.ToolCallFunction{Name: "get_geek_briefing", Arguments: "{}"}}
	batch, err := cortex.executeToolBatch(context.Background(), hands.Definitions(), []core.ToolCall{call}, 0, nil, nil, &toolMediaDelivery{}, &thinkPresentation{})
	if err != nil || len(batch.messages) != 1 || !strings.HasSuffix(batch.messages[0].RecoveryContent, index) || strings.Contains(batch.messages[0].Content, index) {
		t.Fatalf("prepared view index was lost or exposed in the active model prompt: %v", err)
	}
}
