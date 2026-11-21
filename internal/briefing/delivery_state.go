package briefing

import (
	"context"
	"daidai/internal/entity"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

func (s *Service) recordDeliveryResult(ctx context.Context, e *Edition, sub Subscription, plan *deliveryPlan, err error) error {
	d := plan.delivery
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer saveCancel()
	if err == nil {
		d.Status = "sent"
		if plan.resume {
			d.Delivered = join(d.Delivered, plan.content)
		} else {
			d.Delivered = plan.content
		}
		d.Remaining = ""
		d.Notified = false
		if saveErr := s.Store.SaveDelivery(saveCtx, d); saveErr != nil {
			return saveErr
		}
		if viewErr := s.Store.Viewed(saveCtx, sub.Platform, sub.Recipient, sub.IsGroup, e.ID); viewErr != nil {
			slog.Error("briefing: 保存展示上下文失败", "err", viewErr)
		}
		if s.Record != nil {
			s.Record(saveCtx, sub, plan.content)
		}
		return nil
	}
	if delivered := s.failureState(d, err); delivered != "" && s.Record != nil {
		s.Record(saveCtx, sub, delivered)
	}
	if saveErr := s.Store.SaveDelivery(saveCtx, d); saveErr != nil {
		slog.Error("briefing: 保存投递失败状态失败", "err", saveErr)
	}
	return err
}

func canAutomaticallyDeliver(d *Delivery, now time.Time) bool {
	if d == nil {
		return true
	}
	switch d.Status {
	case "sent", "rejected", "partial", "uncertain", "expired", "sending":
		return false
	default:
		return d.Attempts < 3 && !now.Before(d.Next)
	}
}

func join(a, b string) string {
	return strings.TrimSpace(strings.TrimSpace(a) + "\n\n" + strings.TrimSpace(b))
}

func (s *Service) failureState(d *Delivery, err error) string {
	d.Status = "uncertain"
	d.Notified = false
	d.ErrorKind = errorKind(err)
	d.Next = time.Time{}
	var partial *entity.PartialDeliveryError
	if errors.As(err, &partial) {
		d.Delivered = join(d.Delivered, partial.DeliveredText)
		d.Remaining = partial.UndeliveredText
		if partial.DeliveredText != "" {
			d.Status = "partial"
		}
		return partial.DeliveredText
	}
	var rejection *entity.DeliveryRejection
	var unsent *entity.UnsentError
	delay := time.Duration(0)
	if errors.As(err, &rejection) {
		d.Status = "rejected"
		d.ErrorKind = "platform_rejected"
		delay = rejection.After
	} else if errors.As(err, &unsent) {
		d.Status = "rejected"
		if !errors.Is(err, context.Canceled) {
			delay = 5 * time.Minute
		}
	}
	if d.Status == "rejected" && strings.TrimSpace(d.Delivered) != "" {
		// A rejected continuation still has a confirmed prefix. Retrying the
		// edition through the automatic path would send that prefix again.
		d.Status = "partial"
		return ""
	}
	now := s.Now()
	if delay > 0 && d.Attempts < 3 && s.due(now) {
		d.Status = "retryable"
		d.Next = now.Add(max(delay, 5*time.Minute))
	}
	return ""
}

func (s *Service) reject(ctx context.Context, e *Edition, sub Subscription, kind string) error {
	d, err := s.Store.Delivery(ctx, e.ID, sub.Platform, sub.Recipient, sub.IsGroup)
	if err != nil {
		return err
	}
	if d == nil {
		d = &Delivery{Edition: e.ID, Platform: sub.Platform, Recipient: sub.Recipient, IsGroup: sub.IsGroup}
	}
	d.Status = "rejected"
	d.ErrorKind = kind
	d.Notified = false
	if err := s.Store.SaveDelivery(ctx, d); err != nil {
		return err
	}
	return fmt.Errorf("briefing recipient unavailable")
}
