package groupchat

import (
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	// DefaultContinuationTurns is the maximum number of non-@ follow-up turns
	// allowed within one active group session before expiring.
	DefaultContinuationTurns = 2

	// DefaultContinuationTTL is how long an active group session stays open
	// after the bot replies or the target speaker continues the conversation.
	DefaultContinuationTTL = 45 * time.Second

	// MaxInterleavedMessages is the maximum number of messages from other group
	// members tolerated before assuming the group focus has moved on.
	MaxInterleavedMessages = 2
)

// ContinuationDecision classifies a non-@ group message relative to an active
// bot conversation.
type ContinuationDecision uint8

const (
	// ContinuationNone means the message should not trigger a continuation reply.
	ContinuationNone ContinuationDecision = iota
	// ContinuationDirect means high-confidence direct continuation (same speaker
	// within TTL with zero interleaved speakers, or explicit quote/name).
	ContinuationDirect
	// ContinuationAmbiguous means same speaker within TTL after 1..MaxInterleaved
	// other members spoke in between; Cortex should confirm the addressee.
	ContinuationAmbiguous
)

// ContinuationInput contains platform-agnostic protocol facts for a group message.
type ContinuationInput struct {
	GroupID        string
	SpeakerID      string
	Content        string
	MentionedOther bool
	QuotedOther    bool
	QuotedBot      bool
	NamedBot       bool
	Now            time.Time
}

type activeSession struct {
	speakerID        string
	remainingTurns   int
	expiresAt        time.Time
	interleavedCount int
}

// ContinuationTracker tracks per-group active non-@ follow-up sessions.
// It is platform-agnostic and safe for concurrent use.
type ContinuationTracker struct {
	mu       sync.Mutex
	sessions map[string]*activeSession
}

// NewContinuationTracker creates a new per-group continuation tracker.
func NewContinuationTracker() *ContinuationTracker {
	return &ContinuationTracker{
		sessions: make(map[string]*activeSession),
	}
}

// Record creates or refreshes an active continuation session for groupID and speakerID.
// An empty speakerID creates an open session (e.g. after proactive chatter) that locks
// onto the first responding member.
func (t *ContinuationTracker) Record(groupID, speakerID string, now time.Time) {
	if t == nil || groupID == "" {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.sessions == nil {
		t.sessions = make(map[string]*activeSession)
	}
	t.sessions[groupID] = &activeSession{
		speakerID:        speakerID,
		remainingTurns:   DefaultContinuationTurns,
		expiresAt:        now.Add(DefaultContinuationTTL),
		interleavedCount: 0,
	}

	// Clear removes all active sessions (used during channel shutdown).
	if len(t.sessions) > 2048 {
		for id, sess := range t.sessions {
			if sess == nil || now.After(sess.expiresAt) {
				delete(t.sessions, id)
			}
		}
	}
}

// Clear removes all active continuation sessions (e.g. during channel shutdown).
func (t *ContinuationTracker) Clear() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sessions = nil
}

// Evaluate determines whether an unmentioned group message continues an active
// conversation with the bot.
func (t *ContinuationTracker) Evaluate(in ContinuationInput) ContinuationDecision {
	if t == nil || in.GroupID == "" || in.SpeakerID == "" {
		return ContinuationNone
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.sessions == nil {
		return ContinuationNone
	}
	session, exists := t.sessions[in.GroupID]
	if !exists || session == nil {
		return ContinuationNone
	}

	if now.After(session.expiresAt) {
		delete(t.sessions, in.GroupID)
		return ContinuationNone
	}

	// Another member spoke while the session was bound to a specific speaker.
	if in.SpeakerID != session.speakerID && session.speakerID != "" {
		session.interleavedCount++
		if session.interleavedCount > MaxInterleavedMessages {
			delete(t.sessions, in.GroupID)
		}
		return ContinuationNone
	}

	// Open session locks onto the first member who replies.
	if session.speakerID == "" {
		session.speakerID = in.SpeakerID
	}

	trimmed := strings.TrimSpace(in.Content)

	// Hard protocol exclusions: speaking to another member or invoking a command.
	if in.MentionedOther || in.QuotedOther {
		delete(t.sessions, in.GroupID)
		return ContinuationNone
	}
	if trimmed == "" || isPurePunctuationNoise(trimmed) {
		return ContinuationNone
	}
	if strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ".") {
		delete(t.sessions, in.GroupID)
		return ContinuationNone
	}

	// Explicit quote of the bot resets the session and passes directly.
	if in.QuotedBot {
		session.remainingTurns = DefaultContinuationTurns
		session.expiresAt = now.Add(DefaultContinuationTTL)
		session.interleavedCount = 0
		return ContinuationDirect
	}

	// Explicit bot name mention passes directly.
	if in.NamedBot {
		consumeTurn(t.sessions, in.GroupID, session, now)
		session.interleavedCount = 0
		return ContinuationDirect
	}

	// Clean stream: same speaker within TTL with zero interleaved messages from others.
	if session.interleavedCount == 0 {
		consumeTurn(t.sessions, in.GroupID, session, now)
		return ContinuationDirect
	}

	// Interleaved stream (1..MaxInterleavedMessages): genuinely ambiguous, let Cortex verify.
	consumeTurn(t.sessions, in.GroupID, session, now)
	session.interleavedCount = 0
	return ContinuationAmbiguous
}

func consumeTurn(sessions map[string]*activeSession, groupID string, session *activeSession, now time.Time) {
	session.remainingTurns--
	session.expiresAt = now.Add(DefaultContinuationTTL)
	if session.remainingTurns <= 0 {
		delete(sessions, groupID)
	}
}

func isPurePunctuationNoise(s string) bool {
	if strings.ContainsAny(s, "?？") {
		return false
	}
	for _, r := range s {
		if !unicode.IsPunct(r) && !unicode.IsSymbol(r) && !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
