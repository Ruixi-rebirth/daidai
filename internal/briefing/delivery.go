package briefing

import (
	"context"
	"daidai/internal/entity"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

func (s *Service) deliver(parent context.Context, e *Edition, sub Subscription, mode deliveryMode) error {
	s.mu.Lock()
	ctx, finish := s.reserveDeliveryLocked(parent, sub)
	s.mu.Unlock()
	if finish == nil {
		return fmt.Errorf("delivery already running or stopped")
	}
	defer finish()
	select {
	case s.deliverySlots <- struct{}{}:
		defer func() { <-s.deliverySlots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.sendEdition(ctx, e, sub, mode)
}

func (s *Service) reserveDeliveryLocked(parent context.Context, sub Subscription) (context.Context, func()) {
	k := key(sub)
	if _, busy := s.deliveries[k]; busy || s.closed {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	s.deliveries[k] = cancel
	return ctx, func() {
		cancel()
		s.mu.Lock()
		delete(s.deliveries, k)
		s.mu.Unlock()
	}
}

func (s *Service) sendEdition(ctx context.Context, e *Edition, sub Subscription, mode deliveryMode) error {
	plan, err := s.prepareDelivery(ctx, e, sub, mode)
	if err != nil || plan == nil {
		return err
	}
	plan.delivery.Status = "sending"
	if !plan.resume {
		plan.delivery.Delivered = ""
	}
	plan.delivery.Attempts++
	plan.delivery.ErrorKind = ""
	plan.delivery.Next = time.Time{}
	plan.delivery.Remaining = plan.content
	if err := s.Store.SaveDelivery(ctx, plan.delivery); err != nil {
		return err
	}

	sendErr := s.transmitEdition(ctx, plan.utterance, plan.content)
	return s.recordDeliveryResult(ctx, e, sub, plan, sendErr)
}

type deliveryPlan struct {
	delivery  *Delivery
	utterance *entity.Utterance
	content   string
	resume    bool
}

func (s *Service) prepareDelivery(ctx context.Context, e *Edition, sub Subscription, mode deliveryMode) (*deliveryPlan, error) {
	force, resume := mode == fullDelivery, mode == remainingDelivery
	current, err := s.Store.Subscription(ctx, sub.Platform, sub.Recipient, sub.IsGroup)
	if err != nil {
		return nil, err
	}
	if current == nil || (mode == automaticDelivery && !current.Enabled) {
		return nil, fmt.Errorf("subscription disabled")
	}
	d, err := s.Store.Delivery(ctx, e.ID, sub.Platform, sub.Recipient, sub.IsGroup)
	if err != nil {
		return nil, err
	}
	if d == nil {
		d = &Delivery{Edition: e.ID, Platform: sub.Platform, Recipient: sub.Recipient, IsGroup: sub.IsGroup}
	}
	if !force && !resume && (d.Status == "sent" || d.Status == "uncertain" || d.Status == "partial" || d.Status == "rejected") {
		return nil, nil
	}
	if resume && !hasRemainingDelivery(d) {
		return nil, fmt.Errorf("no remaining briefing text")
	}
	if mode == automaticDelivery {
		now := s.Now()
		if current.PausedDay == e.ID || !canAutomaticallyDeliver(d, now) {
			return nil, nil
		}
		if e.ID != now.Format("2006-01-02") || !s.due(now) {
			d.Status = "expired"
			return nil, s.Store.SaveDelivery(ctx, d)
		}
	}
	if s.Channels == nil {
		return nil, fmt.Errorf("delivery channels unavailable")
	}
	if _, ok := s.Channels.Get(sub.Platform); !ok {
		return nil, fmt.Errorf("briefing delivery channel %q is not mounted", sub.Platform)
	}
	verifyCtx, verifyCancel := context.WithTimeout(ctx, 8*time.Second)
	valid := false
	if s.Verify != nil {
		valid, err = s.Verify(verifyCtx, *current)
	}
	verifyContextErr := verifyCtx.Err()
	verifyCancel()
	if verifyContextErr != nil {
		return nil, verifyContextErr
	}
	if err != nil {
		return nil, fmt.Errorf("verify briefing recipient: %w", err)
	}
	if !valid {
		return nil, s.reject(ctx, e, sub, "recipient_not_verified")
	}
	u := &entity.Utterance{Platform: sub.Platform, ChannelID: sub.Recipient, IsGroup: sub.IsGroup, Proactive: true}
	content := e.Text()
	if resume {
		content = d.Remaining
	}
	u.Text = content
	if !resume && len(e.Items) > 0 {
		images, source, renderErr := s.Present(ctx, e)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if renderErr != nil || len(images) == 0 {
			if renderErr == nil {
				renderErr = errors.New("renderer returned no image")
			}
			slog.Warn("briefing: 图卡失败，使用文字", "edition", e.ID, "err", renderErr)
		} else {
			u.Images = images
			u.ImageSources = source
			u.FallbackText = content
			u.Text = e.Index()
			u.InlineImages = sub.Platform == "qq"
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &deliveryPlan{delivery: d, utterance: u, content: content, resume: resume}, nil
}

func (s *Service) transmitEdition(ctx context.Context, utterance *entity.Utterance, content string) error {
	err := s.Channels.Transmit(ctx, utterance)
	var unsent *entity.UnsentError
	var partialBeforeFallback *entity.PartialDeliveryError
	if err != nil && utterance.HasMedia() && ctx.Err() == nil && errors.As(err, &unsent) && !errors.As(err, &partialBeforeFallback) {
		// Preparation never reached a message API: one text fallback is safe.
		utterance.Images = nil
		utterance.ImageSources = nil
		utterance.InlineImages = false
		utterance.FallbackText = ""
		utterance.Text = content
		err = s.Channels.Transmit(ctx, utterance)
	}
	return err
}
