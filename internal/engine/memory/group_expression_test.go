package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
	"daidai/internal/groupchat"
)

type dreamFactModel struct {
	core.LLMProvider
	messages []core.ChatMessage
	called   chan struct{}
}

func (m *dreamFactModel) Chat(_ context.Context, messages []core.ChatMessage, _ *core.ChatOptions) (string, error) {
	m.messages = messages
	m.called <- struct{}{}
	return `{"mutations":[]}`, nil
}

func TestDreamHistoryIsMarkedFictionAndExcludedFromFactInput(t *testing.T) {
	database := db.NewManager(t.TempDir())
	t.Cleanup(func() { _ = database.Close() })
	model := &dreamFactModel{called: make(chan struct{}, 1)}
	engine := NewMemoryEngine(model)
	engine.SetDBManager(database)
	ctx := context.Background()
	event := &entity.SensoryEvent{Platform: "qq", IsGroup: true, ChannelID: "group", SenderID: "user", Content: "我今天在整理 dotfiles", GroupInterjection: true, GroupExpression: groupchat.Dream}
	engine.Commit(ctx, event, "梦到个离谱画面：有人在火星用猫爪驾驶飞船。")
	select {
	case <-model.called:
	case <-time.After(time.Second):
		t.Fatal("real human message was not considered for facts")
	}
	wait, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := engine.Shutdown(wait); err != nil {
		t.Fatal(err)
	}
	if len(model.messages) == 0 {
		t.Fatal("real human message was not considered for facts")
	}
	for _, message := range model.messages {
		if strings.Contains(message.Content, "火星") || strings.Contains(message.Content, "猫爪驾驶") {
			t.Fatal("imaginary reply entered personal-fact extraction")
		}
	}
	conn, err := database.GetConn("qq")
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err = conn.QueryRowContext(ctx, `SELECT text FROM messages WHERE chat_type='group' AND conv_id='group' AND is_bot=1`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "[呆呆的梦境联想：虚构故事") || !strings.Contains(stored, "火星") {
		t.Fatal("delivered fiction lost its history label")
	}
}
