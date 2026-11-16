package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/engine/memory"
	"daidai/internal/entity"
	"daidai/internal/groupchat"
)

type expressionModel struct {
	core.LLMProvider
	response string
	calls    int
	messages []core.ChatMessage
	tools    []core.ToolDefinition
	options  *core.ChatOptions
	after    func()
}

func (*expressionModel) Name() string { return "private-runtime-route" }
func (m *expressionModel) ChatWithTools(_ context.Context, messages []core.ChatMessage, tools []core.ToolDefinition, options *core.ChatOptions) (*core.ChatResponse, error) {
	m.calls++
	m.messages, m.tools, m.options = messages, tools, options
	if m.after != nil {
		m.after()
	}
	return &core.ChatResponse{Content: m.response}, nil
}

func expressionFixture(t *testing.T) (*Cortex, *expressionModel, *groupchat.Store, *entity.SensoryEvent) {
	t.Helper()
	database := db.NewManager(t.TempDir())
	mem := memory.NewMemoryEngine(nil)
	mem.SetDBManager(database)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = mem.Shutdown(ctx)
		_ = database.Close()
	})
	ctx := context.Background()
	for _, msg := range []struct{ kind, group, text string }{
		{"group", "-100/7", "刚把配置改好了，猫把键盘按亮了"},
		{"group", "-100/7", "我这边的猫刚好趴在 Nix 配置上"},
		{"group", "other-group", "other-group-secret"},
		{"private", "owner", "private-chat-secret"},
	} {
		if _, err := database.LogInboundMessage(ctx, "telegram", msg.kind, msg.group, "user", "群友", msg.text, msg.text); err != nil {
			t.Fatal(err)
		}
	}
	store := &groupchat.Store{Database: database}
	on := true
	if err := store.Update(ctx, "telegram", "-100/7", groupchat.Patch{Chatter: &on, Dream: &on}); err != nil {
		t.Fatal(err)
	}
	model := &expressionModel{response: `{"mode":"dream","text":"猫把每个配置项都变成了会发光的毛线球。","mention":0}`}
	cortex := NewCortex(CortexConfig{LLM: model, Memory: mem, Persona: entity.NewDefaultPersona(), GroupExpressions: store})
	event := &entity.SensoryEvent{Platform: "telegram", IsGroup: true, ChannelID: "-100/7", SenderID: "user", SenderName: "群友", Role: entity.RoleGuest, Content: "感觉今晚的键盘归猫管了", RawMessageID: "latest", GroupMessageObserved: true, GroupInterjection: true, OccurredAt: time.Now(), GroupParticipants: []core.GroupParticipant{{ID: "user", Name: "群友"}}}
	if err := mem.RecordInbound(ctx, event); err != nil {
		t.Fatal(err)
	}
	return cortex, model, store, event
}

func TestGroupDreamUsesOnlyGroupSceneAndCommitsCooldownAfterDelivery(t *testing.T) {
	cortex, model, store, event := expressionFixture(t)
	ctx := context.Background()
	u, err := cortex.Think(ctx, event)
	if err != nil || u == nil || !strings.HasPrefix(u.Text, "梦到个离谱画面") || len(u.MentionUserIDs) > 0 || event.GroupExpression != groupchat.Dream {
		t.Fatalf("invalid dream: %+v %v", u, err)
	}
	encoded, _ := json.Marshal(model.messages)
	for _, secret := range []string{"other-group-secret", "private-chat-secret", "private-runtime-route"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("non-group context leaked")
		}
	}
	if model.calls != 1 || len(model.tools) != 0 || model.options == nil || !model.options.ReadOnly {
		t.Fatal("group expression acquired tools or extra model calls")
	}
	state, err := store.Get(ctx, event.Platform, event.ChannelID)
	if err != nil || state.LastDreamMS != 0 {
		t.Fatal("cooldown started before delivery")
	}
	registry := entity.NewChannelRegistry()
	channel := &preparedViewChannel{fail: true}
	registry.Register(channel)
	if registry.Transmit(ctx, u) == nil {
		t.Fatal("test send should fail")
	}
	state, err = store.Get(ctx, event.Platform, event.ChannelID)
	if err != nil || state.LastDreamMS != 0 {
		t.Fatal("failed send started cooldown")
	}
	channel.fail = false
	if err = registry.Transmit(ctx, u); err != nil {
		t.Fatal(err)
	}
	state, err = store.Get(ctx, event.Platform, event.ChannelID)
	if err != nil || state.LastDreamMS == 0 {
		t.Fatal("successful send did not persist cooldown")
	}
	if again, err := cortex.Think(ctx, event); err != nil || again != nil {
		t.Fatal("dream bypassed successful-send cooldown")
	}
}

func TestGroupExpressionsFailClosedAndNormalRepliesRemainAvailable(t *testing.T) {
	for _, response := range []string{
		`{"mode":"dream","text":"故事","mention":1}`,
		`{"mode":"unknown","text":"不该发送"}`,
		`{"mode":"skip","text":"不该发送"}`,
		"not json",
	} {
		cortex, model, _, event := expressionFixture(t)
		model.response = response
		if u, err := cortex.Think(context.Background(), event); err != nil || u != nil {
			t.Fatalf("invalid response admitted: %s", response)
		}
	}
	cortex, model, store, event := expressionFixture(t)
	ctx := context.Background()
	off := false
	model.after = func() {
		if err := store.Update(ctx, event.Platform, event.ChannelID, groupchat.Patch{Dream: &off}); err != nil {
			t.Error(err)
		}
	}
	if u, err := cortex.Think(ctx, event); err != nil || u != nil {
		t.Fatal("disabled mode emitted an in-flight dream")
	}
	model.after = nil
	model.response = `{"mode":"reply","text":"原来我们都在给猫维护配置。","mention":1}`
	u, err := cortex.Think(ctx, event)
	if err != nil || u == nil || len(u.MentionUserIDs) != 1 || u.MentionUserIDs[0] != "user" {
		t.Fatal("normal reply or indexed mention regressed")
	}
	event.IsGroup = false
	before := model.calls
	if u, err = cortex.Think(ctx, event); err != nil || u != nil || model.calls != before {
		t.Fatal("private interjection was processed")
	}
}

func TestNewMessageAndColdConversationSuppressExpressions(t *testing.T) {
	cortex, model, _, event := expressionFixture(t)
	valid := true
	event.StillValid = func() bool { return valid }
	model.after = func() { valid = false }
	if u, err := cortex.Think(context.Background(), event); err != nil || u != nil {
		t.Fatal("stale group thought was sent")
	}
	valid = true
	model.after = nil
	event.ChannelID = "empty-group"
	on := true
	if err := cortex.groupExpressions.Update(context.Background(), event.Platform, event.ChannelID, groupchat.Patch{Chatter: &on, Dream: &on}); err != nil {
		t.Fatal(err)
	}
	if u, err := cortex.Think(context.Background(), event); err != nil || u != nil {
		t.Fatal("cold group received a dream")
	}
}
