package entity

import (
	"context"
	"testing"
	"time"
)

func TestClearSessionReplyContinuationsKeepsOtherChats(t *testing.T) {
	a := &SuperAgent{}
	ctx := context.Background()
	group := SensoryEvent{Platform: "qq", IsGroup: true, ChannelID: "1", SenderID: "owner"}
	member := group
	member.SenderID = "member"
	private := group
	private.IsGroup = false
	otherPlatform := group
	otherPlatform.Platform = "telegram"
	for _, event := range []*SensoryEvent{&group, &member, &private, &otherPlatform} {
		a.restoreReplyContinuation(ctx, event, pendingReplyContinuation{text: "待补发"})
	}
	a.clearSessionReplyContinuations(&group)

	for _, event := range []*SensoryEvent{&group, &member} {
		if _, ok := a.takeReplyContinuation(event, ""); ok {
			t.Fatalf("reset left pending text for %s", event.SenderID)
		}
	}
	for _, event := range []*SensoryEvent{&private, &otherPlatform} {
		if text, ok := a.takeReplyContinuation(event, ""); !ok || text.text != "待补发" {
			t.Fatalf("pending text lost for %s group=%t", event.Platform, event.IsGroup)
		}
	}
}

func TestSendDeadlineRetainsConfirmedUnsentTail(t *testing.T) {
	ctx := context.Background()
	sendCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	channel := &failingReplyChannel{mockChannel: mockChannel{name: "qq"}}
	channel.send = func(*Utterance) error {
		cancel()
		return &PartialDeliveryError{Err: sendCtx.Err(), DeliveredText: "已发送", UndeliveredText: "待发送"}
	}
	registry := NewChannelRegistry()
	registry.Register(channel)
	agent := &SuperAgent{Channels: registry}
	reply := &Utterance{Platform: "qq", ChannelID: "chat", DraftID: "draft", Text: "已发送待发送"}
	delivered, err := agent.transmitUtteranceWithContinuationContext(sendCtx, ctx, reply, "owner")
	if err != nil || delivered != "已发送" {
		t.Fatalf("unexpected delivery: %q, %v", delivered, err)
	}
	event := &SensoryEvent{Platform: "qq", ChannelID: "chat", SenderID: "owner"}
	pending, ok := agent.takeReplyContinuation(event, "draft")
	if !ok || pending.text != "待发送" {
		t.Fatalf("confirmed unsent text was lost: %q", pending.text)
	}
}

func TestReplyContinuationRetryKeepsQueueOrder(t *testing.T) {
	ctx := context.Background()
	event := &SensoryEvent{Platform: "qq", ChannelID: "chat", SenderID: "owner"}
	agent := &SuperAgent{}
	now := time.Now()
	key := thinkKey(event)
	agent.storeReplyContinuationLocked(replyContinuationKey{key, "first"}, "第一份", nil, time.Time{}, now.Add(-time.Minute))
	agent.storeReplyContinuationLocked(replyContinuationKey{key, "second"}, "第二份", nil, time.Time{}, now)
	first, ok := agent.takeReplyContinuation(event, "first")
	if !ok {
		t.Fatal("first reply missing")
	}
	agent.restoreReplyContinuation(ctx, event, first)
	restored, ok := agent.takeReplyContinuation(event, "")
	if !ok || restored.draftID != "first" {
		t.Fatalf("failed retry changed queue order: %+v", restored)
	}
}
