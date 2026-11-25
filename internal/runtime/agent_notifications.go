package runtime

import (
	"context"
	"daidai/internal/entity"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
)

func (a *SuperAgent) notifyTicketCompletion(ctx context.Context, ticket *entity.Ticket, output string, execErr error) {
	if ticket == nil || ctx == nil || ctx.Err() != nil || a.Channels == nil || errors.Is(execErr, context.Canceled) || ticket.Status == entity.TicketAborted || ticket.Status == entity.TicketStopping {
		return
	}
	ackTimer := time.NewTimer(time.Second)
	defer ackTimer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-ackTimer.C:
	}
	state := "✅ 这件事处理好了"
	switch ticket.Status {
	case entity.TicketFailed:
		state = "⚠️ 这件事没能完整处理完"
	case entity.TicketAborted, entity.TicketStopping:
		state = "⏹️ 这件事已停下"
	case entity.TicketInterrupted:
		state = "⚠️ 这件事因服务重启中断了"
	}
	if execErr != nil && ticket.Status != entity.TicketAborted && ticket.Status != entity.TicketStopping {
		state = "⚠️ 这件事没能完整处理完"
	}
	result := strings.TrimSpace(ticket.Summary)
	if result == "" || ticket.Status != entity.TicketCompleted {
		result = strings.TrimSpace(ticket.Output)
	}
	if result == "" {
		result = strings.TrimSpace(output)
	}
	var message strings.Builder
	fmt.Fprintf(&message, "%s：%s\n编号：`%s`\n", state, core.TruncateRunes(strings.TrimSpace(ticket.Goal), 180, "…"), ticket.ID)
	if ticket.Model != "" {
		fmt.Fprintf(&message, "模型：`%s`\n", ticket.Model)
	}
	if result != "" {
		fmt.Fprintf(&message, "\n%s\n", ticketResultPreview(result, 1200))
	}
	if ticket.Status != entity.TicketCompleted {
		message.WriteString("\n目前保留的阶段结果可用 `/task ")
		message.WriteString(ticket.ID)
		message.WriteString("` 查看；不会自动重跑。需要继续时，告诉我从哪个阶段接着做。")
	} else {
		message.WriteString("\n完整执行日志可用 `/task ")
		message.WriteString(ticket.ID)
		message.WriteString("` 查看。")
	}
	body := strings.TrimSpace(message.String())
	if !ticket.AllowSensitive {
		body = core.RedactSensitiveIPs(body)
	}
	utterance := &entity.Utterance{
		Platform:  ticket.Platform,
		IsGroup:   ticket.IsGroup,
		ChannelID: ticket.ChannelID,
		Text:      body,
		Proactive: true,
	}
	if ticket.IsGroup && ticket.RequesterID != "" {
		utterance.MentionUserIDs = []string{ticket.RequesterID}
	}
	role := entity.RoleGuest
	if ticket.AllowSensitive {
		role = entity.RolePartner
	}
	cardCtx, cancelCard := context.WithTimeout(ctx, 20*time.Second)
	a.renderAutomaticReplyCard(cardCtx, &entity.SensoryEvent{
		Platform:  ticket.Platform,
		IsGroup:   ticket.IsGroup,
		ChannelID: ticket.ChannelID,
		SenderID:  ticket.RequesterID,
		Role:      role,
	}, utterance)
	cancelCard()
	notificationKey := "agent-ticket:" + ticket.ID
	queued := false
	if a.database != nil && ticket.RequesterID != "" && ticket.Platform != "" {
		unlock := a.lockPendingNotification(ticket.Platform, ticket.RequesterID)
		defer unlock()
		if ctx.Err() != nil {
			return
		}
		queueCtx, cancelQueue := context.WithTimeout(context.Background(), 3*time.Second)
		inserted, queueErr := a.database.EnqueuePendingNotification(queueCtx, ticket.Platform, ticket.RequesterID,
			notificationKey, "agent_ticket", body)
		cancelQueue()
		if queueErr != nil {
			slog.Warn("superagent: 工单通知未能加入延迟队列", "ticketID", ticket.ID, "err", queueErr)
		} else if !inserted {
			return
		} else {
			queued = true
		}
	}
	if queued {
		claimCtx, cancelClaim := context.WithTimeout(context.Background(), 3*time.Second)
		claimErr := a.database.ClaimPendingNotificationByKey(claimCtx, ticket.Platform, ticket.RequesterID, notificationKey)
		cancelClaim()
		if claimErr != nil {
			slog.Warn("superagent: 无法确认工单通知发送状态，跳过主动发送", "ticketID", ticket.ID, "err", claimErr)
			return
		}
	}
	sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	err := a.Channels.Transmit(sendCtx, utterance)
	cancel()
	if err == nil {
		a.commitProactiveBotReply(ticket.Platform, ticket.IsGroup, ticket.ChannelID, ticket.RequesterID, body)
		if queued {
			deleteCtx, cancelDelete := context.WithTimeout(context.Background(), 3*time.Second)
			if deleteErr := a.database.DeletePendingNotificationByKey(deleteCtx, ticket.Platform, ticket.RequesterID, notificationKey); deleteErr != nil {
				slog.Warn("superagent: 已发送工单通知但未能清除待发送副本", "ticketID", ticket.ID, "err", deleteErr)
			}
			cancelDelete()
		}
		return
	}
	var partial *entity.PartialDeliveryError
	if errors.As(err, &partial) {
		if strings.TrimSpace(partial.DeliveredText) != "" {
			a.commitProactiveBotReply(ticket.Platform, ticket.IsGroup, ticket.ChannelID, ticket.RequesterID, partial.DeliveredText)
		}
		if queued {
			persistCtx, cancelPersist := context.WithTimeout(context.Background(), 3*time.Second)
			remaining := strings.TrimSpace(partial.UndeliveredText)
			var persistErr error
			if remaining == "" {
				persistErr = a.database.DeletePendingNotificationByKey(persistCtx, ticket.Platform, ticket.RequesterID, notificationKey)
			} else {
				persistErr = a.database.UpdatePendingNotificationBodyByKey(persistCtx, ticket.Platform, ticket.RequesterID, notificationKey, remaining)
			}
			if persistErr != nil {
				slog.Error("superagent: 工单通知部分送达后无法保存剩余正文", "ticketID", ticket.ID, "err", persistErr)
				if stateErr := a.database.SetPendingNotificationStateByKey(persistCtx, ticket.Platform, ticket.RequesterID, notificationKey, db.PendingNotificationStateUncertain); stateErr != nil {
					slog.Error("superagent: 无法将剩余正文未保存的工单通知标记为结果不确定", "ticketID", ticket.ID, "err", stateErr)
				}
			}
			cancelPersist()
		}
		slog.Warn("superagent: 工单通知部分送达，队列仅保留未送达正文", "ticketID", ticket.ID, "err", err)
		return
	}
	slog.Warn("superagent: 工单完成通知主动发送失败", "ticketID", ticket.ID, "platform", ticket.Platform, "channel", ticket.ChannelID, "err", err)
	if queued {
		state := db.PendingNotificationStateUncertain
		if entity.NotificationDeliveryCanRetry(err) {
			state = db.PendingNotificationStatePending
		}
		stateCtx, cancelState := context.WithTimeout(context.Background(), 3*time.Second)
		if stateErr := a.database.SetPendingNotificationStateByKey(stateCtx, ticket.Platform, ticket.RequesterID, notificationKey, state); stateErr != nil {
			slog.Error("superagent: 工单通知失败后无法更新发送状态", "ticketID", ticket.ID, "state", state, "err", stateErr)
		}
		cancelState()
	} else {
		slog.Warn("superagent: 工单完成通知没有持久化的延迟副本", "ticketID", ticket.ID)
	}
}

