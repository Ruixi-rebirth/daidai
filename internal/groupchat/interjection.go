package groupchat

import (
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
)

const (
	defaultInterjectionDebounce         = 2500 * time.Millisecond
	defaultInterjectionMaxWait          = 10 * time.Second
	defaultInterjectionCheckCooldown    = 30 * time.Second
	defaultInterjectionSpeakCooldownMin = 3 * time.Minute
	defaultInterjectionSpeakCooldownMax = 5*time.Minute + 30*time.Second
	defaultInterjectionMaxInFlightDrift = 2
	defaultInterjectionMaxGroups        = 1024
	maxTrackedGroupParticipants         = 20
)

// InterjectionTimer abstracts time.Timer for deterministic unit tests.
type InterjectionTimer interface {
	Stop() bool
}

// InterjectionConfig tunes group interjection pacing and cooldowns.
type InterjectionConfig struct {
	Debounce         time.Duration
	MaxWait          time.Duration
	CheckCooldown    time.Duration
	SpeakCooldownMin time.Duration
	SpeakCooldownMax time.Duration
	MaxInFlightDrift int
	MaxGroups        int
	Now              func() time.Time
	RandFloat        func() float64
	AfterFunc        func(time.Duration, func()) InterjectionTimer
}

type interjectionGroupState struct {
	lastSeen          time.Time
	participants      []core.GroupParticipant
	burstStart        time.Time
	pending           *entity.SensoryEvent
	pendingDispatch   func(*entity.SensoryEvent)
	timer             InterjectionTimer
	timerToken        uint64
	msgSeq            uint64
	addressedSeq      uint64
	lastCheckedAt     time.Time
	speakBlockedUntil time.Time
}

// InterjectionScheduler coordinates human-like group interjection timing across
// platforms (QQ, Telegram):
//   - Short debounce settles rapid bursts from one speaker, while MaxWait ensures
//     lively group conversations are still sampled instead of starving until silence.
//   - Two-tier cooldown separates "looked and stayed quiet" (short CheckCooldown)
//     from "actually spoke" (longer jittered SpeakCooldown).
//   - In-flight StillValid tolerates a small drift of new non-addressed messages
//     while thinking, but immediately cancels if someone addresses the bot or the
//     conversation moves on.
type InterjectionScheduler struct {
	mu      sync.Mutex
	cfg     InterjectionConfig
	groups  map[string]*interjectionGroupState
	stopped bool
}

