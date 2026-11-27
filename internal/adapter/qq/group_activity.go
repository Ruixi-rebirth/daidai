package qq

import (
	"context"
	"log/slog"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/groupchat"
)

func (q *QQChannel) interjectionScheduler() *groupchat.InterjectionScheduler {
	q.interjectionsOnce.Do(func() {
		if q.interjections == nil {
			q.interjections = groupchat.NewInterjectionScheduler(groupchat.InterjectionConfig{})
		}
	})
	return q.interjections
}

func (q *QQChannel) cancelPendingGroupInterjection(groupID string) {
	if q == nil || q.interjections == nil {
		return
	}
	q.interjections.Cancel(groupID)
}

func (q *QQChannel) notifyGroupAddressed(groupID, senderID, senderName string) {
	if q == nil {
		return
	}
	q.interjectionScheduler().NotifyAddressed(groupID, senderID, senderName)
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
	accepted := q.IngestRaw(event)
	if accepted && q.dedup != nil {
		q.dedup.Remember(candidate.RawMessageID)
	}
	if !accepted {
		return false
	}
	manager := q.debounce
	generation := q.lifecycleGeneration.Load()
	q.interjectionScheduler().Observe(candidate.ChannelID, candidate, canInterject && manager != nil, func(ev *entity.SensoryEvent) {
		if q.lifecycleGeneration.Load() != generation || !q.wsReady.Load() {
			return
		}
		if dbParticipants := q.recentGroupParticipants(ev.ChannelID); len(dbParticipants) > 0 {
			ev.GroupParticipants = dbParticipants
		}
		if !manager.Push(ev) {
			slog.Warn("qq_channel: 群聊插话候选未能进入处理队列", "channelID", ev.ChannelID, "msgID", ev.RawMessageID)
		}
	})
	return true
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
		if message.IsBot || message.UserID == "" {
			continue
		}
		name := core.NormalizeDisplayName(message.Name, message.UserID)
		if name == "" {
			continue
		}
		if _, exists := seen[message.UserID]; exists {
			continue
		}
		seen[message.UserID] = struct{}{}
		participants = append(participants, core.GroupParticipant{ID: message.UserID, Name: name})
	}
	return participants
}
