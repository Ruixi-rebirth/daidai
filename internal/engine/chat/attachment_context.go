package chat

import (
	"context"

	"daidai/internal/entity"
)

func (c *Cortex) prepareAttachments(ctx context.Context, event *entity.SensoryEvent) *entity.Utterance {
	if event == nil {
		return nil
	}
	c.mergePendingAttachments(event)
	boundAttachmentInput(event)
	return c.resolveAttachmentIntent(ctx, event)
}
