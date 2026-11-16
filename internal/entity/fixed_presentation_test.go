package entity

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fixedPresentationChannel struct {
	Channel
	calls int
	first error
}

func (*fixedPresentationChannel) Name() string { return "fixture" }
func (c *fixedPresentationChannel) Transmit(context.Context, *Utterance) error {
	c.calls++
	if c.calls == 1 {
		return c.first
	}
	return nil
}
func TestFixedPresentationFallbackRequiresUnsentProof(t *testing.T) {
	for _, unsent := range []bool{false, true} {
		t.Run(map[bool]string{true: "preparation", false: "ambiguous"}[unsent], func(t *testing.T) {
			failure := error(errors.New("connection lost"))
			if unsent {
				failure = &UnsentError{Err: failure}
			}
			channel := &fixedPresentationChannel{first: failure}
			registry := NewChannelRegistry()
			registry.Register(channel)
			agent := NewSuperAgent(Config{Channels: registry})
			result, err := agent.transmitUtterance(context.Background(), &Utterance{Platform: "fixture", ChannelID: "1", FixedPresentation: true, Images: [][]byte{[]byte("image")}, FallbackText: "完整早报", Text: "原文索引"}, "owner")
			if unsent {
				if err != nil || channel.calls != 2 || !strings.Contains(result, "完整早报") {
					t.Fatalf("safe text fallback lost: %s %v calls=%d", result, err, channel.calls)
				}
			} else if err == nil || channel.calls != 1 {
				t.Fatal("ambiguous message blindly resent")
			}
		})
	}
}
func TestFixedPartialViewDoesNotCreateSecondContinuationQueue(t *testing.T) {
	channel := &fixedPresentationChannel{first: &PartialDeliveryError{Err: errors.New("failed"), DeliveredText: "已送原文", UndeliveredText: "剩余原文"}}
	registry := NewChannelRegistry()
	registry.Register(channel)
	agent := NewSuperAgent(Config{Channels: registry})
	result, err := agent.transmitUtterance(context.Background(), &Utterance{Platform: "fixture", ChannelID: "1", FixedPresentation: true, Images: [][]byte{[]byte("image")}}, "owner")
	if err != nil || result != "已送原文" || channel.calls != 1 {
		t.Fatal("partial view was resent")
	}
	if _, ok := agent.takeReplyContinuation(&SensoryEvent{Platform: "fixture", ChannelID: "1", SenderID: "owner"}); ok {
		t.Fatal("duplicate continuation queue created")
	}
}
