package groupchat

import (
	"sync"
	"testing"
	"time"

	"daidai/internal/entity"
)

type fakeInterjectionTimer struct {
	mu      sync.Mutex
	fireAt  time.Time
	fn      func()
	stopped bool
}

func (t *fakeInterjectionTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	wasActive := !t.stopped
	t.stopped = true
	return wasActive
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeInterjectionTimer
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, fn func()) InterjectionTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeInterjectionTimer{fireAt: c.now.Add(d), fn: fn}
	c.timers = append(c.timers, t)
	return t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()

	for {
		c.mu.Lock()
		var next *fakeInterjectionTimer
		nextIdx := -1
		for i, t := range c.timers {
			t.mu.Lock()
			stopped := t.stopped
			fireAt := t.fireAt
			t.mu.Unlock()
			if stopped || fireAt.After(target) {
				continue
			}
			if next == nil || fireAt.Before(next.fireAt) {
				next = t
				nextIdx = i
			}
		}
		if next == nil {
			c.now = target
			c.mu.Unlock()
			return
		}
		c.timers = append(c.timers[:nextIdx], c.timers[nextIdx+1:]...)
		next.mu.Lock()
		stopped := next.stopped
		next.stopped = true
		fn := next.fn
		c.now = next.fireAt
		next.mu.Unlock()
		c.mu.Unlock()
		if !stopped && fn != nil {
			fn()
		}
	}
}

func TestInterjectionScheduler_BurstDebounceAndMaxWait(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 27, 19, 0, 0, 0, time.UTC))
	sched := NewInterjectionScheduler(InterjectionConfig{
		Debounce:         2500 * time.Millisecond,
		MaxWait:          10 * time.Second,
		CheckCooldown:    30 * time.Second,
		SpeakCooldownMin: 3 * time.Minute,
		SpeakCooldownMax: 5 * time.Minute,
		Now:              clock.Now,
		AfterFunc:        clock.AfterFunc,
		RandFloat:        func() float64 { return 0.5 },
	})

	var dispatched []*entity.SensoryEvent
	collect := func(ev *entity.SensoryEvent) {
		dispatched = append(dispatched, ev)
	}

	// Lively chat: messages arrive every 2 seconds (faster than 2.5s debounce).
	for i := 1; i <= 5; i++ {
		sched.Observe("g1", &entity.SensoryEvent{
			ChannelID:    "g1",
			SenderID:     "u1",
			SenderName:   "甲",
			RawMessageID: string(rune('a' + i - 1)),
			Content:      "热聊消息",
		}, true, collect)
		if i < 5 {
			clock.Advance(2 * time.Second)
		}
	}
	if len(dispatched) != 0 {
		t.Fatalf("should not dispatch before MaxWait (8s elapsed), got %d", len(dispatched))
	}
	// At t=10s (2s after 5th message at t=8s), MaxWait caps the debounce and fires!
	clock.Advance(2 * time.Second)
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatch at MaxWait=10s during lively chat, got %d", len(dispatched))
	}
	if dispatched[0].RawMessageID != "e" {
		t.Fatalf("expected latest burst message 'e', got %q", dispatched[0].RawMessageID)
	}
}

