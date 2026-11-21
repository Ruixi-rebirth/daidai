package briefing

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

func (s *Service) Now() time.Time { return time.Now().In(s.loc) }

func (s *Service) Schedule() string {
	return fmt.Sprintf("每天 %02d:%02d（%s）", s.hour, s.minute, s.loc.String())
}

func (s *Service) NextRun() time.Time {
	now := s.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), s.hour, s.minute, 0, 0, s.loc)
	if !now.Before(next) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

func (s *Service) due(now time.Time) bool {
	now = now.In(s.loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), s.hour, s.minute, 0, 0, s.loc)
	return !now.Before(start) && now.Before(start.Add(2*time.Hour))
}

func (s *Service) Start() {
	s.mu.Lock()
	if s.closed || s.Running.Load() {
		s.mu.Unlock()
		return
	}
	s.Running.Store(true)
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer s.Running.Store(false)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		s.tick()
		for {
			select {
			case <-s.root.Done():
				return
			case <-ticker.C:
				s.tick()
			}
		}
	}()
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) tick() {
	now := s.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), s.hour, s.minute, 0, 0, s.loc)
	if err := s.Store.Expire(s.root, now.Format("2006-01-02"), !now.Before(start.Add(2*time.Hour))); err != nil {
		slog.Warn("briefing: 更新过期投递失败", "err", err)
	}
	if !s.due(now) {
		return
	}
	ctx, cancel := context.WithTimeout(s.root, 5*time.Second)
	subs, err := s.Store.Subscriptions(ctx)
	cancel()
	if err != nil {
		slog.Warn("briefing: 读取订阅失败", "err", err)
		return
	}
	var targets []Subscription
	for _, sub := range subs {
		if !sub.Enabled || sub.PausedDay == now.Format("2006-01-02") {
			continue
		}
		d, err := s.Store.Delivery(s.root, now.Format("2006-01-02"), sub.Platform, sub.Recipient, sub.IsGroup)
		if err != nil {
			slog.Warn("briefing: 读取订阅投递状态失败", "platform", sub.Platform, "recipient", sub.Recipient, "isGroup", sub.IsGroup, "err", err)
			continue
		}
		if !canAutomaticallyDeliver(d, now) {
			continue
		}
		targets = append(targets, sub)
	}
	if len(targets) == 0 {
		return
	}
	can, err := s.Store.CanRetry(s.root, now.Format("2006-01-02"), now)
	if err != nil {
		slog.Warn("briefing: 读取重试状态失败", "err", err)
		return
	}
	e, _, err := s.Store.Edition(s.root, now.Format("2006-01-02"))
	if err != nil {
		slog.Warn("briefing: 读取当日早报失败", "err", err)
		return
	}
	if !can && e == nil {
		return
	}
	s.start(targets, automaticDelivery)
}
