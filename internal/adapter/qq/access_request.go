package qq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/engine/render"
	"daidai/internal/entity"
)

func (q *QQChannel) replyGroupMessage(ctx context.Context, groupID, messageID, text string) {
	replyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := q.Transmit(replyCtx, &entity.Utterance{
		Platform: "qq", IsGroup: true, ChannelID: groupID, QuoteMsgID: messageID, Text: text,
	}); err != nil {
		slog.Warn("qq_channel: 群机器人标记操作的确认消息发送失败", "channelID", groupID, "err", err)
	}
}

func (q *QQChannel) replyPrivateMessage(userID, messageID, text string) {
	replyCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := q.Transmit(replyCtx, &entity.Utterance{
		Platform: "qq", ChannelID: userID, QuoteMsgID: messageID, Text: text,
	}); err != nil {
		slog.Warn("qq_channel: 访客私聊权限提示发送失败", "channelID", userID, "err", err)
	}
}

func singleLineText(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func (q *QQChannel) notifyOwnerOfPrivateAccessRequest(userID, displayName, message string) {
	ownerID := strings.TrimSpace(q.cfg.OwnerOpenID)
	if ownerID == "" || ownerID == strings.TrimSpace(userID) {
		return
	}

	var pendingRequests []db.PrivateAccessRequest
	if q.accessRequests != nil {
		reqCtx, reqCancel := context.WithTimeout(context.Background(), 3*time.Second)
		reqs, err := q.accessRequests.ListPendingPrivateAccessRequests(reqCtx, "qq", 10)
		reqCancel()
		if err != nil {
			slog.Warn("qq_channel: 查询待处理私聊权限申请失败", "err", err)
		} else {
			pendingRequests = reqs
		}
	}

	// 确保当前申请人在列表中
	foundCurrent := false
	for _, req := range pendingRequests {
		if req.UserID == userID {
			foundCurrent = true
			break
		}
	}
	if !foundCurrent {
		currentReq := db.PrivateAccessRequest{
			UserID:       userID,
			DisplayName:  displayName,
			LastMessage:  message,
			FirstSeenAt:  time.Now(),
			LastSeenAt:   time.Now(),
			RequestCount: 1,
		}
		pendingRequests = append([]db.PrivateAccessRequest{currentReq}, pendingRequests...)
	}

	count := len(pendingRequests)
	var md strings.Builder
	if count <= 1 {
		md.WriteString("### 📬 新的 QQ 私聊权限申请\n\n")
	} else {
		fmt.Fprintf(&md, "### 📬 QQ 私聊权限申请（共 %d 位待处理）\n\n", count)
	}

	for i, req := range pendingRequests {
		name := strings.TrimSpace(req.DisplayName)
		if name == "" {
			name = "未提供昵称"
		}
		msg := singleLineText(req.LastMessage)
		if msg == "" {
			msg = "（空消息或附件消息）"
		}
		msg = core.TruncateRunes(msg, 120, "…")
		lastSeen := req.LastSeenAt.Format("2006-01-02 15:04:05")
		reqCount := req.RequestCount
		if reqCount <= 0 {
			reqCount = 1
		}

		fmt.Fprintf(&md, "#### 👤 #%d · %s\n", i+1, core.TruncateRunes(name, 40, "…"))
		fmt.Fprintf(&md, "- **OpenID**：`%s`\n", req.UserID)
		fmt.Fprintf(&md, "- **最近消息**：%s\n", msg)
		fmt.Fprintf(&md, "- **最近联系**：%s（共 %d 次）\n\n", lastSeen, reqCount)
	}

	md.WriteString("---\n")
	md.WriteString("💡 **权限说明**：\n")
	md.WriteString("- **好友**（`friend`）：仅开放日常私聊对话（不可派发工程工单/切换模型）\n")
	md.WriteString("- **协作者**（`collaborator`）：开放完整工程沙箱与协作者权限\n\n")
	md.WriteString("💬 **操作指令**（可直接回复）：\n")
	if count == 1 {
		md.WriteString("- “将他加为好友” 或 “将第 1 个加为好友”\n")
		md.WriteString("- “将他加为协作者” 或 “将第 1 个加为协作者”\n")
		md.WriteString("- “拒绝申请”\n")
	} else {
		md.WriteString("- “将第 1 个申请者加为好友”（或指定昵称/序号）\n")
		md.WriteString("- “将第 1 个申请者加为协作者”\n")
		md.WriteString("- “拒绝第 1 个申请者的申请”\n")
	}

	body := md.String()

	// 渲染卡片图
	cardCtx, cardCancel := context.WithTimeout(context.Background(), 8*time.Second)
	cardData, cardErr := render.RenderCard(cardCtx, body)
	cardCancel()

	utterance := &entity.Utterance{
		Platform:  "qq",
		ChannelID: ownerID,
		Proactive: true,
	}
	if cardErr == nil && len(cardData) > 0 {
		utterance.CardData = cardData
		utterance.FallbackText = body
		if count <= 1 {
			utterance.Text = "📬 新的 QQ 私聊权限申请"
		} else {
			utterance.Text = fmt.Sprintf("📬 QQ 私聊权限申请（共 %d 位）", count)
		}
	} else {
		if cardErr != nil {
			slog.Warn("qq_channel: 私聊权限申请卡片渲染失败，回退纯文本", "err", cardErr)
		}
		utterance.Text = body
	}

	dedupeKey := fmt.Sprintf("private-access:%s:%d", userID, time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	queued := false
	if q.accessRequests != nil {
		inserted, queueErr := q.accessRequests.EnqueuePendingNotification(ctx, "qq", ownerID, dedupeKey, "private_access_request", body)
		if queueErr != nil {
			slog.Warn("qq_channel: 新私聊权限申请通知入队失败", "channelID", ownerID, "userID", userID, "err", queueErr)
		} else if !inserted {
			cancel()
			return
		} else {
			queued = true
		}
	}
	if queued {
		if err := q.accessRequests.ClaimPendingNotificationByKey(ctx, "qq", ownerID, dedupeKey); err != nil {
			slog.Warn("qq_channel: 无法确认权限申请通知发送状态，跳过主动发送", "channelID", ownerID, "userID", userID, "err", err)
			cancel()
			return
		}
	}
	err := q.Transmit(ctx, utterance)
	if err == nil {
		if queued {
			if deleteErr := q.accessRequests.DeletePendingNotificationByKey(ctx, "qq", ownerID, dedupeKey); deleteErr != nil {
				slog.Warn("qq_channel: 已发送权限申请提醒但未能清除待发副本", "channelID", ownerID, "userID", userID, "err", deleteErr)
			}
		}
		cancel()
		return
	}
	slog.Warn("qq_channel: 新私聊权限申请主动通知失败", "channelID", ownerID, "userID", userID, "err", err)
	if queued {
		state := db.PendingNotificationStateUncertain
		var partial *entity.PartialDeliveryError
		if entity.NotificationDeliveryCanRetry(err) {
			state = db.PendingNotificationStatePending
		} else if errors.As(err, &partial) {
			remaining := strings.TrimSpace(partial.UndeliveredText)
			if remaining == "" {
				if deleteErr := q.accessRequests.DeletePendingNotificationByKey(ctx, "qq", ownerID, dedupeKey); deleteErr != nil {
					slog.Warn("qq_channel: 权限申请提醒部分送达后未能清除通知副本", "channelID", ownerID, "userID", userID, "err", deleteErr)
				}
				cancel()
				return
			}
			if updateErr := q.accessRequests.UpdatePendingNotificationBodyByKey(ctx, "qq", ownerID, dedupeKey, remaining); updateErr != nil {
				slog.Warn("qq_channel: 权限申请提醒部分送达后无法保存剩余正文", "channelID", ownerID, "userID", userID, "err", updateErr)
				state = db.PendingNotificationStateUncertain
			} else {
				cancel()
				return
			}
		}
		if stateErr := q.accessRequests.SetPendingNotificationStateByKey(ctx, "qq", ownerID, dedupeKey, state); stateErr != nil {
			slog.Warn("qq_channel: 无法更新权限申请通知状态", "channelID", ownerID, "userID", userID, "state", state, "err", stateErr)
		}
	}
	cancel()
}

func (q *QQChannel) handleUnprivilegedPrivateMessage(userID, senderName, msgID, content string, imageCount, omittedImageCount, fileCount, omittedFileCount int) {
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(content)), "DAIDAI-") {
		inviteCode, shapeOK := db.NormalizeAccessInviteCode(content)
		if shapeOK && q.accessRequests != nil && q.immune != nil {
			redeemCtx, redeemCancel := context.WithTimeout(context.Background(), 3*time.Second)
			redemption, redeemErr := q.accessRequests.RedeemPrivateAccessInvite(redeemCtx, "qq", db.HashAccessInviteCode(inviteCode), userID)
			redeemCancel()
			if redeemErr == nil && redemption != nil {
				if redemption.EffectiveRole == "collaborator" {
					q.immune.AddCollaborator(userID)
				} else {
					q.immune.AddPrivateUser(userID)
				}
				label := "私聊访客权限"
				if redemption.EffectiveRole == "collaborator" {
					label = "协作者权限"
				}
				q.replyPrivateMessage(userID, msgID, "邀请码验证通过，已开通"+label+"。")
				if q.dedup != nil {
					q.dedup.Remember(msgID)
				}
				return
			}
		}
		q.replyPrivateMessage(userID, msgID, "邀请码无效、已过期或已用完，权限未开通。")
		if q.dedup != nil {
			q.dedup.Remember(msgID)
		}
		return
	}
	if q.dedup != nil {
		q.dedup.Remember(msgID)
	}
	shouldNotifyOwner := false
	accessMessage := strings.TrimSpace(content)
	if entity.IsSensitiveAuthCommand(accessMessage) {
		accessMessage = "[敏感认证指令内容已隐藏]"
	} else if accessMessage == "" {
		accessMessage = attachmentHistoryNote(imageCount, omittedImageCount, fileCount, omittedFileCount)
	}
	accessMessage = core.TruncateRunes(accessMessage, 500, "…")
	if q.accessRequests != nil {
		accessCtx, accessCancel := context.WithTimeout(context.Background(), 2*time.Second)
		var recordErr error
		shouldNotifyOwner, recordErr = q.accessRequests.RecordPrivateAccessRequest(accessCtx, "qq", userID, senderName, accessMessage)
		if recordErr != nil {
			slog.Warn("qq: 保存访客私聊申请失败", "userID", userID, "err", recordErr)
		}
		accessCancel()
	}
	slog.Info("qq: 拒绝未授权访客私聊", "userID", userID, "senderName", senderName, "messageID", msgID)
	q.replyPrivateMessage(userID, msgID, "暂未开通私聊权限")
	if shouldNotifyOwner {
		q.notifyOwnerOfPrivateAccessRequest(userID, senderName, accessMessage)
	}
}
