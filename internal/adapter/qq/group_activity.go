package qq

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/kernel"
)

func (q *QQChannel) shouldConsiderGroupInterjection(groupID string, generation uint64) bool {
	if q == nil || strings.TrimSpace(groupID) == "" {
		return false
	}
	now := time.Now()
	q.groupInterjectionsMu.Lock()
	defer q.groupInterjectionsMu.Unlock()
	if q.lifecycleGeneration.Load() != generation || !q.wsReady.Load() {
		return false
	}
	if q.groupInterjections == nil {
		q.groupInterjections = make(map[string]time.Time)
	}
	if last, exists := q.groupInterjections[groupID]; exists && now.Sub(last) < groupInterjectionCooldown {
		return false
	}
	q.groupInterjections[groupID] = now
	if now.Sub(q.groupInterjectionSweepAt) >= 10*time.Minute || len(q.groupInterjections) > 4096 {
		for id, last := range q.groupInterjections {
			if now.Sub(last) >= groupInterjectionCooldown {
				delete(q.groupInterjections, id)
			}
		}
		q.groupInterjectionSweepAt = now
	}
	if len(q.groupInterjections) > 4096 {
		oldestID := ""
		var oldestAt time.Time
		for id, at := range q.groupInterjections {
			if oldestID == "" || at.Before(oldestAt) {
				oldestID, oldestAt = id, at
			}
		}
		delete(q.groupInterjections, oldestID)
	}
	return true
}

func (q *QQChannel) scheduleGroupInterjection(groupID string, event *entity.SensoryEvent) {
	if q == nil || event == nil || strings.TrimSpace(groupID) == "" {
		return
	}
	manager := q.debounce
	if manager == nil {
		return
	}
	generation := q.lifecycleGeneration.Load()
	q.pendingInterjectionsMu.Lock()
	defer q.pendingInterjectionsMu.Unlock()
	if q.pendingInterjections == nil {
		q.pendingInterjections = make(map[string]*pendingGroupInterjection)
	}
	if pending := q.pendingInterjections[groupID]; pending != nil {
		pending.timer.Stop()
		pending = &pendingGroupInterjection{event: event, scheduled: time.Now()}
		q.pendingInterjections[groupID] = pending
		pending.timer = time.AfterFunc(groupInterjectionQuietPeriod, func() {
			q.dispatchPendingGroupInterjection(groupID, pending, manager, generation)
		})
		return
	}
	pending := &pendingGroupInterjection{event: event, scheduled: time.Now()}
	pending.timer = time.AfterFunc(groupInterjectionQuietPeriod, func() {
		q.dispatchPendingGroupInterjection(groupID, pending, manager, generation)
	})
	q.pendingInterjections[groupID] = pending
	if len(q.pendingInterjections) > 4096 {
		oldestID := ""
		var oldestAt time.Time
		for id, entry := range q.pendingInterjections {
			if oldestID == "" || entry.scheduled.Before(oldestAt) {
				oldestID, oldestAt = id, entry.scheduled
			}
		}
		if oldest := q.pendingInterjections[oldestID]; oldest != nil {
			oldest.timer.Stop()
			delete(q.pendingInterjections, oldestID)
		}
	}
}

func (q *QQChannel) dispatchPendingGroupInterjection(groupID string, pending *pendingGroupInterjection, manager *kernel.DebounceManager, generation uint64) {
	q.pendingInterjectionsMu.Lock()
	if q.pendingInterjections[groupID] != pending {
		q.pendingInterjectionsMu.Unlock()
		return
	}
	delete(q.pendingInterjections, groupID)
	q.pendingInterjectionsMu.Unlock()

	if manager == nil || !q.shouldConsiderGroupInterjection(groupID, generation) {
		return
	}
	if q.lifecycleGeneration.Load() != generation || !q.wsReady.Load() {
		return
	}
	event := pending.event
	event.GroupInterjection = true
	event.GroupParticipants = q.recentGroupParticipants(groupID)
	if !manager.Push(event) {
		slog.Warn("qq_channel: 群聊插话候选未能进入处理队列", "channelID", groupID, "msgID", event.RawMessageID)
	}
}

func (q *QQChannel) cancelPendingGroupInterjection(groupID string) {
	q.pendingInterjectionsMu.Lock()
	if pending := q.pendingInterjections[groupID]; pending != nil {
		pending.timer.Stop()
		delete(q.pendingInterjections, groupID)
	}
	q.pendingInterjectionsMu.Unlock()
}

func (q *QQChannel) ingestUnaddressedGroupMessage(event *entity.SensoryEvent, canInterject bool) bool {
	if event == nil {
		return false
	}
	if event.Content == "" && event.HistoryAttachmentNote == "" {
		if q.dedup != nil {
			q.dedup.Remember(event.RawMessageID)
		}
		return true
	}
	accepted := q.IngestRaw(event)
	if accepted && q.dedup != nil {
		q.dedup.Remember(event.RawMessageID)
	}
	if accepted && canInterject {
		candidate := &entity.SensoryEvent{
			Platform:             event.Platform,
			ChannelID:            event.ChannelID,
			IsGroup:              event.IsGroup,
			SenderID:             event.SenderID,
			SenderName:           event.SenderName,
			Role:                 event.Role,
			Content:              event.Content,
			ReplyContext:         event.ReplyContext,
			RawMessageID:         event.RawMessageID,
			HistoryRecordIDs:     append([]int64(nil), event.HistoryRecordIDs...),
			OccurredAt:           event.OccurredAt,
			GroupMessageObserved: true,
		}
		q.scheduleGroupInterjection(event.ChannelID, candidate)
	} else {
		q.cancelPendingGroupInterjection(event.ChannelID)
	}
	return accepted
}

func (q *QQChannel) recentGroupParticipants(groupID string) []core.GroupParticipant {
	if q.database == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	recent, err := q.database.RecentMessages(ctx, "qq", "group", groupID, 30)
	if err != nil {
		slog.Warn("qq_channel: 读取插话点名候选失败", "channelID", groupID, "err", err)
		return nil
	}
	seen := make(map[string]struct{}, len(recent))
	participants := make([]core.GroupParticipant, 0, len(recent))
	for _, message := range recent {
		if message.IsBot || message.UserID == "" || strings.TrimSpace(message.Name) == "" {
			continue
		}
		if _, exists := seen[message.UserID]; exists {
			continue
		}
		seen[message.UserID] = struct{}{}
		participants = append(participants, core.GroupParticipant{ID: message.UserID, Name: message.Name})
	}
	return participants
}
