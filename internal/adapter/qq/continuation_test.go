package qq

import (
	"testing"

	"daidai/internal/entity"
	"daidai/internal/groupchat"
)

func TestPlainMentionEndsGroupContinuation(t *testing.T) {
	channel := &QQChannel{}
	channel.recordGroupActiveSession("group", "speaker")
	if got := channel.evaluateGroupContinuation("group", "speaker", "@另一位群友", nil, nil, false); got != groupchat.ContinuationNone {
		t.Fatalf("an unstructured mention of another member must not continue the bot's conversation, got %v", got)
	}
	if !qqDirectedAtOtherMember(nil, "@另一位群友") {
		t.Fatal("an unstructured mention must also be ineligible for unsolicited interjection")
	}
}

func TestEvaluateGroupContinuation_FullLifecycle(t *testing.T) {
	q := &QQChannel{}
	groupID := "test_group_1"
	speakerID := "speaker_1"

	q.recordGroupActiveSession(groupID, speakerID)

	if got := q.evaluateGroupContinuation(groupID, speakerID, "我要10 11", map[string]any{}, nil, false); got != groupchat.ContinuationDirect {
		t.Fatalf("expected turn 1 to be ContinuationDirect, got %v", got)
	}
	if got := q.evaluateGroupContinuation(groupID, speakerID, "后天会不会下雨？", map[string]any{}, nil, false); got != groupchat.ContinuationDirect {
		t.Fatalf("expected turn 2 to be ContinuationDirect, got %v", got)
	}
	if got := q.evaluateGroupContinuation(groupID, speakerID, "大后天呢", map[string]any{}, nil, false); got != groupchat.ContinuationNone {
		t.Fatalf("expected turn 3 after exhaustion to be ContinuationNone, got %v", got)
	}
}

func TestEvaluateGroupContinuation_InterleavingBecomesAmbiguousThenBreaks(t *testing.T) {
	q := &QQChannel{}
	groupID := "test_group_interleave"
	speakerID := "user_a"

	q.recordGroupActiveSession(groupID, speakerID)

	if got := q.evaluateGroupContinuation(groupID, "user_b", "哈哈今天真冷", map[string]any{}, nil, false); got != groupchat.ContinuationNone {
		t.Fatalf("other user message should be ContinuationNone, got %v", got)
	}
	if got := q.evaluateGroupContinuation(groupID, "user_c", "确实好冷", map[string]any{}, nil, false); got != groupchat.ContinuationNone {
		t.Fatalf("second other user message should be ContinuationNone, got %v", got)
	}

	// Within tolerance (2 interleaved messages), target speaker's follow-up is Ambiguous
	// regardless of whether it contains specific Chinese keywords.
	if got := q.evaluateGroupContinuation(groupID, speakerID, "我要10 11", map[string]any{}, nil, false); got != groupchat.ContinuationAmbiguous {
		t.Fatalf("expected interleaved follow-up to be ContinuationAmbiguous, got %v", got)
	}

	// More than 2 interleaved messages breaks the session.
	q.evaluateGroupContinuation(groupID, "user_b", "msg 1", map[string]any{}, nil, false)
	q.evaluateGroupContinuation(groupID, "user_c", "msg 2", map[string]any{}, nil, false)
	q.evaluateGroupContinuation(groupID, "user_d", "msg 3", map[string]any{}, nil, false)
	if got := q.evaluateGroupContinuation(groupID, speakerID, "那怎么办", map[string]any{}, nil, false); got != groupchat.ContinuationNone {
		t.Fatalf("expected session to break after 3 interleaved messages, got %v", got)
	}
}

func TestEvaluateGroupContinuation_NegativeChecksAndQuoteExemption(t *testing.T) {
	q := &QQChannel{}
	groupID := "test_group_neg"
	speakerID := "user_a"

	// Mentioning another member breaks session.
	q.recordGroupActiveSession(groupID, speakerID)
	mentionsOther := map[string]any{
		"mentions": []any{map[string]any{"id": "other_user", "bot": false}},
	}
	if got := q.evaluateGroupContinuation(groupID, speakerID, "你觉得呢", mentionsOther, nil, false); got != groupchat.ContinuationNone {
		t.Fatalf("mentioning another user must return ContinuationNone, got %v", got)
	}

	// Quoting another member breaks session.
	q.recordGroupActiveSession(groupID, speakerID)
	otherReplyCtx := &entity.SensoryEvent{SenderID: "other_user", Content: "别人说的话"}
	if got := q.evaluateGroupContinuation(groupID, speakerID, "确实是这样", map[string]any{}, otherReplyCtx, false); got != groupchat.ContinuationNone {
		t.Fatalf("quoting another user must return ContinuationNone, got %v", got)
	}

	// Slash command breaks session.
	q.recordGroupActiveSession(groupID, speakerID)
	if got := q.evaluateGroupContinuation(groupID, speakerID, "/help", map[string]any{}, nil, false); got != groupchat.ContinuationNone {
		t.Fatalf("slash command must return ContinuationNone, got %v", got)
	}

	// Quoting the bot resets turns and returns ContinuationDirect.
	q.recordGroupActiveSession(groupID, speakerID)
	q.evaluateGroupContinuation(groupID, speakerID, "那明天呢", map[string]any{}, nil, false)
	if got := q.evaluateGroupContinuation(groupID, speakerID, "这个报错怎么看？", map[string]any{}, nil, true); got != groupchat.ContinuationDirect {
		t.Fatalf("quoting the bot must return ContinuationDirect, got %v", got)
	}

	// Open session (empty speakerID) locks onto first responder.
	q.recordGroupActiveSession("open_group", "")
	if got := q.evaluateGroupContinuation("open_group", "user_first", "梦到啥离谱的了说说看", map[string]any{}, nil, false); got != groupchat.ContinuationDirect {
		t.Fatalf("open session first responder should get ContinuationDirect, got %v", got)
	}
}
