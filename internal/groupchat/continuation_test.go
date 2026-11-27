package groupchat

import (
	"testing"
	"time"
)

func TestContinuationTracker_DirectAndInterleavedLifecycle(t *testing.T) {
	tracker := NewContinuationTracker()
	base := time.Date(2026, 11, 27, 8, 29, 41, 0, time.Local)
	tracker.Record("g1", "user_a", base)

	// Clean stream: same speaker 14s later with no interleaved speakers -> Direct.
	got := tracker.Evaluate(ContinuationInput{
		GroupID:   "g1",
		SpeakerID: "user_a",
		Content:   "我要10 11",
		Now:       base.Add(14 * time.Second),
	})
	if got != ContinuationDirect {
		t.Fatalf("clean stream decision = %v, want ContinuationDirect", got)
	}

	// One other member chimes in -> None, and increments interleaved count.
	got = tracker.Evaluate(ContinuationInput{
		GroupID:   "g1",
		SpeakerID: "user_b",
		Content:   "早啊",
		Now:       base.Add(20 * time.Second),
	})
	if got != ContinuationNone {
		t.Fatalf("other member decision = %v, want ContinuationNone", got)
	}

	// Same speaker follows up after 1 interleaved message -> Ambiguous (no keyword glue).
	got = tracker.Evaluate(ContinuationInput{
		GroupID:   "g1",
		SpeakerID: "user_a",
		Content:   "我要10 11",
		Now:       base.Add(25 * time.Second),
	})
	if got != ContinuationAmbiguous {
		t.Fatalf("interleaved stream decision = %v, want ContinuationAmbiguous", got)
	}

	// Turns exhausted after 2 turns -> None.
	got = tracker.Evaluate(ContinuationInput{
		GroupID:   "g1",
		SpeakerID: "user_a",
		Content:   "再来一条",
		Now:       base.Add(30 * time.Second),
	})
	if got != ContinuationNone {
		t.Fatalf("post-exhaustion decision = %v, want ContinuationNone", got)
	}
}

func TestContinuationTracker_ExclusionsAndExpiry(t *testing.T) {
	tracker := NewContinuationTracker()
	base := time.Date(2026, 11, 27, 8, 30, 0, 0, time.Local)

	// Expired session returns None.
	tracker.Record("g1", "user_a", base)
	if got := tracker.Evaluate(ContinuationInput{
		GroupID:   "g1",
		SpeakerID: "user_a",
		Content:   "还在吗",
		Now:       base.Add(DefaultContinuationTTL + time.Second),
	}); got != ContinuationNone {
		t.Fatalf("expired decision = %v, want ContinuationNone", got)
	}

	// Mentioning or quoting another member breaks the session.
	tracker.Record("g1", "user_a", base)
	if got := tracker.Evaluate(ContinuationInput{
		GroupID:        "g1",
		SpeakerID:      "user_a",
		Content:        "你看看",
		MentionedOther: true,
		Now:            base.Add(5 * time.Second),
	}); got != ContinuationNone {
		t.Fatalf("mentioned other decision = %v, want ContinuationNone", got)
	}
	if got := tracker.Evaluate(ContinuationInput{
		GroupID:   "g1",
		SpeakerID: "user_a",
		Content:   "继续",
		Now:       base.Add(6 * time.Second),
	}); got != ContinuationNone {
		t.Fatalf("session should be cleared after MentionedOther, got %v", got)
	}

	// More than MaxInterleavedMessages breaks the session.
	tracker.Record("g1", "user_a", base)
	for i := 0; i <= MaxInterleavedMessages; i++ {
		tracker.Evaluate(ContinuationInput{
			GroupID:   "g1",
			SpeakerID: "other",
			Content:   "插话",
			Now:       base.Add(time.Duration(i+1) * time.Second),
		})
	}
	if got := tracker.Evaluate(ContinuationInput{
		GroupID:   "g1",
		SpeakerID: "user_a",
		Content:   "那怎么办",
		Now:       base.Add(10 * time.Second),
	}); got != ContinuationNone {
		t.Fatalf("session should break after >MaxInterleavedMessages, got %v", got)
	}

	// Question mark is NOT treated as punctuation noise; pure dots ARE noise.
	tracker.Record("g1", "user_a", base)
	if got := tracker.Evaluate(ContinuationInput{
		GroupID:   "g1",
		SpeakerID: "user_a",
		Content:   "...",
		Now:       base.Add(2 * time.Second),
	}); got != ContinuationNone {
		t.Fatalf("pure punctuation should be ContinuationNone, got %v", got)
	}
	if got := tracker.Evaluate(ContinuationInput{
		GroupID:   "g1",
		SpeakerID: "user_a",
		Content:   "？",
		Now:       base.Add(3 * time.Second),
	}); got != ContinuationDirect {
		t.Fatalf("question mark follow-up should be ContinuationDirect, got %v", got)
	}
}
