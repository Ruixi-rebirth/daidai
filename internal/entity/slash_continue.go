package entity

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"daidai/internal/core"
)

func parseContinueCommand(content string) (string, bool) {
	content = strings.TrimSpace(content)
	if content == "继续" {
		return "", true
	}
	if separator := strings.IndexFunc(content, unicode.IsSpace); separator >= 0 {
		return strings.TrimSpace(content[separator:]), content[:separator] == "/continue"
	}
	return "", content == "/continue"
}

func (a *SuperAgent) handleContinueCommand(ctx context.Context, event *SensoryEvent, content string) {
	argument, valid := parseContinueCommand(content)
	if !valid {
		return
	}
	continueCtx, continueCancel := context.WithCancel(ctx)
	cancellation := &thinkCancellation{key: thinkKey(event)}
	a.sessionCancels.Store(cancellation, continueCancel)
	defer func() {
		a.sessionCancels.Delete(cancellation)
		continueCancel()
	}()
	unlock := a.lockSession(event)
	defer unlock()
	if continueCtx.Err() != nil || (event.StillValid != nil && !event.StillValid()) {
		return
	}
	if continuation, ok := a.takeReplyContinuation(event, argument); ok {
		a.deliverReplyContinuation(continueCtx, event, continuation)
		return
	}
	if resumer, ok := a.Cognition.(interface {
		ContinueDraft(context.Context, *SensoryEvent, string) (*Utterance, string, error)
	}); ok {
		utterance, draftMessage, resumeErr := resumer.ContinueDraft(continueCtx, event, argument)
		if resumeErr != nil {
			a.replyText(continueCtx, event, "续写没有完成，草稿仍保留："+resumeErr.Error())
			return
		}
		if utterance != nil {
			addressReply(utterance, event)
			if !event.IsPartnerPrivate() {
				utterance.Text = core.RedactSensitiveIPs(utterance.Text)
			}
			sendCtx, cancel := context.WithTimeout(continueCtx, 90*time.Second)
			delivered, sendErr := a.transmitUtteranceWithContinuationContext(sendCtx, continueCtx, utterance, event.SenderID)
			cancel()
			if sendErr == nil {
				a.commitCognitionReply(continueCtx, event, delivered)
			}
			return
		}
		if draftMessage != "" {
			a.replyText(continueCtx, event, draftMessage)
			return
		}
	}
	if argument != "" {
		a.replyText(continueCtx, event, "当前没有找到该任务的续写草稿。")
		return
	}
	a.replyText(continueCtx, event, "当前没有待续发的长回复。")
}

func (a *SuperAgent) deliverReplyContinuation(continueCtx context.Context, event *SensoryEvent, continuation pendingReplyContinuation) {
	if !event.IsPartnerPrivate() {
		continuation.text = core.RedactSensitiveIPs(continuation.text)
	}
	sendCtx, cancel := context.WithTimeout(continueCtx, 45*time.Second)
	continuationUtterance := &Utterance{Platform: event.Platform, IsGroup: event.IsGroup, ChannelID: event.ChannelID, QuoteMsgID: event.RawMessageID, DraftID: continuation.draftID}
	continuationUtterance.continuationQueuedAt = continuation.queuedAt
	addressReply(continuationUtterance, event)
	if len(continuation.onDelivered) > 0 {
		continuationUtterance.OnDelivered = func(ctx context.Context) {
			for _, callback := range continuation.onDelivered {
				callback(ctx)
			}
		}
	}
	deliveredText, err := a.transmitTextFallbackWithContinuationContext(sendCtx, continueCtx, continuationUtterance, continuation.text, event.SenderID)
	cancel()
	if err != nil {
		if continueCtx.Err() == nil {
			a.restoreReplyContinuation(continueCtx, event, continuation)
			slog.Warn("superagent: 续发长回复失败，已保留续发内容", "channel", event.ChannelID, "err", err)
		}
	} else {
		commitCtx, cancelCommit := context.WithTimeout(continueCtx, 15*time.Second)
		a.commitCognitionReply(commitCtx, event, deliveredText)
		cancelCommit()
	}
}
