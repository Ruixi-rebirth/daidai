package kernel

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"daidai/internal/core"
)

// SessionKey 标识会话隔离键。
type SessionKey struct {
	Platform  string
	IsGroup   bool
	ChannelID string
	SenderID  string
}

func (k SessionKey) String() string {
	platform := k.Platform + ":"
	if k.IsGroup {
		return platform + "g:" + k.ChannelID + ":" + k.SenderID
	}
	return platform + "p:" + k.ChannelID + ":" + k.SenderID
}

// SessionHandler 窗口合并后的处理回调
type SessionHandler func(mergedEvent *core.SensoryEvent)

type queuedEvent struct {
	event        *core.SensoryEvent
	generation   uint64
	afterControl <-chan struct{}
}

const maxMergedEventsPerTurn = 64

// DebounceConfig 防抖配置
type DebounceConfig struct {
	MergeWindow    time.Duration // 连续发言合并窗口，默认 2 秒
	IdleTimeout    time.Duration // 空闲回收时间，默认 5 分钟
	MaxMergedRunes int           // 合并事件的字符上限，默认沿用用户消息上限
}

// sessionWorker 单个会话的 Actor 调度器
type sessionWorker struct {
	manager    *DebounceManager
	key        SessionKey
	config     DebounceConfig
	handler    SessionHandler
	msgCh      chan queuedEvent
	processCh  chan queuedEvent
	queued     atomic.Int32
	pushers    atomic.Int32
	generation atomic.Uint64
	closeCh    chan struct{}
	closeOnce  sync.Once
}

func (w *sessionWorker) close() {
	w.closeOnce.Do(func() {
		close(w.closeCh)
	})
}

// DebounceManager 负责并发会话防抖与 Actor 调度
type DebounceManager struct {
	config          DebounceConfig
	handler         SessionHandler
	workers         sync.Map // map[SessionKey]*sessionWorker
	mu              sync.Mutex
	controlBarriers map[SessionKey]chan struct{}
	stopped         bool
	wg              sync.WaitGroup
}

// NewDebounceManager 实例化防抖管理器
func NewDebounceManager(cfg DebounceConfig, handler SessionHandler) *DebounceManager {
	if cfg.MergeWindow <= 0 {
		cfg.MergeWindow = 2 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 5 * time.Minute
	}
	if cfg.MaxMergedRunes <= 0 {
		cfg.MaxMergedRunes = core.MaxUserMessageRunes
	}
	return &DebounceManager{
		config:  cfg,
		handler: handler,
	}
}

// Push 收到新感知事件，控制面指令直通分发，普通对话投递到对应用户的防抖管道
func (m *DebounceManager) Push(event *core.SensoryEvent) bool {
	if event == nil {
		return false
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return false
	}
	// 以 "/" 开头的控制指令（如 /stop, /status, /help）零延迟直通，不进入合并窗口以免被前序普通文本吞没
	if strings.HasPrefix(strings.TrimSpace(event.Content), "/") && len(event.Images) == 0 {
		var barrier chan struct{}
		var previousBarrier <-chan struct{}
		barrierKey := SessionKey{Platform: event.Platform, IsGroup: event.IsGroup, ChannelID: event.ChannelID}
		if invalidatesPendingTurns(event.Content) && (event.Role == core.RolePartner || event.Role == core.RoleCollaborator) {
			m.workers.Range(func(_, value any) bool {
				worker, ok := value.(*sessionWorker)
				if ok && worker.key.Platform == event.Platform && worker.key.IsGroup == event.IsGroup && worker.key.ChannelID == event.ChannelID {
					worker.generation.Add(1)
				}
				return true
			})
		}
		if requiresControlBarrier(event.Content) && (event.Role == core.RolePartner || event.Role == core.RoleCollaborator) {
			if m.controlBarriers == nil {
				m.controlBarriers = make(map[SessionKey]chan struct{})
			}
			previousBarrier = m.controlBarriers[barrierKey]
			barrier = make(chan struct{})
			m.controlBarriers[barrierKey] = barrier
		}
		handler := m.handler
		if handler != nil || barrier != nil {
			m.wg.Add(1)
		}
		m.mu.Unlock()
		if handler != nil || barrier != nil {
			go func() {
				defer m.wg.Done()
				defer func() {
					if recovered := recover(); recovered != nil {
						slog.Error("debounce: control event handler panicked", "session", keyOf(event).String(), "panic", recovered)
					}
					if barrier != nil {
						close(barrier)
						m.mu.Lock()
						if m.controlBarriers[barrierKey] == barrier {
							delete(m.controlBarriers, barrierKey)
						}
						m.mu.Unlock()
					}
				}()
				if previousBarrier != nil {
					<-previousBarrier
				}
				if handler != nil {
					handler(event)
				}
			}()
		}
		return true
	}

	// A channel owns one FIFO worker. Keeping group members in separate workers
	// would serialize through the agent lock in nondeterministic mutex order.
	key := SessionKey{Platform: event.Platform, IsGroup: event.IsGroup, ChannelID: event.ChannelID}
	val, ok := m.workers.Load(key)
	var worker *sessionWorker
	if !ok {
		if val, ok = m.workers.Load(key); !ok {
			worker = &sessionWorker{
				manager:   m,
				key:       key,
				config:    m.config,
				handler:   m.handler,
				msgCh:     make(chan queuedEvent, 32),
				processCh: make(chan queuedEvent, 32),
				closeCh:   make(chan struct{}),
			}
			m.workers.Store(key, worker)
			m.wg.Add(2)
			go worker.processLoop()
			go worker.loop(func() bool {
				m.mu.Lock()
				defer m.mu.Unlock()
				if cur, ok := m.workers.Load(key); ok && cur == worker {
					if worker.pushers.Load() != 0 || worker.queued.Load() != 0 || len(worker.msgCh) != 0 {
						return false
					}
					worker.close()
					m.workers.Delete(key)
					return true
				}
				return true
			})
		} else {
			worker = val.(*sessionWorker)
		}
	} else {
		worker = val.(*sessionWorker)
	}
	queued := queuedEvent{event: event, generation: worker.generation.Load(), afterControl: m.controlBarriers[key]}
	worker.pushers.Add(1)
	m.mu.Unlock()

	accepted := false
	select {
	case worker.msgCh <- queued:
		accepted = true
	case <-worker.closeCh:
	default:
		slog.Warn("debounce: 会话输入队列已满，要求上游恢复投递", "session", key.String())
	}
	worker.pushers.Add(-1)
	return accepted
}

