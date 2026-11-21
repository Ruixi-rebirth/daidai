package briefing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/entity"
)

type Service struct {
	Store         *Store
	Collector     *Collector
	Model         core.LLMProvider
	Channels      *entity.ChannelRegistry
	Verify        func(context.Context, Subscription) (bool, error)
	Record        func(context.Context, Subscription, string)
	loc           *time.Location
	hour, minute  int
	root          context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	mu            sync.Mutex
	closed        bool
	job           *generation
	deliveries    map[string]context.CancelFunc
	deliverySlots chan struct{}
	renderSlot    chan struct{}
	Running       atomic.Bool
}
type generation struct {
	targets map[string]deliveryTarget
	active  map[string]context.CancelFunc
	ctx     context.Context
	id      string
	cancel  context.CancelFunc
	done    chan struct{}
}
type deliveryMode int

const (
	automaticDelivery deliveryMode = iota
	requestedDelivery
	remainingDelivery
	fullDelivery
)

type deliveryTarget struct {
	sub  Subscription
	mode deliveryMode
}

const maxConcurrentBriefingDeliveries = 4

func New(ctx context.Context, store *Store, cfg config.BriefingConfig, model core.LLMProvider, channels *entity.ChannelRegistry) (*Service, error) {
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, err
	}
	clock, err := time.Parse("15:04", cfg.Time)
	if err != nil {
		return nil, err
	}
	root, cancel := context.WithCancel(ctx)
	return &Service{Store: store, Collector: NewCollector(cfg.GitHubToken), Model: model, Channels: channels, loc: loc, hour: clock.Hour(), minute: clock.Minute(), root: root, cancel: cancel, deliveries: map[string]context.CancelFunc{}, deliverySlots: make(chan struct{}, maxConcurrentBriefingDeliveries), renderSlot: make(chan struct{}, 1)}, nil
}
func key(sub Subscription) string {
	return fmt.Sprintf("%s:%t:%s", sub.Platform, sub.IsGroup, sub.Recipient)
}

