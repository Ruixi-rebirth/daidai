package kernel

import (
	"strings"
	"sync"
	"time"

	"daidai/internal/core"
)

// SessionKey 标识会话隔离键 (群私聊类型:会话ID:用户ID)
type SessionKey struct {
	IsGroup   bool
	ChannelID string
	SenderID  string
}

func (k SessionKey) String() string {
	if k.IsGroup {
		return "g:" + k.ChannelID + ":" + k.SenderID
	}
	return "p:" + k.ChannelID + ":" + k.SenderID
}

// SessionHandler 窗口合并后的处理回调
type SessionHandler func(mergedEvent *core.SensoryEvent)

// DebounceConfig 防抖配置
type DebounceConfig struct {
	MergeWindow time.Duration // 连续发言合并窗口，默认 2 秒
	IdleTimeout time.Duration // 空闲回收时间，默认 5 分钟
}

// sessionWorker 单个会话的 Actor 调度器
type sessionWorker struct {
	key       SessionKey
	config    DebounceConfig
	handler   SessionHandler
	msgCh     chan *core.SensoryEvent
	closeCh   chan struct{}
	closeOnce sync.Once
}

func (w *sessionWorker) close() {
	w.closeOnce.Do(func() {
		close(w.closeCh)
	})
}

// DebounceManager 负责并发会话防抖与 Actor 调度
type DebounceManager struct {
	config  DebounceConfig
	handler SessionHandler
	workers sync.Map // map[string]*sessionWorker
	mu      sync.Mutex
}

// NewDebounceManager 实例化防抖管理器
func NewDebounceManager(cfg DebounceConfig, handler SessionHandler) *DebounceManager {
	if cfg.MergeWindow <= 0 {
		cfg.MergeWindow = 2 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 5 * time.Minute
	}
	return &DebounceManager{
		config:  cfg,
		handler: handler,
	}
}

// Push 收到新感知事件，控制面指令直通分发，普通对话投递到对应用户的防抖管道
func (m *DebounceManager) Push(event *core.SensoryEvent) {
	if event == nil {
		return
	}
	// 以 "/" 开头的控制指令（如 /stop, /status, /help）零延迟直通，不进入合并窗口以免被前序普通文本吞没
	if strings.HasPrefix(strings.TrimSpace(event.Content), "/") && len(event.Images) == 0 {
		if m.handler != nil {
			go m.handler(event)
		}
		return
	}

	key := SessionKey{IsGroup: event.IsGroup, ChannelID: event.ChannelID, SenderID: event.SenderID}
	kStr := key.String()

	val, ok := m.workers.Load(kStr)
	var worker *sessionWorker
	if !ok {
		m.mu.Lock()
		if val, ok = m.workers.Load(kStr); !ok {
			worker = &sessionWorker{
				key:     key,
				config:  m.config,
				handler: m.handler,
				msgCh:   make(chan *core.SensoryEvent, 32),
				closeCh: make(chan struct{}),
			}
			m.workers.Store(kStr, worker)
			go worker.loop(func() {
				m.mu.Lock()
				if cur, ok := m.workers.Load(kStr); ok && cur == worker {
					m.workers.Delete(kStr)
				}
				m.mu.Unlock()
			})
		} else {
			worker = val.(*sessionWorker)
		}
		m.mu.Unlock()
	} else {
		worker = val.(*sessionWorker)
	}

	select {
	case worker.msgCh <- event:
	default:
	}
}

// Stop 停止所有防抖 Actor 协程
func (m *DebounceManager) Stop() {
	m.workers.Range(func(key, value any) bool {
		if w, ok := value.(*sessionWorker); ok {
			w.close()
		}
		m.workers.Delete(key)
		return true
	})
}

// loop 单个用户的独立事件驱动循环
func (w *sessionWorker) loop(onIdleCleanup func()) {
	var (
		pendingEvents []*core.SensoryEvent
		timer         *time.Timer
		timerCh       <-chan time.Time
	)

	idleTimer := time.NewTimer(w.config.IdleTimeout)
	defer idleTimer.Stop()

	for {
		select {
		case ev := <-w.msgCh:
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(w.config.IdleTimeout)

			pendingEvents = append(pendingEvents, ev)

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

		case <-timerCh:
			timer = nil
			timerCh = nil
			if len(pendingEvents) == 0 {
				continue
			}

			merged := mergeEvents(pendingEvents)
			pendingEvents = nil

			if w.handler != nil {
				go w.handler(merged)
			}

		case <-idleTimer.C:
			if len(pendingEvents) == 0 {
				onIdleCleanup()
				return
			}
			idleTimer.Reset(w.config.IdleTimeout)

		case <-w.closeCh:
			return
		}
	}
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
