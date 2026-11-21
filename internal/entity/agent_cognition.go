package entity

import (
	"context"
)

type cognitionInboundRecorder interface {
	RecordInbound(context.Context, *SensoryEvent) error
}

type cognitionReplyCommitter interface {
	CommitDeliveredReply(context.Context, *SensoryEvent, string)
}

func (a *SuperAgent) recordCognitionInbound(ctx context.Context, event *SensoryEvent) error {
	recorder, ok := a.Cognition.(cognitionInboundRecorder)
	if !ok {
		return nil
	}
	return recorder.RecordInbound(ctx, event)
}

func (a *SuperAgent) commitCognitionReply(ctx context.Context, event *SensoryEvent, text string) {
	committer, ok := a.Cognition.(cognitionReplyCommitter)
	if ok {
		committer.CommitDeliveredReply(ctx, event, text)
	}
}