func ticketResultPreview(result string, limit int) string {
	runes := []rune(result)
	if limit <= 0 {
		return ""
	}
	if len(runes) <= limit {
		return result
	}
	preview := string(runes[:limit])
	parts := strings.Split(preview, "```")
	if len(parts)%2 == 0 {
		preview = strings.TrimRight(preview, "\n") + "\n```"
	} else {
		inlineTicks := 0
		for i := 0; i < len(parts); i += 2 {
			inlineTicks += strings.Count(parts[i], "`")
		}
		if inlineTicks%2 != 0 {
			preview += "`"
		}
	}
	return preview + "\n[后续结果已保存]"
}

func (a *SuperAgent) commitProactiveBotReply(platform string, isGroup bool, channelID, senderID, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	a.commitCognitionReply(ctx, &entity.SensoryEvent{
		Platform:   platform,
		IsGroup:    isGroup,
		ChannelID:  channelID,
		SenderID:   senderID,
		OccurredAt: time.Now(),
	}, text)
}

func (a *SuperAgent) deliverPendingNotifications(ctx context.Context, event *entity.SensoryEvent) {
	if event == nil || event.IsGroup || event.RawMessageID == "" || (event.Role != entity.RolePartner && event.Role != entity.RoleCollaborator) {
		return
	}
	if event.IsPartnerPrivate() && a.OnPrivateInteraction != nil {
		a.OnPrivateInteraction(ctx, event)
	}
	if a.database == nil {
		return
	}
	unlock := a.lockPendingNotification(event.Platform, event.SenderID)
	defer unlock()
	loadCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	notifications, err := a.database.ListPendingNotifications(loadCtx, event.Platform, event.SenderID, 10)
	cancel()
	if err != nil {
		slog.Warn("superagent: 读取待发送通知失败", "err", err)
		return
	}
	if len(notifications) == 0 {
		return
	}
	var notice db.PendingNotification
	for _, candidate := range notifications {
		if event.IsPartnerPrivate() || candidate.Category == "agent_ticket" {
			if candidate.State == db.PendingNotificationStateUncertain {
				notice = candidate
				break
			}
			if candidate.State != db.PendingNotificationStatePending {
				continue
			}
			notice = candidate
			break
		}
	}
	if notice.ID == 0 {
		return
	}
	if notice.State == db.PendingNotificationStateUncertain {
		body := uncertainNotificationNotice(notice)
		sendCtx, cancelSend := context.WithTimeout(ctx, 15*time.Second)
		err := a.Channels.Transmit(sendCtx, &entity.Utterance{
			Platform: event.Platform, ChannelID: event.ChannelID, QuoteMsgID: event.RawMessageID, Text: body,
		})
		cancelSend()
		if err != nil {
			slog.Warn("superagent: 通知结果不确定的提示未能送达", "notificationID", notice.ID, "err", err)
			return
		}
		stateCtx, cancelState := context.WithTimeout(context.Background(), 3*time.Second)
		if err := a.database.SetPendingNotificationState(stateCtx, event.Platform, event.SenderID, notice.ID, db.PendingNotificationStateUncertainNotified); err != nil {
			slog.Warn("superagent: 无法记录已提示的不确定通知", "notificationID", notice.ID, "err", err)
		}
		cancelState()
		return
	}
	claimCtx, cancelClaim := context.WithTimeout(ctx, 3*time.Second)
	claimErr := a.database.ClaimPendingNotification(claimCtx, event.Platform, event.SenderID, notice.ID)
	cancelClaim()
	if claimErr != nil {
		slog.Warn("superagent: 无法确认待发送通知状态，跳过本次发送", "notificationID", notice.ID, "err", claimErr)
		return
	}
	caption := "📨 延迟送达的通知"
	switch notice.Category {
	case "oci_daily":
		caption = "📊 延迟送达的 OCI 账单日报"
	case "oci_alert":
		caption = "⚠️ 延迟送达的 OCI 资源告警"
	case "agent_ticket":
		caption = "🛠️ 延迟送达的工单通知"
	}
	sendCtx, cancelSend := context.WithTimeout(ctx, 25*time.Second)
	complete, remainingBody, err := a.transmitPendingNotification(sendCtx, event, caption, notice.Body)
	cancelSend()
	if err != nil {
		slog.Warn("superagent: 待发送通知仍未送达", "notificationID", notice.ID, "category", notice.Category, "err", err)
		state := db.PendingNotificationStateUncertain
		if entity.NotificationDeliveryCanRetry(err) {
			state = db.PendingNotificationStatePending
		}
		stateCtx, cancelState := context.WithTimeout(context.Background(), 3*time.Second)
		if stateErr := a.database.SetPendingNotificationState(stateCtx, event.Platform, event.SenderID, notice.ID, state); stateErr != nil {
			slog.Error("superagent: 待发送通知失败后无法更新发送状态", "notificationID", notice.ID, "state", state, "err", stateErr)
		}
		cancelState()
		return
	}
	deleteCtx, cancelDelete := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelDelete()
	if !complete {
		if err := a.database.UpdatePendingNotificationBody(deleteCtx, event.Platform, event.SenderID, notice.ID, remainingBody); err != nil {
			slog.Error("superagent: 通知分段送达但未能保存剩余内容", "notificationID", notice.ID, "err", err)
			if stateErr := a.database.SetPendingNotificationState(deleteCtx, event.Platform, event.SenderID, notice.ID, db.PendingNotificationStateUncertain); stateErr != nil {
				slog.Error("superagent: 无法将剩余正文未保存的通知标记为结果不确定", "notificationID", notice.ID, "err", stateErr)
			}
		} else {
			slog.Info("superagent: 通知已分段送达，剩余内容保留待下次私聊", "notificationID", notice.ID, "remainingBytes", len(remainingBody))
		}
		return
	}
	if notice.Category == "agent_ticket" {
		a.commitProactiveBotReply(event.Platform, false, event.ChannelID, event.SenderID, notice.Body)
	}
	if err := a.database.DeletePendingNotification(deleteCtx, event.Platform, event.SenderID, notice.ID); err != nil {
		slog.Error("superagent: 通知已发送但无法从待发队列删除", "notificationID", notice.ID, "err", err)
	}
}

func uncertainNotificationNotice(notification db.PendingNotification) string {
	switch notification.Category {
	case "agent_ticket":
		id, _ := strings.CutPrefix(notification.DedupeKey, "agent-ticket:")
		if id != "" {
			return "之前的工单通知发送结果不确定，为避免重复我没有自动重发；完整结果可以用 `/task " + id + "` 查看。"
		}
		return "之前的工单通知发送结果不确定，为避免重复我没有自动重发；可以用 `/tasks` 查看工单。"
	case "oci_daily":
		return "之前的 OCI 日报发送结果不确定，为避免重复我没有自动重发；需要的话可以让我重新查询。"
	case "oci_alert":
		return "之前的 OCI 告警发送结果不确定，为避免重复我没有自动重发；需要的话可以让我重新检查资源。"
	case "private_access_request":
		return "之前的私聊权限申请提醒发送结果不确定，为避免重复我没有自动重发；可以问我查看待处理申请。"
	default:
		return "之前有一条通知发送结果不确定，为避免重复我没有自动重发；需要的话请告诉我重新查询相关状态。"
	}
}
