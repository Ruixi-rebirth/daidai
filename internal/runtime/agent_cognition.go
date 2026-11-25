package runtime

import (
	"context"
	"daidai/internal/entity"
)

type cognitionInboundRecorder interface {
	RecordInbound(context.Context, *entity.SensoryEvent) error
}

type cognitionReplyCommitter interface {
	CommitDeliveredReply(context.Context, *entity.SensoryEvent, string)
}

func (a *SuperAgent) recordCognitionInbound(ctx context.Context, event *entity.SensoryEvent) error {
	recorder, ok := a.Cognition.(cognitionInboundRecorder)
	if !ok {
		return nil
	}
	return recorder.RecordInbound(ctx, event)
}

func (a *SuperAgent) commitCognitionReply(ctx context.Context, event *entity.SensoryEvent, text string) {
	committer, ok := a.Cognition.(cognitionReplyCommitter)
	if ok {
		committer.CommitDeliveredReply(ctx, event, text)
	}
}
