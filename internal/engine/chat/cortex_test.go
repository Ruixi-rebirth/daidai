package chat

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/media"
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
	expectedQuote := `"quoted_message":{"content":"package main\nfunc main() {}"}`
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

func TestQuoteCompletenessReflectsPromptTruncation(t *testing.T) {
	for _, budget := range []int{5, core.MaxQuotedMessageRunes * 2} {
		remaining := budget
		quote := quotedMessageContextFromEvent(&entity.SensoryEvent{Content: strings.Repeat("字", core.MaxQuotedMessageRunes+100), ContentComplete: true}, 0, &remaining)
		if quote == nil || quote.ContentComplete {
			t.Fatal("truncated quote was advertised as complete")
		}
	}
	remaining := 100
	quote := quotedMessageContextFromEvent(&entity.SensoryEvent{Content: "完整正文", ContentComplete: true}, 0, &remaining)
	if !quote.ContentComplete {
		t.Fatal("complete quote incorrectly marked partial")
	}
}

type intentTestLLM struct {
	textOnly    bool
	response    string
	intentCalls int
}

func (*intentTestLLM) Name() string { return "intent-test-llm" }
func (m *intentTestLLM) Chat(context.Context, []core.ChatMessage, *core.ChatOptions) (string, error) {
	m.intentCalls++
	if m.response != "" {
		return m.response, nil
	}
	if m.textOnly {
		return `{"text_only": true,"request":"转为文字"}`, nil
	}
	return `{"text_only": false}`, nil
}

func TestTextOnlyPreferenceRequiresExplicitOriginalRequest(t *testing.T) {
	for _, tc := range []struct {
		name, content, response string
		want                    bool
		wantCalls               int
	}{
		{"ordinary Nix question", "nix的优点是什么", `{"text_only":true,"request":"nix的优点是什么"}`, false, 0},
		{"explicit request", "只发文字，方便复制", `{"text_only":true,"request":"只发文字"}`, true, 1},
		{"extract quoted card", "把刚才图里第二条用文字发给我", `{"text_only":true,"request":"第二条用文字发给我"}`, true, 1},
		{"unsupported guess", "介绍图片压缩", `{"text_only":true}`, false, 1},
		{"invented evidence", "介绍图片压缩", `{"text_only":true,"request":"请只发文字"}`, false, 1},
		{"talking about pictures", "介绍图片压缩", `{"text_only":false,"request":""}`, false, 1},
		{"image preference", "别发文字，请用图片", `{"text_only":false,"request":""}`, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &intentTestLLM{response: tc.response}
			c := NewCortex(CortexConfig{LLM: model, Persona: entity.NewDefaultPersona()})
			if got := c.detectTextOnlyIntent(context.Background(), tc.content); got != tc.want || model.intentCalls != tc.wantCalls {
				t.Fatalf("got=%v calls=%d", got, model.intentCalls)
			}
		})
	}
}

func TestTextPreferenceDoesNotPersistIntoNextReply(t *testing.T) {
	model := &intentTestLLM{response: `{"text_only":true,"request":"第二条用文字发给我"}`}
	c := NewCortex(CortexConfig{LLM: model, Persona: entity.NewDefaultPersona()})
	for _, tc := range []struct {
		content  string
		textOnly bool
	}{
		{"把图里第二条用文字发给我", true},
		{"nix的优点是什么", false},
	} {
		out, err := c.Think(context.Background(), &entity.SensoryEvent{
			Platform: "qq", ChannelID: "group", SenderID: "member", Content: tc.content,
			ReplyContext: &entity.SensoryEvent{Content: "第一条介绍\n第二条：可复制的正文", ContentKind: "text_card", ContentComplete: true},
		})
		if err != nil || out == nil || out.FixedPresentation != tc.textOnly {
			t.Fatalf("content=%s out=%+v err=%v", tc.content, out, err)
		}
	}
}

func TestTextPreferenceUsesCardSourceAndKeepsRealImages(t *testing.T) {
	for _, realImage := range []bool{false, true} {
		source := "# 可复制的卡片正文"
		if realImage {
			source = ""
		}
		out := &entity.Utterance{Images: [][]byte{[]byte("image")}, ImageSources: []string{source}, Text: "补充链接", InlineImages: true}
		useCardSourceText(out)
		if realImage {
			if len(out.Images) != 1 || out.Text != "补充链接" {
				t.Fatal("real image was replaced by text")
			}
		} else if out.HasMedia() || out.InlineImages || !strings.Contains(out.Text, source) || !strings.Contains(out.Text, "补充链接") {
			t.Fatalf("card source was not preserved: %+v", out)
		}
	}
}
func (*intentTestLLM) ChatWithTools(context.Context, []core.ChatMessage, []core.ToolDefinition, *core.ChatOptions) (*core.ChatResponse, error) {
	return &core.ChatResponse{Content: "这里是生成的回复内容"}, nil
}