// Request returns promptly; generation and delivery do not hold a chat session lock.
func (s *Service) Request(sub Subscription, fullResend bool) string {
	if err := s.Store.Touch(s.root, sub, nil); err != nil {
		return "早报存储暂时不可用，请稍后重试。"
	}
	if err := s.Store.Pause(s.root, sub.Platform, sub.Recipient, sub.IsGroup, ""); err != nil {
		return "无法恢复早报任务，请稍后重试。"
	}
	if d, err := s.Store.Delivery(s.root, s.Now().Format("2006-01-02"), sub.Platform, sub.Recipient, sub.IsGroup); err != nil {
		return "无法确认已有早报的投递状态，请稍后重试。"
	} else if d != nil && !fullResend {
		switch d.Status {
		case "sent":
			return "今天的早报已经生成并送达，无需重新生成；可以直接查看今天的早报。"
		case "partial", "uncertain":
			return "今天的早报已经生成，但上次没有确认完整送达；可以明确要求补发剩余内容，或直接查看整期。"
		case "rejected":
			if d.ErrorKind != "generation_failed" {
				return "今天的早报投递被拒绝；可以查看状态，或明确要求补发已有刊物。"
			}
			d.Status = "pending"
			d.ErrorKind = ""
			if err := s.Store.SaveDelivery(s.root, d); err != nil {
				return "无法保存重试请求，请稍后重试。"
			}
		}
	}
	mode := requestedDelivery
	if fullResend {
		mode = fullDelivery
	}
	s.start([]Subscription{sub}, mode)
	return "正在准备今天的极客早报，完成后会发到这里；同一天已有的内容会直接使用。"
}
func (s *Service) start(targets []Subscription, mode deliveryMode) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if s.job != nil && (s.job.ctx.Err() != nil || s.job.id != s.Now().Format("2006-01-02")) {
		s.job.cancel()
		s.job = nil
	}
	if s.job != nil {
		for _, sub := range targets {
			k := key(sub)
			if s.job.active[k] == nil {
				previous, exists := s.job.targets[k]
				if !exists || mode > previous.mode {
					s.job.targets[k] = deliveryTarget{sub, mode}
				}
			}
		}
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(s.root)
	job := &generation{targets: map[string]deliveryTarget{}, active: map[string]context.CancelFunc{}, ctx: ctx, id: s.Now().Format("2006-01-02"), cancel: cancel, done: make(chan struct{})}
	for _, sub := range targets {
		job.targets[key(sub)] = deliveryTarget{sub, mode}
	}
	s.job = job
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer cancel()
		defer close(job.done)
		e, err := s.generate(ctx)
		if err != nil {
			s.mu.Lock()
			var recipients []deliveryTarget
			for _, target := range job.targets {
				recipients = append(recipients, target)
			}
			if s.job == job {
				s.job = nil
			}
			s.mu.Unlock()
			if errors.Is(err, context.Canceled) {
				slog.Info("briefing: 早报任务已取消")
				return
			}
			slog.Warn("briefing: 早报生成未完成", "kind", errorKind(err), "err", err)
			s.notifyGenerationFailure(ctx, job.id, recipients)
			return
		}
		for {
			s.mu.Lock()
			if ctx.Err() != nil || (len(job.targets) == 0 && len(job.active) == 0) {
				if s.job == job {
					s.job = nil
				}
				s.mu.Unlock()
				return
			}
			var wg sync.WaitGroup
			// Each batch is fully joined before the next loop, so active only
			// indexes this batch and this cap bounds all delivery goroutines.
			type delivery struct {
				key    string
				target deliveryTarget
				ctx    context.Context
				cancel context.CancelFunc
			}
			batch := make([]delivery, 0, maxConcurrentBriefingDeliveries)
			for k, target := range job.targets {
				if len(batch) >= maxConcurrentBriefingDeliveries {
					break
				}
				targetCtx, targetCancel := context.WithCancel(ctx)
				job.active[k] = targetCancel
				delete(job.targets, k)
				batch = append(batch, delivery{key: k, target: target, ctx: targetCtx, cancel: targetCancel})
			}
			for _, item := range batch {
				wg.Add(1)
				go func(item delivery) {
					defer wg.Done()
					defer item.cancel()
					if err := s.deliver(item.ctx, e, item.target.sub, item.target.mode); err != nil {
						slog.Warn("briefing: 早报投递未完成", "platform", item.target.sub.Platform, "recipient", item.target.sub.Recipient, "isGroup", item.target.sub.IsGroup, "kind", errorKind(err), "err", err)
					}
					s.mu.Lock()
					delete(job.active, item.key)
					s.mu.Unlock()
				}(item)
			}
			s.mu.Unlock()
			wg.Wait()
		}
	}()
}
func (s *Service) Cancel(platform, recipient string, group bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(Subscription{Platform: platform, Recipient: recipient, IsGroup: group})
	found := false
	if cancel := s.deliveries[k]; cancel != nil {
		found = true
		cancel()
	}
	if s.job != nil {
		if _, ok := s.job.targets[k]; ok {
			found = true
		}
		delete(s.job.targets, k)
		if cancel := s.job.active[k]; cancel != nil {
			found = true
			cancel()
			delete(s.job.active, k)
		}
		if len(s.job.targets) == 0 && len(s.job.active) == 0 {
			s.job.cancel()
		}
	}
	return found
}
func errorKind(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "unavailable"
}
func (s *Service) notifyGenerationFailure(ctx context.Context, id string, targets []deliveryTarget) {
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	can, err := s.Store.CanRetry(save, id, s.Now().Add(5*time.Minute))
	if err != nil {
		slog.Warn("briefing: 无法安排生成失败后的重试", "edition", id, "err", err)
		return
	}
	for _, target := range targets {
		sub := target.sub
		d, err := s.Store.Delivery(save, id, sub.Platform, sub.Recipient, sub.IsGroup)
		if err != nil {
			slog.Warn("briefing: 无法读取生成失败提醒状态", "edition", id, "platform", sub.Platform, "recipient", sub.Recipient, "isGroup", sub.IsGroup, "err", err)
			continue
		}
		if d == nil {
			d = &Delivery{Edition: id, Platform: sub.Platform, Recipient: sub.Recipient, IsGroup: sub.IsGroup}
		}
		d.Status = "retryable"
		if !can {
			d.Status = "rejected"
		}
		d.ErrorKind = "generation_failed"
		d.Next = s.Now().Add(5 * time.Minute)
		if err = s.Store.SaveDelivery(save, d); err != nil {
			slog.Error("briefing: 保存生成失败提醒失败", "edition", id, "platform", sub.Platform, "recipient", sub.Recipient, "isGroup", sub.IsGroup, "err", err)
		}
	}
}
