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

func (s *Service) deliver(parent context.Context, e *Edition, sub Subscription, mode deliveryMode) error {
	force, resume := mode == fullDelivery, mode == remainingDelivery
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	s.mu.Lock()
	k := key(sub)
	if _, busy := s.deliveries[k]; busy || s.closed {
		s.mu.Unlock()
		return fmt.Errorf("delivery already running or stopped")
	}
	s.deliveries[k] = cancel
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.deliveries, k); s.mu.Unlock() }()
	current, err := s.Store.Subscription(ctx, sub.Platform, sub.Recipient)
	if err != nil {
		return err
	}
	if current == nil || (mode == automaticDelivery && !current.Enabled) {
		return fmt.Errorf("subscription disabled")
	}
	if mode == automaticDelivery && !s.due(s.Now()) {
		return s.Store.SaveDelivery(ctx, &Delivery{Edition: e.ID, Platform: sub.Platform, Recipient: sub.Recipient, Status: "expired"})
	}
	verifyCtx, verifyCancel := context.WithTimeout(ctx, 8*time.Second)
	valid := s.Verify != nil && s.Verify(verifyCtx, *current)
	verifyCancel()
	if !valid {
		return s.reject(ctx, e, sub, "recipient_not_verified")
	}
	d, err := s.Store.Delivery(ctx, e.ID, sub.Platform, sub.Recipient)
	if err != nil {
		return err
	}
	if d == nil {
		d = &Delivery{Edition: e.ID, Platform: sub.Platform, Recipient: sub.Recipient}
	}
	if !force && !resume && (d.Status == "sent" || d.Status == "uncertain" || d.Status == "partial" || d.Status == "rejected") {
		return nil
	}
	remaining := ""
	if resume {
		if d.Status == "sent" || strings.TrimSpace(d.Remaining) == "" {
			return fmt.Errorf("no remaining briefing text")
		}
		remaining = d.Remaining
	}
	u := &entity.Utterance{Platform: sub.Platform, ChannelID: sub.Recipient, Proactive: true}
	content := e.PageText(0)
	if resume {
		u.Text = remaining
		content = remaining
	} else if len(e.Items) == 0 {
		u.Text = content
	} else {
		images, source, renderErr := s.Present(ctx, e)
		if renderErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			u.Text = content
			slog.Warn("briefing: 图卡失败，使用文字", "edition", e.ID)
		} else {
			u.Images = images
			u.ImageSources = source
			u.FallbackText = content
			u.Text = e.Index()
			u.InlineImages = sub.Platform == "qq"
		}
	}
	d.Status = "sending"
	d.Attempts++
	d.ErrorKind = ""
	d.Next = time.Time{}
	d.Remaining = content
	if err = s.Store.SaveDelivery(ctx, d); err != nil {
		return err
	}
	if s.Channels == nil {
		return fmt.Errorf("delivery channels unavailable")
	}
	err = s.Channels.Transmit(ctx, u)
	var unsent *entity.UnsentError
	var partialBeforeFallback *entity.PartialDeliveryError
	if err != nil && u.HasMedia() && ctx.Err() == nil && errors.As(err, &unsent) && !errors.As(err, &partialBeforeFallback) {
		// Preparation never reached a message API: one text fallback is safe.
		u.Images = nil
		u.ImageSources = nil
		u.InlineImages = false
		u.FallbackText = ""
		u.Text = content
		err = s.Channels.Transmit(ctx, u)
	}
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer saveCancel()
	if err == nil {
		d.Status = "sent"
		d.Delivered = join(d.Delivered, content)
		d.Remaining = ""
		d.Notified = false
		if saveErr := s.Store.SaveDelivery(saveCtx, d); saveErr != nil {
			return saveErr
		}
		if viewErr := s.Store.Viewed(saveCtx, sub.Platform, sub.Recipient, e.ID); viewErr != nil {
			slog.Error("briefing: 保存展示上下文失败")
		}
		if s.Record != nil {
			s.Record(saveCtx, sub, content)
		}
		return nil
	}
	if delivered := s.failureState(d, err); delivered != "" && s.Record != nil {
		s.Record(saveCtx, sub, delivered)
	}
	if saveErr := s.Store.SaveDelivery(saveCtx, d); saveErr != nil {
		slog.Error("briefing: 保存投递失败状态失败")
	}
	return err
}
func join(a, b string) string {
	return strings.TrimSpace(strings.TrimSpace(a) + "\n\n" + strings.TrimSpace(b))
}
func (s *Service) failureState(d *Delivery, err error) string {
	d.Status = "uncertain"
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
	if delay > 0 && d.Attempts < 3 && s.due(s.Now()) {
		d.Status = "retryable"
		d.Next = s.Now().Add(max(delay, 5*time.Minute))
	}
	return ""
}

// FailedView shares the delivery ledger with scheduled reports. The normal
// chat path owns recording its actually delivered history.
func (s *Service) FailedView(ctx context.Context, e *Edition, sub Subscription, sendErr error) {
	d, err := s.Store.Delivery(ctx, e.ID, sub.Platform, sub.Recipient)
	if err != nil {
		slog.Warn("briefing: 无法读取查询投递状态")
		return
	}
	if d == nil {
		d = &Delivery{Edition: e.ID, Platform: sub.Platform, Recipient: sub.Recipient}
	}
	d.Attempts++
	d.Remaining = e.PageText(0)
	s.failureState(d, sendErr)
	if err = s.Store.SaveDelivery(ctx, d); err != nil {
		slog.Warn("briefing: 无法保存查询投递状态")
	}
}
func (s *Service) reject(ctx context.Context, e *Edition, sub Subscription, kind string) error {
	d, err := s.Store.Delivery(ctx, e.ID, sub.Platform, sub.Recipient)
	if err != nil {
		return err
	}
	if d == nil {
		d = &Delivery{Edition: e.ID, Platform: sub.Platform, Recipient: sub.Recipient}
	}
	d.Status = "rejected"
	d.ErrorKind = kind
	if err := s.Store.SaveDelivery(ctx, d); err != nil {
		return err
	}
	return fmt.Errorf("briefing recipient unavailable")
}
func (s *Service) Resend(sub Subscription, date string, full bool) string {
	e, _, err := s.Store.Edition(s.root, date)
	if err != nil || e == nil {
		return "没有找到这期已生成的早报。"
	}
	if !full {
		d, err := s.Store.Delivery(s.root, date, sub.Platform, sub.Recipient)
		if err != nil || d == nil || d.Status == "sent" || d.Remaining == "" {
			return "这期没有可补发的剩余正文。明确说“完整再发一次”可以重新发送已有刊物。"
		}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "早报服务正在停止。"
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		mode := remainingDelivery
		if full {
			mode = fullDelivery
		}
		if err := s.deliver(s.root, e, sub, mode); err != nil {
			slog.Warn("briefing: 手动补发未完成", "platform", sub.Platform, "kind", errorKind(err))
		}
	}()
	return "正在发送已有早报，不会重新生成内容。"
}
