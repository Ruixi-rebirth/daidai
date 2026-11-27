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

func TestGroupContinuationChecksAddresseeWhenAmbiguous(t *testing.T) {
	ctx := context.Background()
	cortex, _, store, event := expressionFixture(t)
	event.GroupInterjection = false
	event.GroupContinuation = true
	event.GroupContinuationAmbiguous = true
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

func TestGroupContinuationDirectSkipsSecondaryClassifier(t *testing.T) {
	ctx := context.Background()
	cortex, _, store, event := expressionFixture(t)
	event.GroupInterjection = false
	event.GroupContinuation = true
	event.GroupContinuationAmbiguous = false
	event.Content = "我要10 11"
	event.HistoryRecordIDs = nil
	if _, err := store.Database.LogBotReply(ctx, event.Platform, "group", event.ChannelID, "在呢，咋啦", event.SenderID, 0); err != nil {
		t.Fatal(err)
	}
	// Even if the secondary classifier would return "skip", a direct clean-stream
	// continuation goes straight to the main brain without being dropped.
	model := &continuationModel{target: "skip", expressionModel: expressionModel{response: "来啦，第 10 和 11 条在这。"}}
	cortex.llm = model
	out, err := cortex.Think(ctx, event)
	if out == nil || err != nil || model.calls != 1 || out.Text != "来啦，第 10 和 11 条在这。" {
		t.Fatalf("direct continuation should reach main brain directly: out=%v err=%v calls=%d", out, err, model.calls)
	}
}

type namedBotJudgeModel struct {
	expressionModel
	target string
}

func (m *namedBotJudgeModel) Chat(_ context.Context, _ []core.ChatMessage, options *core.ChatOptions) (string, error) {
	if options == nil || !options.ReadOnly {
		return "skip", nil
	}
	return m.target, nil
}

func TestGroupContinuationNamedBotWithOtherMentionJudgedWithoutPriorBotReply(t *testing.T) {
	ctx := context.Background()
	cortex, _, _, event := expressionFixture(t)
	event.GroupInterjection = false
	event.GroupContinuation = true
	event.GroupContinuationAmbiguous = true
	event.Content = "呆呆把@爱在第58秒 头像做个撒娇表情包"
	event.HistoryRecordIDs = nil

	// No prior bot reply in history, yet message names the bot -> judge must still run.
	model := &namedBotJudgeModel{target: "bot", expressionModel: expressionModel{response: "好呀，马上画。"}}
	cortex.llm = model
	out, err := cortex.Think(ctx, event)
	if out == nil || err != nil || model.calls != 1 || out.Text != "好呀，马上画。" {
		t.Fatalf("named-bot ambiguous message should be judged and answered: out=%v err=%v calls=%d", out, err, model.calls)
	}
}
