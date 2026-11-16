package briefing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/engine/render"
	"daidai/internal/entity"
)

type Service struct {
	Store        *Store
	Collector    *Collector
	Model        core.LLMProvider
	Channels     *entity.ChannelRegistry
	Verify       func(context.Context, Subscription) bool
	Record       func(context.Context, Subscription, string)
	loc          *time.Location
	hour, minute int
	root         context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	mu           sync.Mutex
	closed       bool
	job          *generation
	deliveries   map[string]context.CancelFunc
	renderSlot   chan struct{}
	Running      atomic.Bool
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
	return &Service{Store: store, Collector: NewCollector(cfg.GitHubToken), Model: model, Channels: channels, loc: loc, hour: clock.Hour(), minute: clock.Minute(), root: root, cancel: cancel, deliveries: map[string]context.CancelFunc{}, renderSlot: make(chan struct{}, 1)}, nil
}
func key(sub Subscription) string { return sub.Platform + ":" + sub.Recipient }
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
		slog.Warn("briefing: 更新过期投递失败")
	}
	if !s.due(now) {
		return
	}
	ctx, cancel := context.WithTimeout(s.root, 5*time.Second)
	subs, err := s.Store.Subscriptions(ctx)
	cancel()
	if err != nil {
		slog.Warn("briefing: 读取订阅失败")
		return
	}
	var targets []Subscription
	for _, sub := range subs {
		if !sub.Enabled || sub.PausedDay == now.Format("2006-01-02") {
			continue
		}
		d, err := s.Store.Delivery(s.root, now.Format("2006-01-02"), sub.Platform, sub.Recipient)
		if err != nil {
			continue
		}
		if d != nil && (d.Status == "sent" || d.Status == "rejected" || d.Status == "partial" || d.Status == "uncertain" || d.Status == "expired" || d.Status == "sending" || d.Attempts >= 3 || now.Before(d.Next)) {
			continue
		}
		targets = append(targets, sub)
	}
	if len(targets) == 0 {
		return
	}
	can, err := s.Store.CanRetry(s.root, now.Format("2006-01-02"), now)
	if err != nil {
		return
	}
	e, _, err := s.Store.Edition(s.root, now.Format("2006-01-02"))
	if err != nil || (!can && e == nil) {
		return
	}
	s.start(targets, automaticDelivery)
}

