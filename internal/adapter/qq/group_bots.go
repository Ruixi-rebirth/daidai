package qq

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/runtime"
)

func (q *QQChannel) handleGroupBotRegistryCommand(groupID, messageID, senderID string, role entity.UserRole, content string, eventData map[string]any) bool {
	trimmed := strings.TrimSpace(content)
	if !runtime.IsAdapterSlashCommand("qq", trimmed) {
		return false
	}
	q.cancelPendingGroupInterjection(groupID)
	marked, matched := groupBotCommand(trimmed)
	if !matched {
		q.replyGroupMessage(context.Background(), groupID, messageID, "群机器人管理指令用法：\n- /bot mark @成员 ：标记为机器人（不触发回复）\n- /bot unmark @成员 ：取消机器人标记")
		if q.dedup != nil {
			q.dedup.Remember(messageID)
		}
		return true
	}
	manageCtx, manageCancel := context.WithTimeout(context.Background(), 10*time.Second)
	q.handleGroupBotRegistryRequest(manageCtx, groupID, messageID, senderID, role, marked, content, eventData)
	manageCancel()
	if q.dedup != nil {
		q.dedup.Remember(messageID)
	}
	return true
}

func (q *QQChannel) loadKnownGroupBots(ctx context.Context) error {
	known := make(map[string]map[string]string)
	if q.database != nil {
		value, err := q.database.GetRuntimeState(ctx, "qq", "known_group_bots")
		if err != nil {
			return err
		}
		if strings.TrimSpace(value) != "" {
			if err := json.Unmarshal([]byte(value), &known); err != nil {
				return fmt.Errorf("decode known group bots: %w", err)
			}
		}
	}
	q.knownGroupBotsMu.Lock()
	q.knownGroupBots = known
	q.knownGroupBotsMu.Unlock()
	return nil
}

func (q *QQChannel) isKnownGroupBot(groupID, memberID string) bool {
	q.knownGroupBotsMu.RLock()
	defer q.knownGroupBotsMu.RUnlock()
	return q.knownGroupBots[groupID][memberID] != ""
}

func (q *QQChannel) setKnownGroupBot(ctx context.Context, groupID string, bot groupBotMention, marked bool) error {
	q.knownGroupBotsMu.Lock()
	defer q.knownGroupBotsMu.Unlock()
	known := make(map[string]map[string]string)
	if q.database != nil {
		value, err := q.database.GetRuntimeState(ctx, "qq", "known_group_bots")
		if err != nil {
			return err
		}
		if strings.TrimSpace(value) != "" {
			if err := json.Unmarshal([]byte(value), &known); err != nil {
				return fmt.Errorf("decode known group bots: %w", err)
			}
		}
	} else {
		for gid, members := range q.knownGroupBots {
			copied := make(map[string]string, len(members))
			for mid, name := range members {
				copied[mid] = name
			}
			known[gid] = copied
		}
	}
	if known[groupID] == nil {
		known[groupID] = make(map[string]string)
	}
	if marked {
		if _, exists := known[groupID][bot.ID]; !exists && len(known[groupID]) >= 100 {
			return fmt.Errorf("群机器人标记已达到每群 100 个的上限")
		}
		known[groupID][bot.ID] = bot.Name
	} else {
		delete(known[groupID], bot.ID)
		if len(known[groupID]) == 0 {
			delete(known, groupID)
		}
	}
	if q.database != nil {
		updated, err := json.Marshal(known)
		if err != nil {
			return err
		}
		if err := q.database.SetRuntimeState(ctx, "qq", "known_group_bots", string(updated)); err != nil {
			return err
		}
	}
	q.knownGroupBots = known
	return nil
}

func groupBotCommand(content string) (marked, matched bool) {
	fields := strings.Fields(content)
	if len(fields) < 2 || fields[0] != "/bot" {
		return false, false
	}
	switch fields[1] {
	case "mark":
		return true, true
	case "unmark":
		return false, true
	default:
		return false, false
	}
}

func qqMentionedGroupMembers(eventData map[string]any) []groupBotMention {
	mentions, ok := eventData["mentions"].([]any)
	if !ok {
		return nil
	}
	seen := make(map[string]struct{}, len(mentions))
	var members []groupBotMention
	for _, rawMention := range mentions {
		mention, ok := rawMention.(map[string]any)
		if !ok {
			continue
		}
		if isYou, _ := mention["is_you"].(bool); isYou {
			continue
		}
		memberID, _ := mention["member_openid"].(string)
		if memberID == "" {
			memberID, _ = mention["id"].(string)
		}
		memberID = strings.TrimSpace(memberID)
		if memberID == "" {
			continue
		}
		if _, exists := seen[memberID]; exists {
			continue
		}
		seen[memberID] = struct{}{}
		name, _ := mention["username"].(string)
		members = append(members, groupBotMention{ID: memberID, Name: core.NormalizeDisplayName(name, memberID)})
	}
	return members
}

func (q *QQChannel) handleGroupBotRegistryRequest(ctx context.Context, groupID, messageID, senderID string, role entity.UserRole, marked bool, content string, eventData map[string]any) {
	if role != entity.RolePartner {
		q.replyGroupMessage(ctx, groupID, messageID, "只有核心搭档可以管理群聊中的机器人标记。")
		return
	}
	members := qqMentionedGroupMembers(eventData)
	if len(members) == 0 {
		q.replyGroupMessage(ctx, groupID, messageID, "未检测到被操作的群成员。请在使用指令的同时 @ 目标成员，例如：/bot mark @目标 或 /bot unmark @目标")
		return
	}
	if q.historyRecorder != nil {
		recordCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := q.historyRecorder(recordCtx, &entity.SensoryEvent{
			Platform: "qq", ChannelID: groupID, IsGroup: true, SenderID: senderID,
			Content: content, RawMessageID: messageID, OccurredAt: time.Now(), Role: role,
		})
		cancel()
		if err != nil {
			slog.Warn("qq_channel: 保存群机器人标记操作失败", "channelID", groupID, "msgID", messageID, "err", err)
		}
	}
	var updated []string
	for _, member := range members {
		if err := q.setKnownGroupBot(ctx, groupID, member, marked); err != nil {
			slog.Warn("qq_channel: 保存群机器人标记失败", "channelID", groupID, "memberID", member.ID, "err", err)
			continue
		}
		name := member.Name
		if name == "" {
			name = "该成员"
		}
		updated = append(updated, name)
	}
	if len(updated) == 0 {
		q.replyGroupMessage(ctx, groupID, messageID, "机器人标记未能保存，请稍后重试。")
		return
	}
	action := "已标记为机器人，后续群消息不会触发呆呆回复。"
	if !marked {
		action = "已取消机器人标记，后续群消息会按普通成员消息处理。"
	}
	q.replyGroupMessage(ctx, groupID, messageID, strings.Join(updated, "、")+action)
}