func TestCortex_TextOnlyIntentSetsFixedPresentation(t *testing.T) {
	for _, textOnly := range []bool{true, false} {
		t.Run(map[bool]string{true: "text_requested", false: "normal_chat"}[textOnly], func(t *testing.T) {
			c := NewCortex(CortexConfig{
				LLM:     &intentTestLLM{textOnly: textOnly},
				Persona: entity.NewDefaultPersona(),
			})
			event := &entity.SensoryEvent{
				Platform:  "qq",
				ChannelID: "test-channel",
				SenderID:  "test-sender",
				Content:   "版本2转为文字",
			}
			out, err := c.Think(context.Background(), event)
			if err != nil {
				t.Fatalf("Think failed: %v", err)
			}
			if out == nil {
				t.Fatal("expected utterance")
			}
			if out.FixedPresentation != textOnly {
				t.Fatalf("expected FixedPresentation=%v, got %v", textOnly, out.FixedPresentation)
			}
		})
	}
}

func TestCortex_RenderFileContext(t *testing.T) {
	inbox, err := media.NewInbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sources := &media.Sources{}
	sources.Register("test-proto", func(_ context.Context, u string, limit int64) ([]byte, error) {
		if strings.Contains(u, "largetextfile") {
			buf := bytes.Repeat([]byte("log line 1234567890\n"), 20000)
			if int64(len(buf)) > limit {
				buf = buf[:limit]
			}
			return buf, nil
		}
		if strings.Contains(u, "textfile") {
			return []byte("Hello, this is a plain text file without standard extension!"), nil
		}
		if strings.Contains(u, "binaryfile") {
			return []byte{0x7f, 'E', 'L', 'F', 0x00, 0x01, 0x02}, nil
		}
		return nil, fmt.Errorf("file not found")
	})
	inbox.Sources = sources

	c := NewCortex(CortexConfig{
		Inbox: inbox,
	})

	t.Run("inlines text file and records processed attachment", func(t *testing.T) {
		event := &entity.SensoryEvent{Platform: "qq"}
		textAttachment := &core.Attachment{
			Kind:     core.AttachmentFile,
			URL:      "test-proto:textfile",
			Name:     "Dockerfile",
			MimeType: "application/octet-stream",
		}
		result := c.renderFileContext(context.Background(), event, []*core.Attachment{textAttachment}, false, 0)
		if !strings.Contains(result, "【文件附件内容】") {
			t.Fatalf("expected inlined file header, got: %s", result)
		}
		if !strings.Contains(result, "Hello, this is a plain text file without standard extension!") {
			t.Fatalf("expected text content inlined, got: %s", result)
		}
		if len(event.ProcessedAttachments) != 1 || event.ProcessedAttachments[0] != textAttachment {
			t.Fatalf("expected attachment in ProcessedAttachments, got: %v", event.ProcessedAttachments)
		}
	})

	t.Run("marks binary file without inlining data", func(t *testing.T) {
		event := &entity.SensoryEvent{Platform: "qq"}
		binAttachment := &core.Attachment{
			Kind:     core.AttachmentFile,
			URL:      "test-proto:binaryfile",
			Name:     "app.bin",
			MimeType: "application/octet-stream",
		}
		result := c.renderFileContext(context.Background(), event, []*core.Attachment{binAttachment}, false, 0)
		if strings.Contains(result, "【文件附件内容】") {
			t.Fatalf("binary file should not be rendered as text, got: %s", result)
		}
		if !strings.Contains(result, "二进制数据，无法直接作为纯文本阅读") {
			t.Fatalf("expected binary notice, got: %s", result)
		}
		if len(event.ProcessedAttachments) != 0 {
			t.Fatalf("binary file should not be in ProcessedAttachments, got: %v", event.ProcessedAttachments)
		}
	})

	t.Run("large text file is truncated without allocating huge buffer", func(t *testing.T) {
		event := &entity.SensoryEvent{Platform: "qq"}
		largeAttachment := &core.Attachment{
			Kind:     core.AttachmentFile,
			URL:      "test-proto:largetextfile",
			Name:     "access.log",
			MimeType: "text/plain",
		}
		result := c.renderFileContext(context.Background(), event, []*core.Attachment{largeAttachment}, false, 0)
		if !strings.Contains(result, "文件较大") || !strings.Contains(result, "已流式截取") {
			t.Fatalf("expected large file truncation notice, got: %s", result)
		}
		if len(event.ProcessedAttachments) != 1 || event.ProcessedAttachments[0] != largeAttachment {
			t.Fatalf("expected large attachment in ProcessedAttachments, got: %v", event.ProcessedAttachments)
		}
	})
}