// Request returns promptly; generation and delivery do not hold a chat session lock.
func (s *Service) Request(sub Subscription, fullResend bool) string {
	if err := s.Store.Touch(s.root, sub, nil); err != nil {
		return "早报存储暂时不可用，请稍后重试。"
	}
	if err := s.Store.Pause(s.root, sub.Platform, sub.Recipient, ""); err != nil {
		return "无法恢复早报任务，请稍后重试。"
	}
	if d, err := s.Store.Delivery(s.root, s.Now().Format("2006-01-02"), sub.Platform, sub.Recipient); err != nil {
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
			slog.Warn("briefing: 早报生成未完成", "kind", errorKind(err))
			s.notifyGenerationFailure(ctx, job.id, recipients)
			return
		}
		for {
			s.mu.Lock()
			if ctx.Err() != nil || len(job.targets) == 0 {
				if s.job == job {
					s.job = nil
				}
				s.mu.Unlock()
				return
			}
			var wg sync.WaitGroup
			for k, target := range job.targets {
				targetCtx, targetCancel := context.WithCancel(ctx)
				job.active[k] = targetCancel
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer targetCancel()
					if err := s.deliver(targetCtx, e, target.sub, target.mode); err != nil {
						slog.Warn("briefing: 早报投递未完成", "platform", target.sub.Platform, "kind", errorKind(err))
					}
					s.mu.Lock()
					delete(job.active, k)
					s.mu.Unlock()
				}()
			}
			clear(job.targets)
			s.mu.Unlock()
			wg.Wait()
		}
	}()
}
func (s *Service) generate(parent context.Context) (*Edition, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	now := s.Now()
	id := now.Format("2006-01-02")
	if e, _, err := s.Store.Edition(ctx, id); err != nil || e != nil {
		return e, err
	}
	if err := s.Store.Begin(ctx, id, now); err != nil {
		return nil, err
	}
	complete := false
	defer func() {
		if !complete {
			save, cancel := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
			defer cancel()
			if err := s.Store.Fail(save, id, "generation_failed"); err != nil {
				slog.Error("briefing: 保存失败状态失败")
			}
		}
	}()
	collectCtx, collectCancel := context.WithTimeout(ctx, 60*time.Second)
	defer collectCancel()
	items, states, cutoff, err := s.Collector.Collect(collectCtx)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	seen, err := s.Store.RecentIdentities(ctx, now)
	if err != nil {
		return nil, err
	}
	items = candidates(items, seen)
	items = parallelItems(collectCtx, items, s.Collector.readme)
	for index := range items {
		items[index].Evidence = clipped(items[index].Evidence, 1400)
	}
	selected, leads, basic := edit(ctx, s.Model, items)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	localCutoff := cutoff.In(s.loc)
	if localCutoff.Format("2006-01-02") != id {
		return nil, fmt.Errorf("briefing collection crossed local midnight")
	}
	e := &Edition{ID: id, Date: now.Format("2006.01.02"), Weekday: []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[now.Weekday()], Cutoff: localCutoff.Format("15:04 MST"), CutoffAt: cutoff, Timezone: s.loc.String(), SourceStates: states, Basic: basic}
	if s.Model != nil {
		e.Model = s.Model.Name()
	}
	finalize(e, selected, leads)
	if err = s.Store.Save(ctx, e); err != nil {
		return nil, err
	}
	complete = true
	slog.Info("briefing: 已归档早报", "edition", e.ID, "items", len(e.Items), "basic", e.Basic)
	return e, nil
}
func (s *Service) Present(ctx context.Context, e *Edition) ([][]byte, []string, error) {
	select {
	case s.renderSlot <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	defer func() { <-s.renderSlot }()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	doc := render.BriefingDocument{Date: e.Date, Weekday: e.Weekday, Cutoff: e.Cutoff, Basic: e.Basic, Leads: e.Leads}
	var notes []string
	for _, st := range e.SourceStates {
		if st.Status == "error" || st.Status == "partial" {
			notes = append(notes, labels[st.Source]+"资料不完整")
		}
	}
	doc.SourceNote = strings.Join(notes, "；")
	for _, i := range e.Items {
		doc.Items = append(doc.Items, render.BriefingItem{Number: i.Number, Page: i.Page, Source: i.Source, Title: i.Title, Summary: i.Summary, Meta: i.Meta, EvidenceKind: i.EvidenceKind})
	}
	if len(e.Items) == 0 {
		return nil, nil, nil
	}
	images, err := render.RenderBriefing(ctx, doc)
	if err != nil {
		return nil, nil, err
	}
	var pageSources []string
	for index := range images {
		pageSources = append(pageSources, e.PageText(index+1))
	}
	return images, pageSources, nil
}
func (s *Service) Cancel(platform, recipient string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := platform + ":" + recipient
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
		return
	}
	for _, target := range targets {
		sub := target.sub
		d, err := s.Store.Delivery(save, id, sub.Platform, sub.Recipient)
		if err != nil {
			continue
		}
		if d == nil {
			d = &Delivery{Edition: id, Platform: sub.Platform, Recipient: sub.Recipient}
		}
		d.Status = "retryable"
		if !can {
			d.Status = "rejected"
		}
		d.ErrorKind = "generation_failed"
		d.Next = s.Now().Add(5 * time.Minute)
		if err = s.Store.SaveDelivery(save, d); err != nil {
			slog.Error("briefing: 保存生成失败提醒失败")
		}
	}
}
