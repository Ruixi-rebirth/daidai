package runtime

import (
	"context"
	"daidai/internal/entity"
	"errors"
	"testing"
)

type fixedPresentationChannel struct {
	entity.Channel
	calls int
	first error
}

func (*fixedPresentationChannel) Name() string { return "fixture" }
func (c *fixedPresentationChannel) Transmit(context.Context, *entity.Utterance) error {
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
				failure = &entity.UnsentError{Err: failure}
			}
			channel := &fixedPresentationChannel{first: failure}
			registry := entity.NewChannelRegistry()
			registry.Register(channel)
			agent := NewSuperAgent(Config{Channels: registry})
			result, err := agent.transmitUtterance(context.Background(), &entity.Utterance{Platform: "fixture", ChannelID: "1", FixedPresentation: true, Images: [][]byte{[]byte("image")}, FallbackText: "完整早报", Text: "原文索引"}, "owner")
			if unsent {
				if err != nil || channel.calls != 2 || result != "完整早报\n\n原文索引" {
					t.Fatalf("safe text fallback lost: %s %v calls=%d", result, err, channel.calls)
				}
			} else if err == nil || channel.calls != 1 {
				t.Fatal("ambiguous message blindly resent")
			}
		})
	}
}
func TestFixedPartialViewDoesNotCreateSecondContinuationQueue(t *testing.T) {
	channel := &fixedPresentationChannel{first: &entity.PartialDeliveryError{Err: errors.New("failed"), DeliveredText: "已送原文", UndeliveredText: "剩余原文"}}
	registry := entity.NewChannelRegistry()
	registry.Register(channel)
	agent := NewSuperAgent(Config{Channels: registry})
	result, err := agent.transmitUtterance(context.Background(), &entity.Utterance{Platform: "fixture", ChannelID: "1", FixedPresentation: true, Images: [][]byte{[]byte("image")}}, "owner")
	if err != nil || result != "已送原文" || channel.calls != 1 {
		t.Fatal("partial view was resent")
	}
	if _, ok := agent.takeReplyContinuation(&entity.SensoryEvent{Platform: "fixture", ChannelID: "1", SenderID: "owner"}, ""); ok {
		t.Fatal("duplicate continuation queue created")
	}
}

func TestOrdinaryMediaFallbackRequiresKnownRejection(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		failure  error
		fallback bool
	}{
		{"ambiguous receipt", errors.New("connection lost"), false},
		{"explicit rejection", &entity.DeliveryRejection{Err: errors.New("unsupported image")}, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			channel := &fixedPresentationChannel{first: scenario.failure}
			registry := entity.NewChannelRegistry()
			registry.Register(channel)
			agent := NewSuperAgent(Config{Channels: registry})
			u := &entity.Utterance{Platform: "fixture", ChannelID: "chat", Images: [][]byte{[]byte("image")}, FallbackText: "正文"}
			text, err := agent.transmitUtterance(context.Background(), u, "owner")
			if scenario.fallback {
				if err != nil || text != "正文" || channel.calls != 2 {
					t.Fatalf("safe fallback failed: %q, %v, calls=%d", text, err, channel.calls)
				}
			} else if !errors.Is(err, scenario.failure) || channel.calls != 1 {
				t.Fatalf("ambiguous media was retried: %v, calls=%d", err, channel.calls)
			}
		})
	}
}

func TestHelpCardDoesNotResendAfterAmbiguousReceipt(t *testing.T) {
	failure := errors.New("connection lost")
	channel := &fixedPresentationChannel{first: failure}
	registry := entity.NewChannelRegistry()
	registry.Register(channel)
	agent := NewSuperAgent(Config{Channels: registry, Cognition: &pendingCardRenderer{}})
	event := &entity.SensoryEvent{Platform: "fixture", ChannelID: "chat", SenderID: "owner"}
	err := agent.transmitInfo(context.Background(), event, "帮助", "命令列表", true)
	if !errors.Is(err, failure) || channel.calls != 1 {
		t.Fatalf("help card delivery was retried: %v, calls=%d", err, channel.calls)
	}
}

func TestFixedCardFallbackPreservesPerImageSources(t *testing.T) {
	channel := &fixedPresentationChannel{first: &entity.UnsentError{Err: errors.New("image preparation failed")}}
	registry := entity.NewChannelRegistry()
	registry.Register(channel)
	agent := NewSuperAgent(Config{Channels: registry})
	u := &entity.Utterance{
		Platform: "fixture", ChannelID: "chat", FixedPresentation: true,
		Images: [][]byte{{1}, {2}, {3}}, ImageSources: []string{"第一页", "第二页", "第二页"},
		Text: "链接索引",
	}
	text, err := agent.transmitUtterance(context.Background(), u, "owner")
	if err != nil || text != "第一页\n\n第二页\n\n链接索引" || channel.calls != 2 {
		t.Fatalf("fallback lost image source text: %q, %v, calls=%d", text, err, channel.calls)
	}
}