func TestInterjectionScheduler_TwoTierCooldown(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 27, 19, 0, 0, 0, time.UTC))
	sched := NewInterjectionScheduler(InterjectionConfig{
		Debounce:         2 * time.Second,
		MaxWait:          10 * time.Second,
		CheckCooldown:    30 * time.Second,
		SpeakCooldownMin: 3 * time.Minute,
		SpeakCooldownMax: 5 * time.Minute,
		Now:              clock.Now,
		AfterFunc:        clock.AfterFunc,
		RandFloat:        func() float64 { return 0.5 }, // exact 4m speak cooldown
	})

	count := 0
	collect := func(*entity.SensoryEvent) { count++ }

	// First check at t=2s; bot decides NOT to speak (no NotifySpoke).
	sched.Observe("g1", &entity.SensoryEvent{ChannelID: "g1", SenderID: "u1", RawMessageID: "m1", Content: "hi"}, true, collect)
	clock.Advance(2 * time.Second)
	if count != 1 {
		t.Fatalf("expected first check to fire, got %d", count)
	}

	// 15s later: still inside 30s CheckCooldown.
	clock.Advance(15 * time.Second)
	sched.Observe("g1", &entity.SensoryEvent{ChannelID: "g1", SenderID: "u2", RawMessageID: "m2", Content: "yo"}, true, collect)
	clock.Advance(3 * time.Second)
	if count != 1 {
		t.Fatalf("expected CheckCooldown to block second check at 18s, got %d", count)
	}

	// After 30s CheckCooldown expires, bot can look again without waiting 4 minutes!
	clock.Advance(15 * time.Second)
	sched.Observe("g1", &entity.SensoryEvent{ChannelID: "g1", SenderID: "u2", RawMessageID: "m3", Content: "new topic"}, true, collect)
	clock.Advance(2 * time.Second)
	if count != 2 {
		t.Fatalf("expected second check after 30s CheckCooldown, got %d", count)
	}

	// Now bot actually speaks -> enters 4m SpeakCooldown.
	sched.NotifySpoke("g1")
	clock.Advance(2 * time.Minute)
	sched.Observe("g1", &entity.SensoryEvent{ChannelID: "g1", SenderID: "u1", RawMessageID: "m4", Content: "after 2m"}, true, collect)
	clock.Advance(3 * time.Second)
	if count != 2 {
		t.Fatalf("expected SpeakCooldown to block at 2m, got %d", count)
	}

	// After 4m SpeakCooldown expires, bot can look again.
	clock.Advance(2 * time.Minute)
	sched.Observe("g1", &entity.SensoryEvent{ChannelID: "g1", SenderID: "u1", RawMessageID: "m5", Content: "after 4m"}, true, collect)
	clock.Advance(2 * time.Second)
	if count != 3 {
		t.Fatalf("expected check after SpeakCooldown expired, got %d", count)
	}
}

func TestInterjectionScheduler_InFlightDriftAndAddressedInvalidation(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 27, 19, 0, 0, 0, time.UTC))
	sched := NewInterjectionScheduler(InterjectionConfig{
		Debounce:         2 * time.Second,
		MaxWait:          10 * time.Second,
		CheckCooldown:    30 * time.Second,
		SpeakCooldownMin: 3 * time.Minute,
		SpeakCooldownMax: 3 * time.Minute,
		MaxInFlightDrift: 2,
		Now:              clock.Now,
		AfterFunc:        clock.AfterFunc,
	})

	var dispatched *entity.SensoryEvent
	sched.Observe("g1", &entity.SensoryEvent{ChannelID: "g1", SenderID: "u1", RawMessageID: "m1", Content: "hello"}, true, func(ev *entity.SensoryEvent) {
		dispatched = ev
	})
	clock.Advance(2 * time.Second)
	if dispatched == nil || dispatched.StillValid == nil || !dispatched.StillValid() {
		t.Fatalf("expected valid dispatched interjection")
	}

	// 1st and 2nd new group messages during LLM thinking are tolerated.
	sched.Observe("g1", &entity.SensoryEvent{ChannelID: "g1", SenderID: "u2", RawMessageID: "m2", Content: "follow1"}, true, nil)
	if !dispatched.StillValid() {
		t.Fatalf("1 message drift should still be valid")
	}
	sched.Observe("g1", &entity.SensoryEvent{ChannelID: "g1", SenderID: "u3", RawMessageID: "m3", Content: "follow2"}, true, nil)
	if !dispatched.StillValid() {
		t.Fatalf("2 message drift should still be valid")
	}

	// 3rd message exceeds MaxInFlightDrift=2 -> stale.
	sched.Observe("g1", &entity.SensoryEvent{ChannelID: "g1", SenderID: "u4", RawMessageID: "m4", Content: "follow3"}, true, nil)
	if dispatched.StillValid() {
		t.Fatalf("3 message drift should invalidate in-flight interjection")
	}

	// Direct address immediately invalidates even with 0 drift.
	clock.Advance(35 * time.Second)
	dispatched = nil
	sched.Observe("g1", &entity.SensoryEvent{ChannelID: "g1", SenderID: "u1", RawMessageID: "m5", Content: "hello again"}, true, func(ev *entity.SensoryEvent) {
		dispatched = ev
	})
	clock.Advance(2 * time.Second)
	if dispatched == nil || !dispatched.StillValid() {
		t.Fatalf("expected second dispatched interjection to be valid")
	}
	sched.NotifyAddressed("g1", "u1", "甲")
	if dispatched.StillValid() {
		t.Fatalf("NotifyAddressed must immediately invalidate in-flight interjection")
	}
}