func invalidatesPendingTurns(content string) bool {
	switch strings.TrimSpace(content) {
	case "/stop", "/reset", "/new", "/clear", "/ws clear", "/ws reset":
		return true
	default:
		return false
	}
}

func requiresControlBarrier(content string) bool {
	switch strings.TrimSpace(content) {
	case "/reset", "/new", "/clear", "/ws clear", "/ws reset":
		return true
	default:
		return false
	}
}

func keyOf(event *core.SensoryEvent) SessionKey {
	if event == nil {
		return SessionKey{}
	}
	return SessionKey{Platform: event.Platform, IsGroup: event.IsGroup, ChannelID: event.ChannelID, SenderID: event.SenderID}
}

func (w *sessionWorker) dispatchQueued(event queuedEvent) {
	w.queued.Add(1)
	select {
	case w.processCh <- event:
	case <-w.closeCh:
		w.queued.Add(-1)
	}
}

func (w *sessionWorker) processLoop() {
	defer w.manager.wg.Done()
	for {
		// Shutdown must not start another queued turn after closeCh is closed.
		select {
		case <-w.closeCh:
			return
		default:
		}

		select {
		case <-w.closeCh:
			return
		case event := <-w.processCh:
			select {
			case <-w.closeCh:
				w.queued.Add(-1)
				return
			default:
			}
			if event.generation != w.generation.Load() {
				w.queued.Add(-1)
				continue
			}
			w.handle(event.event)
		}
	}
}

func (w *sessionWorker) handle(event *core.SensoryEvent) {
	defer func() {
		w.queued.Add(-1)
		if recovered := recover(); recovered != nil {
			slog.Error("debounce: session event handler panicked", "session", w.key.String(), "panic", recovered)
		}
	}()
	if w.handler != nil {
		w.handler(event)
	}
}

// Stop 停止所有防抖 Actor 协程
func (m *DebounceManager) Stop() {
	m.stop()
}

// Shutdown 收敛所有会话 Actor，并等待其调度协程退出或 ctx 超时。
func (m *DebounceManager) Shutdown(ctx context.Context) error {
	m.stop()

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *DebounceManager) stop() {
	m.mu.Lock()
	m.stopped = true
	m.workers.Range(func(key, value any) bool {
		if w, ok := value.(*sessionWorker); ok {
			w.close()
		}
		m.workers.Delete(key)
		return true
	})
	m.mu.Unlock()
}

