package entity

import (
	"context"
	"log/slog"
	"time"

	"daidai/internal/core"
)

func (a *SuperAgent) deliverCognitionReply(thinkCtx, requestCtx context.Context, event *SensoryEvent, utterance *Utterance) {
	if thinkCtx.Err() != nil {
		return
	}
	if event.GroupInterjection && event.StillValid != nil && !event.StillValid() {
		a.commitObservedGroupMessage(event)
		return
	}

	addressReply(utterance, event)
	a.renderAutomaticReplyCard(thinkCtx, event, utterance)
	if !event.IsPartnerPrivate() {
		utterance.Text = core.RedactSensitiveIPs(utterance.Text)
		utterance.FallbackText = core.RedactSensitiveIPs(utterance.FallbackText)
	}
	slog.Info("superagent: 认知合成完毕准备发声", "channel", utterance.ChannelID, "textBytes", len(utterance.Text), "hasMedia", utterance.HasMedia())

	sendTimeout := 30 * time.Second
	if utterance.FixedPresentation {
		sendTimeout = 90 * time.Second
	}
	sendCtx, cancel := context.WithTimeout(thinkCtx, sendTimeout)
	deliveredText, sendErr := a.transmitUtteranceWithContinuationContext(sendCtx, thinkCtx, utterance, event.SenderID)
	cancel()

	if sendErr != nil {
		slog.Error("superagent: 回复发送失败", "channel", utterance.ChannelID, "err", sendErr)
		deliveredText = ""
	}
	commitCtx, commitCancel := context.WithTimeout(requestCtx, 15*time.Second)
	a.commitCognitionReply(commitCtx, event, deliveredText)
	commitCancel()
}
