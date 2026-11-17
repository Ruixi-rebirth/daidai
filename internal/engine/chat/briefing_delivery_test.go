package chat

import (
	"context"
	"daidai/internal/core"
	"daidai/internal/entity"
	"encoding/json"
	"errors"
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
		OnDelivered: func(context.Context) { commits++ },
	}}
	c := NewCortex(CortexConfig{LLM: model, Hands: hands, Persona: entity.NewDefaultPersona()})
	out, err := c.Think(context.Background(), &entity.SensoryEvent{
		Platform: "qq", ChannelID: "group", IsGroup: true, SenderID: "member", Content: "呆呆你还记得我吗",
	})
	if err != nil {
		t.Fatal(err)
	}
	if model.round != 1 || len(out.Images) != 1 || out.Text != "" || out.FallbackText != "# 记忆画像" {
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
	}
}
func (h *preparedViewHands) Execute(context.Context, string, json.RawMessage) (*core.ToolResult, error) {
	return h.result, nil
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
