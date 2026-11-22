package chat

import (
	"context"
	"strings"
	"testing"

	"daidai/internal/core"
)

type continuationModel struct {
	expressionModel
	target string
}

func (m *continuationModel) Chat(_ context.Context, messages []core.ChatMessage, options *core.ChatOptions) (string, error) {
	if options == nil || !options.ReadOnly || !strings.Contains(messages[1].Content, `"text":"这是上一条机器人回复","reply_target":"current_speaker"`) {
		return "skip", nil
	}
	return m.target, nil
}

func TestGroupContinuationChecksAddresseeBeforeAnswering(t *testing.T) {
	ctx := context.Background()
	cortex, _, store, event := expressionFixture(t)
	event.GroupInterjection = false
	event.GroupContinuation = true
	event.HistoryRecordIDs = nil
	if _, err := store.Database.LogBotReply(ctx, event.Platform, "group", event.ChannelID, "这是上一条机器人回复", event.SenderID, 0); err != nil {
		t.Fatal(err)
	}
	model := &continuationModel{target: "skip", expressionModel: expressionModel{response: "接上这句。"}}
	cortex.llm = model
	out, err := cortex.Think(ctx, event)
	if out != nil || err != nil || model.calls != 0 {
		t.Fatalf("a message to another participant reached normal answering: %v %v", out, err)
	}
	model.target = "bot"
	out, err = cortex.Think(ctx, event)
	if out == nil || err != nil || model.calls != 1 || out.Text != "接上这句。" {
		t.Fatalf("a confirmed continuation did not reach normal answering: %v %v", out, err)
	}
	for _, msg := range model.messages {
		if strings.Contains(msg.Content, "reply_target") || strings.Contains(msg.Content, "这条回复的对象") {
			t.Fatal("the normal reply model saw continuation-only metadata")
		}
	}
}
