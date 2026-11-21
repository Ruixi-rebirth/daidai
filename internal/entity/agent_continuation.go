package entity

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func continuationKey(senderID string, utterance *Utterance) thinkSessionKey {
	return thinkSessionKey{
		session:   fmt.Sprintf("%s:%v:%s", utterance.Platform, utterance.IsGroup, utterance.ChannelID),
		requester: senderID,
	}
}

func nonEmptyStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func (a *SuperAgent) setReplyContinuation(ctx context.Context, senderID string, utterance *Utterance, text string) {
	if senderID == "" || utterance == nil || strings.TrimSpace(text) == "" {
		return
	}
	key := replyContinuationKey{continuationKey(senderID, utterance), utterance.DraftID}
	a.continuationMu.Lock()
	defer a.continuationMu.Unlock()
	if ctx != nil && ctx.Err() != nil {
		return
	}
	var callbacks []func(context.Context)
	if utterance.OnDelivered != nil {
		callbacks = append(callbacks, utterance.OnDelivered)
	}
	a.storeReplyContinuationLocked(key, text, callbacks, utterance.continuationQueuedAt, time.Now())
}

func (a *SuperAgent) storeReplyContinuationLocked(key replyContinuationKey, text string, callbacks []func(context.Context), queuedAt, now time.Time) {
	if a.continuations == nil {
		a.continuations = make(map[replyContinuationKey]pendingReplyContinuation)
	}
	for existingKey, continuation := range a.continuations {
		if now.Sub(continuation.updatedAt) > 24*time.Hour {
			delete(a.continuations, existingKey)
		}
	}
	if _, exists := a.continuations[key]; !exists && len(a.continuations) >= 128 {
		var oldestKey replyContinuationKey
		var oldest time.Time
		for existingKey, continuation := range a.continuations {
			if oldest.IsZero() || continuation.updatedAt.Before(oldest) {
				oldestKey, oldest = existingKey, continuation.updatedAt
			}
		}
		delete(a.continuations, oldestKey)
	}
	continuation := a.continuations[key]
	if continuation.queuedAt.IsZero() {
		if queuedAt.IsZero() {
			queuedAt = now
		}
		continuation.queuedAt = queuedAt
	}
	continuation.draftID = key.draftID
	if continuation.text != "" {
		continuation.text += "\n\n"
	}
	continuation.text += text
	continuation.onDelivered = append(continuation.onDelivered, callbacks...)
	continuation.updatedAt = now
	a.continuations[key] = continuation
}

func (a *SuperAgent) takeReplyContinuation(event *SensoryEvent, draftID string) (pendingReplyContinuation, bool) {
	if event == nil || event.SenderID == "" {
		return pendingReplyContinuation{}, false
	}
	a.continuationMu.Lock()
	defer a.continuationMu.Unlock()
	key := replyContinuationKey{thinkKey(event), draftID}
	if draftID == "" {
		var oldest time.Time
		for candidate, pending := range a.continuations {
			if candidate.thinkSessionKey == key.thinkSessionKey && time.Since(pending.updatedAt) <= 24*time.Hour &&
				(oldest.IsZero() || pending.queuedAt.Before(oldest)) {
				key, oldest = candidate, pending.queuedAt
			}
		}
	}
	continuation, ok := a.continuations[key]
	if !ok {
		return pendingReplyContinuation{}, false
	}
	if time.Since(continuation.updatedAt) > 24*time.Hour {
		delete(a.continuations, key)
		return pendingReplyContinuation{}, false
	}
	delete(a.continuations, key)
	return continuation, true
}

func (a *SuperAgent) restoreReplyContinuation(ctx context.Context, event *SensoryEvent, pending pendingReplyContinuation) {
	if event == nil || event.SenderID == "" || strings.TrimSpace(pending.text) == "" {
		return
	}
	key := replyContinuationKey{thinkKey(event), pending.draftID}
	a.continuationMu.Lock()
	defer a.continuationMu.Unlock()
	if ctx != nil && ctx.Err() != nil {
		return
	}
	now := time.Now()
	if a.continuations == nil {
		a.continuations = make(map[replyContinuationKey]pendingReplyContinuation)
	}
	continuation := a.continuations[key]
	if continuation.text != "" {
		pending.text += "\n\n" + continuation.text
	}
	pending.onDelivered = append(pending.onDelivered, continuation.onDelivered...)
	if pending.queuedAt.IsZero() {
		pending.queuedAt = now
	}
	pending.updatedAt = now
	a.continuations[key] = pending
}

func (a *SuperAgent) clearReplyContinuation(event *SensoryEvent) {
	if event == nil || event.SenderID == "" {
		return
	}
	a.continuationMu.Lock()
	session := thinkKey(event)
	for key := range a.continuations {
		if key.thinkSessionKey == session {
			delete(a.continuations, key)
		}
	}
	a.continuationMu.Unlock()
}

func (a *SuperAgent) clearSessionReplyContinuations(event *SensoryEvent) {
	session := sessionKey(event)
	a.continuationMu.Lock()
	defer a.continuationMu.Unlock()
	for key := range a.continuations {
		if key.session == session {
			delete(a.continuations, key)
		}
	}
}