// loop 单个用户的独立事件驱动循环
func (w *sessionWorker) loop(onIdleCleanup func() bool) {
	defer w.manager.wg.Done()
	var (
		pendingEvents []queuedEvent
		pendingRunes  int
		timer         *time.Timer
		timerCh       <-chan time.Time
	)

	idleTimer := time.NewTimer(w.config.IdleTimeout)
	defer idleTimer.Stop()

	for {
		select {
		case queued := <-w.msgCh:
			if queued.afterControl != nil {
				select {
				case <-queued.afterControl:
				case <-w.closeCh:
					return
				}
			}
			if queued.generation != w.generation.Load() {
				continue
			}
			ev := queued.event
			if len(pendingEvents) > 0 && pendingEvents[0].generation != queued.generation {
				pendingEvents = nil
				pendingRunes = 0
				if timer != nil {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer = nil
					timerCh = nil
				}
			}
			// A merged turn has one outgoing quote ID and one quoted context.
			// Flush before a different reply target so text is never attributed to
			// the wrong quoted message.
			if len(pendingEvents) > 0 && (pendingEvents[0].event.SenderID != ev.SenderID || !sameReplyContext(pendingEvents[0].event, ev)) {
				if timer != nil {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer = nil
					timerCh = nil
				}
				merged := mergeQueuedEvents(pendingEvents)
				pendingEvents = nil
				pendingRunes = 0
				w.dispatchQueued(merged)
			}

			eventRunes := utf8.RuneCountInString(ev.Content)
			if len(pendingEvents) > 0 && pendingRunes+eventRunes+1 > w.config.MaxMergedRunes {
				if timer != nil {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer = nil
					timerCh = nil
				}
				w.dispatchQueued(mergeQueuedEvents(pendingEvents))
				pendingEvents = nil
				pendingRunes = 0
			}

			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(w.config.IdleTimeout)

			pendingEvents = append(pendingEvents, queued)
			if pendingRunes > 0 {
				pendingRunes++
			}
			pendingRunes += eventRunes

			if timer == nil {
				timer = time.NewTimer(w.config.MergeWindow)
				timerCh = timer.C
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(w.config.MergeWindow)
			}
			if len(pendingEvents) >= maxMergedEventsPerTurn {
				if timer != nil {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer = nil
					timerCh = nil
				}
				w.dispatchQueued(mergeQueuedEvents(pendingEvents))
				pendingEvents = nil
				pendingRunes = 0
			}

		case <-timerCh:
			timer = nil
			timerCh = nil
			if len(pendingEvents) == 0 {
				continue
			}
			if pendingEvents[0].generation != w.generation.Load() {
				pendingEvents = nil
				continue
			}

			merged := mergeQueuedEvents(pendingEvents)
			pendingEvents = nil
			pendingRunes = 0
			w.dispatchQueued(merged)

		case <-idleTimer.C:
			// Push completes its send before decrementing pushers, but a queued
			// message can remain in msgCh after that decrement. Do not retire the
			// actor while the input buffer still contains work.
			if len(pendingEvents) == 0 && len(w.msgCh) == 0 && w.queued.Load() == 0 && w.pushers.Load() == 0 {
				if onIdleCleanup() {
					return
				}
			}
			idleTimer.Reset(w.config.IdleTimeout)

		case <-w.closeCh:
			return
		}
	}
}

func mergeQueuedEvents(events []queuedEvent) queuedEvent {
	if len(events) == 0 {
		return queuedEvent{}
	}
	plain := make([]*core.SensoryEvent, 0, len(events))
	for _, event := range events {
		plain = append(plain, event.event)
	}
	return queuedEvent{event: mergeEvents(plain), generation: events[len(events)-1].generation}
}

func sameReplyContext(a, b *core.SensoryEvent) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.ReplyContext == nil || b.ReplyContext == nil {
		return a.ReplyContext == nil && b.ReplyContext == nil
	}
	return strings.TrimSpace(a.ReplyContext.Content) == strings.TrimSpace(b.ReplyContext.Content) &&
		a.ReplyContext.RawMessageID == b.ReplyContext.RawMessageID
}

// mergeEvents 将同一个人在窗口内的多条感知事件合并为一条事件
func mergeEvents(events []*core.SensoryEvent) *core.SensoryEvent {
	if len(events) == 1 {
		return events[0]
	}
	latest := events[len(events)-1]
	merged := &core.SensoryEvent{
		Platform:     latest.Platform,
		IsGroup:      latest.IsGroup,
		ChannelID:    latest.ChannelID,
		SenderID:     latest.SenderID,
		SenderName:   latest.SenderName,
		Role:         latest.Role,
		RawMessageID: latest.RawMessageID,
		OccurredAt:   latest.OccurredAt,
		ReplyContext: latest.ReplyContext,
	}

	var textParts []string
	var allImages []*core.Attachment

	for _, ev := range events {
		if ev.Content != "" {
			textParts = append(textParts, ev.Content)
		}
		if ev.ReplyContext != nil {
			merged.ReplyContext = ev.ReplyContext
		}
		allImages = append(allImages, ev.Images...)
	}

	merged.Content = strings.Join(textParts, "\n")
	merged.Images = allImages
	return merged
}
