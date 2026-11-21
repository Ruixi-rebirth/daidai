package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
)

type truncatedMediaModel struct {
	preparedViewModel
	answerErr       error
	waitForDeadline bool
	emptyTruncation bool
}

func TestTruncatedContinuationPersistsOnlyReplySegments(t *testing.T) {
	store := db.NewManager(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	model := &truncatedMediaModel{preparedViewModel: preparedViewModel{round: 1}}
	cortex := NewCortex(CortexConfig{LLM: model, Drafts: store})
	ctx := context.Background()
	event := &entity.SensoryEvent{Platform: "qq", ChannelID: "chat", SenderID: "owner", Content: "原始任务", Role: entity.RolePartner}
	_, err := cortex.saveDraft(ctx, event, []core.ChatMessage{
		{Role: core.RoleUser, Content: event.Content},
		{Role: core.RoleAssistant, Content: "第一段"},
		{Role: core.RoleUser, Content: resumeDraftInstruction},
		{Role: core.RoleAssistant, Content: "旧草稿第二段"},
	})
	if err != nil {
		t.Fatal(err)
	}
	model.emptyTruncation = true
	out, notice, err := cortex.ContinueDraft(ctx, event, "")
	if err != nil || out != nil || !strings.Contains(notice, "达到输出上限") {
		t.Fatalf("empty truncation was not reported as retryable: %+v, %q, %v", out, notice, err)
	}
	model.emptyTruncation = false
	for range 2 {
		out, notice, err := cortex.ContinueDraft(ctx, event, "")
		if err != nil || out == nil || out.DraftID == "" || notice != "" || !strings.Contains(out.Text, "\n~~~\n\n[本段已保存") {
			t.Fatalf("resume failed: %+v, %q, %v", out, notice, err)
		}
	}
	summaries, err := store.ListConversationDraftSummaries(ctx, "qq", "private", "chat", "owner")
	if err != nil || len(summaries) != 1 {
		t.Fatalf("unexpected summaries: %+v, %v", summaries, err)
	}
	draft, err := store.GetConversationDraft(ctx, "qq", "private", "chat", "owner", summaries[0].ID)
	if err != nil || draft == nil {
		t.Fatalf("draft missing: %v", err)
	}
	var saved savedDraftContext
	if err := json.Unmarshal([]byte(draft.ContextJSON), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Messages) != 5 || strings.Contains(draft.ContextJSON, resumeDraftInstruction) || saved.Messages[4].Content != "~~~go\nfunc unfinished() {" {
		t.Fatalf("unexpected persisted messages: %+v", saved.Messages)
	}
}

func (m *truncatedMediaModel) ChatWithTools(ctx context.Context, messages []core.ChatMessage, tools []core.ToolDefinition, opts *core.ChatOptions) (*core.ChatResponse, error) {
	if m.round == 0 {
		return m.preparedViewModel.ChatWithTools(ctx, messages, tools, opts)
	}
	if m.answerErr != nil {
		return nil, m.answerErr
	}
	if m.waitForDeadline {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if m.emptyTruncation {
		return &core.ChatResponse{FinishReason: "length"}, nil
	}
	return &core.ChatResponse{Content: "~~~go\nfunc unfinished() {", FinishReason: "length"}, nil
}

func TestTurnDeadlinePreservesCompletedToolRound(t *testing.T) {
	store := db.NewManager(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	model := &truncatedMediaModel{
		preparedViewModel: preparedViewModel{toolName: "manage_memory"},
		waitForDeadline:   true,
	}
	hands := &preparedViewHands{result: &core.ToolResult{Text: "工具结果"}}
	cortex := NewCortex(CortexConfig{LLM: model, Hands: hands, Drafts: store})
	event := &entity.SensoryEvent{Platform: "qq", ChannelID: "chat", SenderID: "owner", Content: "查找记录", Role: entity.RolePartner}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	var checkpoint *entity.ReplyDeadlineWithDraft
	if _, err := cortex.Think(ctx, event); !errors.As(err, &checkpoint) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline with saved checkpoint, got %v", err)
	}
	drafts, err := store.ListConversationDraftSummaries(context.Background(), "qq", "private", "chat", "owner")
	if err != nil || len(drafts) != 1 {
		t.Fatalf("expected saved draft: %+v, %v", drafts, err)
	}
	if checkpoint.DraftID != drafts[0].ID {
		t.Fatalf("timeout points to the wrong draft: %q", checkpoint.DraftID)
	}
}

func TestFailedReplySavesToolResultsForContinuation(t *testing.T) {
	store := db.NewManager(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	model := &truncatedMediaModel{
		preparedViewModel: preparedViewModel{toolName: "manage_memory"},
		answerErr:         errors.New("provider timed out"),
	}
	hands := &preparedViewHands{result: &core.ToolResult{Text: "本轮取得的工具证据"}}
	cortex := NewCortex(CortexConfig{LLM: model, Hands: hands, Drafts: store})
	ctx := context.Background()
	event := &entity.SensoryEvent{Platform: "qq", ChannelID: "chat", SenderID: "owner", Content: "帮我找找记录", Role: entity.RolePartner}
	out, err := cortex.Think(ctx, event)
	if err != nil || out == nil || !strings.Contains(out.Text, "/continue") {
		t.Fatalf("expected recoverable reply: %+v, %v", out, err)
	}
	summaries, err := store.ListConversationDraftSummaries(ctx, "qq", "private", "chat", "owner")
	if err != nil || len(summaries) != 1 {
		t.Fatalf("expected one draft: %+v, %v", summaries, err)
	}
	draft, err := store.GetConversationDraft(ctx, "qq", "private", "chat", "owner", summaries[0].ID)
	if err != nil || draft == nil || !strings.Contains(draft.ContextJSON, "本轮取得的工具证据") {
		t.Fatalf("tool evidence was not saved: %+v, %v", draft, err)
	}
}

func TestTruncatedReplyKeepsPreparedMedia(t *testing.T) {
	store := db.NewManager(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	model := &truncatedMediaModel{preparedViewModel: preparedViewModel{toolName: "manage_memory"}}
	hands := &preparedViewHands{result: &core.ToolResult{ImageData: []byte("image"), Text: "图片已经生成"}}
	cortex := NewCortex(CortexConfig{LLM: model, Hands: hands, Drafts: store})
	ctx := context.Background()
	event := &entity.SensoryEvent{Platform: "qq", ChannelID: "chat", SenderID: "owner", Content: "请展示结果并说明", Role: entity.RolePartner}

	out, err := cortex.Think(ctx, event)
	if err != nil || out == nil || len(out.Images) != 1 || !strings.Contains(out.Text, "\n~~~\n\n[内容未完成，已保存进度") {
		t.Fatalf("unexpected truncated reply: %+v, %v", out, err)
	}
	drafts, err := store.ListConversationDraftSummaries(ctx, "qq", "private", "chat", "owner")
	if err != nil || len(drafts) != 1 {
		t.Fatalf("expected saved draft, got %+v, %v", drafts, err)
	}
	if out.DraftID != drafts[0].ID || !strings.Contains(out.Text, "/continue "+out.DraftID) {
		t.Fatalf("truncated reply did not identify the saved draft: %+v", out)
	}
}

type interruptedBatchModel struct{ preparedViewModel }

func (*interruptedBatchModel) ChatWithTools(context.Context, []core.ChatMessage, []core.ToolDefinition, *core.ChatOptions) (*core.ChatResponse, error) {
	return &core.ChatResponse{ToolCalls: []core.ToolCall{
		{ID: "oversized", Function: core.ToolCallFunction{Name: "manage_memory", Arguments: strings.Repeat("x", 2<<20)}},
		{ID: "invalid", Function: core.ToolCallFunction{Name: "manage_memory", Arguments: "null"}},
		{ID: "done", Function: core.ToolCallFunction{Name: "manage_memory", Arguments: "{}"}},
		{ID: "interrupted", Function: core.ToolCallFunction{Name: "delegate_ticket", Arguments: "{}"}},
	}}, nil
}

type interruptedBatchHands struct{ preparedViewHands }

func (*interruptedBatchHands) Execute(ctx context.Context, name string, _ json.RawMessage) (*core.ToolResult, error) {
	if name == "manage_memory" {
		return &core.ToolResult{Text: "首个工具的结果"}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestDeadlineWithinToolBatchSavesCompletedResults(t *testing.T) {
	store := db.NewManager(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	cortex := NewCortex(CortexConfig{LLM: &interruptedBatchModel{}, Hands: &interruptedBatchHands{}, Drafts: store})
	event := &entity.SensoryEvent{Platform: "qq", ChannelID: "chat", SenderID: "owner", Content: "查找记录", Role: entity.RolePartner}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := cortex.Think(ctx, event); !errors.Is(err, entity.ErrReplyDeadlineWithDraft) {
		t.Fatalf("expected saved draft after partial batch, got %v", err)
	}
	summaries, err := store.ListConversationDraftSummaries(context.Background(), "qq", "private", "chat", "owner")
	if err != nil || len(summaries) != 1 {
		t.Fatalf("draft missing: %v", err)
	}
	draft, err := store.GetConversationDraft(context.Background(), "qq", "private", "chat", "owner", summaries[0].ID)
	if err != nil || draft == nil {
		t.Fatalf("draft missing: %v", err)
	}
	var saved savedDraftContext
	if err := json.Unmarshal([]byte(draft.ContextJSON), &saved); err != nil {
		t.Fatal(err)
	}
	last := saved.Messages[len(saved.Messages)-2:]
	if last[0].ToolCallID != "done" || last[0].Content != "首个工具的结果" ||
		last[1].ToolCallID != "interrupted" || !strings.Contains(last[1].Content, "中断") {
		t.Fatalf("incorrect tool feedback: %+v", last)
	}
	batch := saved.Messages[len(saved.Messages)-5:]
	if batch[0].ToolCalls[0].Function.Arguments != "{}" || batch[1].ToolCallID != "oversized" {
		t.Fatal("rejected arguments were retained or detached from their feedback")
	}
	if batch[0].ToolCalls[1].Function.Arguments != "{}" || batch[2].ToolCallID != "invalid" || !strings.Contains(batch[2].Content, "JSON 对象") {
		t.Fatal("non-object arguments were executed or retained")
	}
}