// NewInterjectionScheduler constructs a scheduler with sensible defaults.
func NewInterjectionScheduler(cfg InterjectionConfig) *InterjectionScheduler {
	if cfg.Debounce <= 0 {
		cfg.Debounce = defaultInterjectionDebounce
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = defaultInterjectionMaxWait
	}
	if cfg.MaxWait < cfg.Debounce {
		cfg.MaxWait = cfg.Debounce
	}
	if cfg.CheckCooldown <= 0 {
		cfg.CheckCooldown = defaultInterjectionCheckCooldown
	}
	if cfg.SpeakCooldownMin <= 0 {
		cfg.SpeakCooldownMin = defaultInterjectionSpeakCooldownMin
	}
	if cfg.SpeakCooldownMax < cfg.SpeakCooldownMin {
		cfg.SpeakCooldownMax = defaultInterjectionSpeakCooldownMax
		if cfg.SpeakCooldownMax < cfg.SpeakCooldownMin {
			cfg.SpeakCooldownMax = cfg.SpeakCooldownMin
		}
	}
	if cfg.MaxInFlightDrift < 0 {
		cfg.MaxInFlightDrift = 0
	} else if cfg.MaxInFlightDrift == 0 {
		cfg.MaxInFlightDrift = defaultInterjectionMaxInFlightDrift
	}
	if cfg.MaxGroups <= 0 {
		cfg.MaxGroups = defaultInterjectionMaxGroups
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RandFloat == nil {
		cfg.RandFloat = rand.Float64
	}
	if cfg.AfterFunc == nil {
		cfg.AfterFunc = func(d time.Duration, fn func()) InterjectionTimer {
			return time.AfterFunc(d, fn)
		}
	}
	return &InterjectionScheduler{
		cfg:    cfg,
		groups: make(map[string]*interjectionGroupState),
	}
}

func (s *InterjectionScheduler) now() time.Time {
	return s.cfg.Now()
}

func (s *InterjectionScheduler) nextSpeakCooldown() time.Duration {
	span := s.cfg.SpeakCooldownMax - s.cfg.SpeakCooldownMin
	if span <= 0 {
		return s.cfg.SpeakCooldownMin
	}
	r := s.cfg.RandFloat()
	if r < 0 {
		r = 0
	} else if r >= 1 {
		r = 0.999999
	}
	return s.cfg.SpeakCooldownMin + time.Duration(float64(span)*r)
}

func (s *InterjectionScheduler) ensureGroupLocked(groupID string, now time.Time) *interjectionGroupState {
	if s.groups == nil {
		s.groups = make(map[string]*interjectionGroupState)
	}
	if g := s.groups[groupID]; g != nil {
		g.lastSeen = now
		return g
	}
	if len(s.groups) >= s.cfg.MaxGroups {
		var oldestID string
		var oldestAt time.Time
		for id, st := range s.groups {
			if oldestID == "" || st.lastSeen.Before(oldestAt) {
				oldestID, oldestAt = id, st.lastSeen
			}
		}
		if oldest := s.groups[oldestID]; oldest != nil {
			if oldest.timer != nil {
				oldest.timer.Stop()
				oldest.timer = nil
			}
			delete(s.groups, oldestID)
		}
	}
	g := &interjectionGroupState{lastSeen: now}
	s.groups[groupID] = g
	return g
}

func recordParticipantLocked(g *interjectionGroupState, senderID, senderName string) {
	id := strings.TrimSpace(senderID)
	name := strings.TrimSpace(senderName)
	if id == "" && name == "" {
		return
	}
	participants := make([]core.GroupParticipant, 0, min(len(g.participants)+1, maxTrackedGroupParticipants))
	participants = append(participants, core.GroupParticipant{ID: id, Name: name})
	for _, p := range g.participants {
		if (id != "" && p.ID == id) || (id == "" && p.Name == name) {
			continue
		}
		if len(participants) >= maxTrackedGroupParticipants {
			break
		}
		participants = append(participants, p)
	}
	g.participants = participants
}

func clearPendingLocked(g *interjectionGroupState) {
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
	g.timerToken++
	g.pending = nil
	g.pendingDispatch = nil
	g.burstStart = time.Time{}
}

// Observe records an unaddressed group message. When eligible is true and the
// group is outside speak/check cooldowns, it schedules (or updates) an
// evaluation timer bounded by Debounce and MaxWait.
func (s *InterjectionScheduler) Observe(groupID string, event *entity.SensoryEvent, eligible bool, onDispatch func(*entity.SensoryEvent)) {
	groupID = strings.TrimSpace(groupID)
	if s == nil || groupID == "" || event == nil {
		return
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	g := s.ensureGroupLocked(groupID, now)
	g.msgSeq++
	recordParticipantLocked(g, event.SenderID, event.SenderName)

	if !eligible || onDispatch == nil {
		clearPendingLocked(g)
		return
	}
	if !g.speakBlockedUntil.IsZero() && now.Before(g.speakBlockedUntil) {
		clearPendingLocked(g)
		return
	}
	if !g.lastCheckedAt.IsZero() && now.Sub(g.lastCheckedAt) < s.cfg.CheckCooldown {
		clearPendingLocked(g)
		return
	}

	if g.burstStart.IsZero() {
		g.burstStart = now
	}
	g.pending = event
	g.pendingDispatch = onDispatch

	remainingMaxWait := s.cfg.MaxWait - now.Sub(g.burstStart)
	delay := s.cfg.Debounce
	if remainingMaxWait <= 0 {
		delay = 0
	} else if remainingMaxWait < delay {
		delay = remainingMaxWait
	}

	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
	g.timerToken++
	token := g.timerToken
	if delay == 0 {
		s.fireLocked(groupID, g, token, now)
		return
	}
	g.timer = s.cfg.AfterFunc(delay, func() {
		s.onTimer(groupID, g, token)
	})
}

func (s *InterjectionScheduler) onTimer(groupID string, g *interjectionGroupState, token uint64) {
	s.mu.Lock()
	if s.stopped || s.groups[groupID] != g || g.timerToken != token {
		s.mu.Unlock()
		return
	}
	dispatchFn, candidate := s.prepareDispatchLocked(groupID, g, s.now())
	s.mu.Unlock()
	if dispatchFn != nil && candidate != nil {
		dispatchFn(candidate)
	}
}

func (s *InterjectionScheduler) fireLocked(groupID string, g *interjectionGroupState, token uint64, now time.Time) {
	if g.timerToken != token {
		return
	}
	dispatchFn, candidate := s.prepareDispatchLocked(groupID, g, now)
	if dispatchFn != nil && candidate != nil {
		// Dispatch asynchronously so callers of Observe never deadlock if onDispatch re-enters.
		go dispatchFn(candidate)
	}
}

func (s *InterjectionScheduler) prepareDispatchLocked(groupID string, g *interjectionGroupState, now time.Time) (func(*entity.SensoryEvent), *entity.SensoryEvent) {
	candidate := g.pending
	dispatchFn := g.pendingDispatch
	clearPendingLocked(g)
	if candidate == nil || dispatchFn == nil {
		return nil, nil
	}
	if !g.speakBlockedUntil.IsZero() && now.Before(g.speakBlockedUntil) {
		return nil, nil
	}
	if !g.lastCheckedAt.IsZero() && now.Sub(g.lastCheckedAt) < s.cfg.CheckCooldown {
		return nil, nil
	}
	g.lastCheckedAt = now

	snapMsgSeq := g.msgSeq
	snapAddrSeq := g.addressedSeq
	participants := append([]core.GroupParticipant(nil), g.participants...)

	ev := *candidate
	ev.GroupInterjection = true
	if len(ev.GroupParticipants) == 0 {
		ev.GroupParticipants = participants
	}
	ev.StillValid = func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.stopped {
			return false
		}
		cur := s.groups[groupID]
		if cur != g {
			return false
		}
		if cur.addressedSeq != snapAddrSeq {
			return false
		}
		if !cur.speakBlockedUntil.IsZero() && s.now().Before(cur.speakBlockedUntil) {
			return false
		}
		if cur.msgSeq < snapMsgSeq {
			return false
		}
		return cur.msgSeq-snapMsgSeq <= uint64(s.cfg.MaxInFlightDrift)
	}
	return dispatchFn, &ev
}

// NotifyAddressed marks that the bot was directly addressed in this group.
// It cancels any pending interjection, invalidates in-flight interjection
// thinking, and refreshes the speak cooldown so the bot does not immediately
// chime in unprompted right after a direct exchange.
func (s *InterjectionScheduler) NotifyAddressed(groupID string, senderID, senderName string) {
	groupID = strings.TrimSpace(groupID)
	if s == nil || groupID == "" {
		return
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	g := s.ensureGroupLocked(groupID, now)
	recordParticipantLocked(g, senderID, senderName)
	clearPendingLocked(g)
	g.addressedSeq++
	g.speakBlockedUntil = now.Add(s.nextSpeakCooldown())
}

// NotifySpoke marks that the bot successfully delivered a message in this group,
// entering the longer jittered speak cooldown.
func (s *InterjectionScheduler) NotifySpoke(groupID string) {
	groupID = strings.TrimSpace(groupID)
	if s == nil || groupID == "" {
		return
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	g := s.ensureGroupLocked(groupID, now)
	clearPendingLocked(g)
	g.addressedSeq++
	g.speakBlockedUntil = now.Add(s.nextSpeakCooldown())
}

// Cancel drops any pending or in-flight interjection for the group without
// imposing a long speak cooldown (e.g., when another group bot speaks).
func (s *InterjectionScheduler) Cancel(groupID string) {
	groupID = strings.TrimSpace(groupID)
	if s == nil || groupID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.groups[groupID]; g != nil {
		clearPendingLocked(g)
		g.addressedSeq++
	}
}

// Participants returns a snapshot of recently observed speakers in the group.
func (s *InterjectionScheduler) Participants(groupID string) []core.GroupParticipant {
	groupID = strings.TrimSpace(groupID)
	if s == nil || groupID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.groups[groupID]
	if g == nil || len(g.participants) == 0 {
		return nil
	}
	return append([]core.GroupParticipant(nil), g.participants...)
}

// Stop halts all pending timers and invalidates all in-flight StillValid checks.
func (s *InterjectionScheduler) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	for id, g := range s.groups {
		clearPendingLocked(g)
		delete(s.groups, id)
	}
}

// Reset clears stopped state and any leftover timers/state (e.g., on channel restart).
func (s *InterjectionScheduler) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, g := range s.groups {
		clearPendingLocked(g)
		delete(s.groups, id)
	}
	s.stopped = false
}
