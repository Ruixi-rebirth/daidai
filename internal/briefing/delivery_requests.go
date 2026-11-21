package briefing

import (
	"context"
	"daidai/internal/entity"
	"log/slog"
	"strings"
	"time"
)

// RecordViewDelivered records a manual view without replacing a running push.
func (s *Service) RecordViewDelivered(ctx context.Context, editionID string, sub Subscription, body string) {
	if err := s.Store.Viewed(ctx, sub.Platform, sub.Recipient, sub.IsGroup, editionID); err != nil {
		slog.Warn("briefing: 无法保存展示上下文", "err", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.deliveries[key(sub)]; busy {
		return
	}
	d, err := s.Store.Delivery(ctx, editionID, sub.Platform, sub.Recipient, sub.IsGroup)
	if err != nil {
		slog.Warn("briefing: 无法读取投递状态", "err", err)
		return
	}
	if d == nil || d.Status == "sending" {
		return
	}
	d.Status, d.Delivered, d.Remaining, d.ErrorKind = "sent", body, "", ""
	d.Next = time.Time{}
	d.Notified = false
	if err := s.Store.SaveDelivery(ctx, d); err != nil {
		slog.Warn("briefing: 无法保存阅览投递状态", "err", err)
	}
}

// FailedView shares the delivery ledger with scheduled reports. The normal
// chat path owns recording its actually delivered history.
func (s *Service) FailedView(ctx context.Context, e *Edition, sub Subscription, sendErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.deliveries[key(sub)]; busy {
		return
	}
	d, err := s.Store.Delivery(ctx, e.ID, sub.Platform, sub.Recipient, sub.IsGroup)
	if err != nil {
		slog.Warn("briefing: 无法读取查询投递状态", "err", err)
		return
	}
	if d == nil {
		d = &Delivery{Edition: e.ID, Platform: sub.Platform, Recipient: sub.Recipient, IsGroup: sub.IsGroup}
	}
	d.Attempts++
	d.Remaining = e.Text()
	d.Delivered = ""
	s.failureState(d, sendErr)
	if err = s.Store.SaveDelivery(ctx, d); err != nil {
		slog.Warn("briefing: 无法保存查询投递状态", "err", err)
	}
}

func (s *Service) Resend(sub Subscription, date string, full bool) string {
	e, _, err := s.Store.Edition(s.root, date)
	if err != nil || e == nil {
		return "没有找到这期已生成的早报。"
	}
	if !full {
		d, err := s.Store.Delivery(s.root, date, sub.Platform, sub.Recipient, sub.IsGroup)
		if err != nil || !hasRemainingDelivery(d) {
			return "这期没有可补发的剩余正文。明确说“完整再发一次”可以重新发送已有刊物。"
		}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "早报服务正在停止。"
	}
	ctx, finish := s.reserveDeliveryLocked(s.root, sub)
	if finish == nil {
		s.mu.Unlock()
		return "当前会话已有早报正在发送，请稍后再试。"
	}
	select {
	case s.deliverySlots <- struct{}{}:
	default:
		s.mu.Unlock()
		finish()
		return "当前早报投递较多，请稍后再试。"
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer finish()
		defer func() { <-s.deliverySlots }()
		mode := remainingDelivery
		if full {
			mode = fullDelivery
		}
		if err := s.sendEdition(ctx, e, sub, mode); err != nil {
			slog.Warn("briefing: 手动补发未完成", "platform", sub.Platform, "kind", errorKind(err), "err", err)
		}
	}()
	return "正在发送已有早报，不会重新生成内容。"
}

func hasRemainingDelivery(d *Delivery) bool {
	return d != nil && d.Status != "sent" && d.Status != "sending" && strings.TrimSpace(d.Remaining) != ""
}

// NotifyPrivateInteraction sends one pending briefing failure notice.
func (s *Service) NotifyPrivateInteraction(ctx context.Context, event *entity.SensoryEvent) {
	if event == nil || !event.IsPartnerPrivate() || event.RawMessageID == "" || s.Channels == nil {
		return
	}
	noticeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	failure, err := s.Store.latestUnnotifiedFailure(noticeCtx, event.Platform, event.ChannelID, false)
	if err != nil || failure == nil {
		return
	}
	id := failure.editionID
	text := id + " 的极客早报未完成投递，可以查询状态或补发。"
	switch failure.status {
	case "rejected":
	case "uncertain":
		text = id + " 的早报发送结果未确认；如果没有收到，可以让我补发。"
	case "partial":
		text = id + " 的早报还有未送完的正文，可以说‘继续早报’补看。"
	case "expired":
		text = id + " 的早报已过自动补发时间，仍可让我查看已有刊物。"
	default:
		return
	}
	if failure.errorKind == "generation_failed" {
		text = id + " 的早报生成未完成，可以让我重新尝试生成。"
	}
	u := &entity.Utterance{Platform: event.Platform, ChannelID: event.ChannelID, QuoteMsgID: event.RawMessageID, Text: text}
	if err = s.Channels.Transmit(noticeCtx, u); err != nil {
		return
	}
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer saveCancel()
	if err = s.Store.markFailureNotified(saveCtx, failure, event.Platform, event.ChannelID, false); err != nil {
		slog.Warn("briefing: 保存异常提醒状态失败", "err", err)
	}
	if s.Record != nil {
		s.Record(saveCtx, Subscription{Platform: event.Platform, Recipient: event.ChannelID}, text)
	}
}
