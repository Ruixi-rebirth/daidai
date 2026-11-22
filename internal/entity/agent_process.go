package entity

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

func (a *SuperAgent) delegationPermissionError(role UserRole) error {
	if a.Immune == nil {
		return fmt.Errorf("权限校验未配置，拒绝执行此操作")
	}
	return a.Immune.VerifyDelegationPerm(role)
}

func parseAGYAuthCodeCommand(content string) (string, bool) {
	parts := strings.Fields(strings.TrimSpace(content))
	if len(parts) < 3 || !strings.EqualFold(parts[0], "/agy") ||
		(!strings.EqualFold(parts[1], "code") && !strings.EqualFold(parts[1], "auth")) {
		return "", false
	}
	return strings.Trim(strings.Join(parts[2:], " "), "`'\""), true
}

func (a *SuperAgent) commitObservedGroupMessage(event *SensoryEvent) {
	if event == nil || (event.StillValid != nil && !event.StillValid()) {
		return
	}
	commitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.commitCognitionReply(commitCtx, event, "")
}

// Process 核心认知信息流：物理感知 -> 控制面快筛 -> 认知大脑 -> 物理通道发声
func (a *SuperAgent) Process(ctx context.Context, event *SensoryEvent) {
	if event == nil {
		return
	}
	if !event.IsGroup && event.Role == RoleGuest && !event.PrivateAccessGranted {
		slog.Info("superagent: 忽略访客私聊", "platform", event.Platform, "channel", event.ChannelID, "sender", event.SenderID)
		return
	}
	if event.StillValid != nil && !event.StillValid() {
		return
	}
	if event.LastHistoryRecordID() == 0 {
		recordCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := a.recordCognitionInbound(recordCtx, event); err != nil {
			slog.Warn("superagent: 保存入站消息历史失败", "channel", event.ChannelID, "err", err)
		}
		cancel()
	}

	content := strings.TrimSpace(event.Content)
	slog.Info("superagent: 感知事件流入大脑", "channel", event.ChannelID, "isGroup", event.IsGroup, "sender", event.SenderName, "role", event.Role, "contentBytes", len(content))
	if event.GroupMessageObserved && ((!event.GroupInterjection && !event.GroupContinuation) || strings.HasPrefix(content, "/") || content == "继续" || a.Cognition == nil) {
		unlock := a.lockSession(event)
		defer unlock()
		if event.StillValid == nil || event.StillValid() {
			a.commitObservedGroupMessage(event)
		}
		return
	}
	if a.runRegisteredSlashCommand(ctx, event, content) {
		return
	}
	if a.askBeforeUnrequestedAttachmentProcessing(ctx, event, content) {
		return
	}

	var thinkCtx context.Context
	if a.Cognition != nil {
		var thinkCancel context.CancelFunc
		thinkCtx, thinkCancel = context.WithCancel(ctx)
		if a.Channels != nil {
			thinkCtx = WithChannelStatus(thinkCtx, a.Channels.FormatStatus())
		}
		cancellation := &thinkCancellation{key: thinkKey(event)}
		a.sessionCancels.Store(cancellation, thinkCancel)
		defer func() {
			a.sessionCancels.Delete(cancellation)
			thinkCancel()
		}()
	}

	// Register cancellation before waiting on the shared channel lock so /stop
	// can also invalidate this request during the lock-acquisition window.
	unlock := a.lockSession(event)
	defer unlock()
	if thinkCtx != nil && thinkCtx.Err() != nil {
		a.notifyCognitionDeadline(event, thinkCtx.Err(), thinkCtx.Err())
		return
	}
	if event.StillValid != nil && !event.StillValid() {
		return
	}
	// Control commands remain immediate. Ordinary private messages deliver
	// durable notifications only after acquiring the session lock and validating
	// their queue generation, so a stale pre-reset event cannot trigger a send.
	if _, isContinue := parseContinueCommand(content); !isContinue && !strings.HasPrefix(content, "/") {
		notificationCtx := ctx
		if thinkCtx != nil {
			notificationCtx = thinkCtx
		}
		a.deliverPendingNotifications(notificationCtx, event)
		if notificationCtx.Err() != nil {
			return
		}
	}

	if event.OnProgress == nil {
		event.OnProgress = func(progressText string) {
			progressCtx := ctx
			if thinkCtx != nil {
				progressCtx = thinkCtx
			}
			if strings.TrimSpace(progressText) == "" || progressCtx.Err() != nil {
				return
			}
			pCtx, cancel := context.WithTimeout(progressCtx, 15*time.Second)
			defer cancel()
			a.replyText(pCtx, event, progressText)
		}
	}

	if a.Cognition != nil {
		utterance, err := a.Cognition.Think(thinkCtx, event)
		if err != nil {
			a.handleCognitionFailure(thinkCtx, event, err)
			return
		}
		if utterance == nil || strings.TrimSpace(utterance.Text) == "[IGNORE]" {
			if event.GroupMessageObserved {
				a.commitObservedGroupMessage(event)
			}
			return
		}

		a.deliverCognitionReply(thinkCtx, ctx, event, utterance)
	}
}
