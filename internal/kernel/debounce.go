package kernel

import (
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	event            *core.SensoryEvent
	generation       uint64
	senderGeneration uint64
	afterControl     <-chan struct{}
}

const maxMergedEventsPerTurn = 64

const (
	maxConcurrentControlEvents = 64
	maxConcurrentStopEvents    = 8
)

// DebounceConfig 防抖配置
type DebounceConfig struct {
	MergeWindow    time.Duration // 连续发言合并窗口，默认 2 秒
	IdleTimeout    time.Duration // 空闲回收时间，默认 5 分钟
	MaxMergedRunes int           // 合并事件的字符上限，默认沿用用户消息上限
}

// sessionWorker 单个会话的 Actor 调度器
type sessionWorker struct {
	manager           *DebounceManager
	key               SessionKey
	config            DebounceConfig
	handler           SessionHandler
	msgCh             chan queuedEvent
	processCh         chan queuedEvent
	queued            atomic.Int32
	pushers           atomic.Int32
	generation        atomic.Uint64
	senderGenerations map[string]uint64 // accessed under manager.mu; bounded by actor lifetime
	closeCh           chan struct{}
	closeOnce         sync.Once
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
	controlSlots    chan struct{}
	stopSlots       chan struct{}
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
		config:       cfg,
		handler:      handler,
		controlSlots: make(chan struct{}, maxConcurrentControlEvents),
		stopSlots:    make(chan struct{}, maxConcurrentStopEvents),
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
	// 已识别的控制指令零延迟直通，避免被防抖窗口吞并；同条消息上的附件仍只保留待确认元数据。
	// 未知斜杠文本仍走普通会话 FIFO。
	if isImmediateControlEvent(event.Content) {
		trimmedCommand := strings.TrimSpace(event.Content)
		controlSlots := m.controlSlots
		if isStopCommand(trimmedCommand) {
			controlSlots = m.stopSlots
		}
		select {
		case controlSlots <- struct{}{}:
		default:
			m.mu.Unlock()
			slog.Warn("debounce: 即时控制事件处理已满，要求上游恢复投递", "command", controlCommandName(trimmedCommand))
			return false
		}
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
		if invalidatesPendingSenderTurns(event.Content) && (event.Role == core.RolePartner || event.Role == core.RoleCollaborator) {
			m.workers.Range(func(_, value any) bool {
				worker, ok := value.(*sessionWorker)
				if ok && worker.key.Platform == event.Platform && worker.key.IsGroup == event.IsGroup && worker.key.ChannelID == event.ChannelID &&
					(worker.key.SenderID == "" || worker.key.SenderID == event.SenderID) {
					if worker.senderGenerations == nil {
						worker.senderGenerations = make(map[string]uint64)
					}
					worker.senderGenerations[event.SenderID]++
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
		} else {
			<-controlSlots
		}
		m.mu.Unlock()
		if handler != nil || barrier != nil {
			go func() {
				defer m.wg.Done()
				defer func() {
					defer func() { <-controlSlots }()
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

	// Isolate group turns per speaker (matching SuperAgent's per-speaker lock) so
	// one member's long tool turn never blocks other group members or observed
	// history ingestion. Group interjections and private chats stay channel-scoped.
	key := workerKeyFor(event)
	barrierKey := SessionKey{Platform: event.Platform, IsGroup: event.IsGroup, ChannelID: event.ChannelID}
	val, ok := m.workers.Load(key)
	var worker *sessionWorker
	if !ok {
		if val, ok = m.workers.Load(key); !ok {
			worker = &sessionWorker{
				manager:           m,
				key:               key,
				config:            m.config,
				handler:           m.handler,
				msgCh:             make(chan queuedEvent, 32),
				processCh:         make(chan queuedEvent, 32),
				senderGenerations: make(map[string]uint64),
				closeCh:           make(chan struct{}),
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
	queued := queuedEvent{event: event, generation: worker.generation.Load(), senderGeneration: worker.senderGenerations[event.SenderID], afterControl: m.controlBarriers[barrierKey]}
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

func workerKeyFor(event *core.SensoryEvent) SessionKey {
	if event == nil {
		return SessionKey{}
	}
	if event.IsGroup && event.SenderID != "" && !event.GroupInterjection {
		return SessionKey{Platform: event.Platform, IsGroup: true, ChannelID: event.ChannelID, SenderID: event.SenderID}
	}
	return SessionKey{Platform: event.Platform, IsGroup: event.IsGroup, ChannelID: event.ChannelID}
}

func keyOf(event *core.SensoryEvent) SessionKey {
	if event == nil {
		return SessionKey{}
	}
	return SessionKey{Platform: event.Platform, IsGroup: event.IsGroup, ChannelID: event.ChannelID, SenderID: event.SenderID}
}
