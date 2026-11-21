package entity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"daidai/internal/db"
)

const groupInterjectionProcessingTimeout = 45 * time.Second

// ChannelRegistry 全双工通道管理器
type SuperAgent struct {
	Persona                    *Persona
	Immune                     *ImmuneSystem
	Cognition                  Cognition
	Delegates                  *Delegates
	Channels                   *ChannelRegistry
	Workspaces                 *WorkspaceManager
	AGYAuth                    AGYAuthenticator
	database                   *db.Manager
	OnAuthSuccess              func(ctx context.Context)
	OnPrivateInteraction       func(context.Context, *SensoryEvent)
	OnSessionReset             func(context.Context, *SensoryEvent) error
	OnStop                     func(context.Context, *SensoryEvent) bool
	pendingNotificationLocksMu sync.Mutex
	pendingNotificationLocks   map[pendingNotificationKey]*sessionLockEntry
	continuationMu             sync.Mutex
	continuations              map[replyContinuationKey]pendingReplyContinuation
	sessionLocksMu             sync.Mutex
	sessionLocks               map[string]*sessionLockEntry // 引用计数覆盖持锁者和等待者，空闲后删除会话锁
	sessionCancels             sync.Map                     // key: *thinkCancellation -> context.CancelFunc，支持按发起人停止并发思考
	lifecycleMu                sync.Mutex
	runCancel                  context.CancelFunc
	stopOp                     *agentStopOperation
}

type agentStopOperation struct {
	done chan struct{}
	err  error
}

type thinkSessionKey struct {
	session   string
	requester string
}

type thinkCancellation struct {
	key thinkSessionKey
}

type replyContinuationKey struct {
	thinkSessionKey
	draftID string
}

type pendingReplyContinuation struct {
	queuedAt    time.Time
	draftID     string
	text        string
	updatedAt   time.Time
	onDelivered []func(context.Context)
}

type pendingNotificationKey struct {
	platform    string
	recipientID string
}

type sessionLockEntry struct {
	mu   sync.Mutex
	refs int
}

func sessionKey(event *SensoryEvent) string {
	return fmt.Sprintf("%s:%v:%s", event.Platform, event.IsGroup, event.ChannelID)
}

func thinkKey(event *SensoryEvent) thinkSessionKey {
	return thinkSessionKey{session: sessionKey(event), requester: event.SenderID}
}

func (a *SuperAgent) cancelThinkingSession(event *SensoryEvent) {
	if event == nil {
		return
	}
	key := sessionKey(event)
	a.sessionCancels.Range(func(rawKey, rawValue any) bool {
		entry, ok := rawKey.(*thinkCancellation)
		if !ok || entry.key.session != key {
			return true
		}
		if cancelValue, loaded := a.sessionCancels.LoadAndDelete(entry); loaded {
			if cancel, ok := cancelValue.(context.CancelFunc); ok && cancel != nil {
				cancel()
			}
		}
		return true
	})
}

func (a *SuperAgent) cancelThinkingRequester(event *SensoryEvent) bool {
	if event == nil {
		return false
	}
	key := thinkKey(event)
	canceled := false
	a.sessionCancels.Range(func(rawKey, rawValue any) bool {
		entry, ok := rawKey.(*thinkCancellation)
		if !ok || entry.key != key {
			return true
		}
		if cancelValue, loaded := a.sessionCancels.LoadAndDelete(entry); loaded {
			if cancel, ok := cancelValue.(context.CancelFunc); ok && cancel != nil {
				cancel()
				canceled = true
			}
		}
		return true
	})
	return canceled
}

func (a *SuperAgent) cancelAllThinking() {
	a.sessionCancels.Range(func(rawKey, _ any) bool {
		entry, ok := rawKey.(*thinkCancellation)
		if !ok {
			return true
		}
		if cancelValue, loaded := a.sessionCancels.LoadAndDelete(entry); loaded {
			if cancel, ok := cancelValue.(context.CancelFunc); ok && cancel != nil {
				cancel()
			}
		}
		return true
	})
}

func (a *SuperAgent) lockSession(event *SensoryEvent) func() {
	key := sessionKey(event)
	a.sessionLocksMu.Lock()
	if a.sessionLocks == nil {
		a.sessionLocks = make(map[string]*sessionLockEntry)
	}
	entry := a.sessionLocks[key]
	if entry == nil {
		entry = &sessionLockEntry{}
		a.sessionLocks[key] = entry
	}
	entry.refs++
	a.sessionLocksMu.Unlock()

	entry.mu.Lock()
	return func() {
		a.sessionLocksMu.Lock()
		entry.mu.Unlock()
		entry.refs--
		if entry.refs == 0 {
			delete(a.sessionLocks, key)
		}
		a.sessionLocksMu.Unlock()
	}
}

func (a *SuperAgent) lockPendingNotification(platform, recipientID string) func() {
	key := pendingNotificationKey{platform: platform, recipientID: recipientID}
	a.pendingNotificationLocksMu.Lock()
	if a.pendingNotificationLocks == nil {
		a.pendingNotificationLocks = make(map[pendingNotificationKey]*sessionLockEntry)
	}
	entry := a.pendingNotificationLocks[key]
	if entry == nil {
		entry = &sessionLockEntry{}
		a.pendingNotificationLocks[key] = entry
	}
	entry.refs++
	a.pendingNotificationLocksMu.Unlock()

	entry.mu.Lock()
	return func() {
		a.pendingNotificationLocksMu.Lock()
		entry.mu.Unlock()
		entry.refs--
		if entry.refs == 0 && a.pendingNotificationLocks[key] == entry {
			delete(a.pendingNotificationLocks, key)
		}
		a.pendingNotificationLocksMu.Unlock()
	}
}

// Config 生命体初始化依赖
type Config struct {
	Database             *db.Manager
	PartnerID            string
	Persona              *Persona
	Immune               *ImmuneSystem
	Cognition            Cognition
	Delegates            *Delegates
	Channels             *ChannelRegistry
	Workspaces           *WorkspaceManager
	AGYAuth              AGYAuthenticator
	OnAuthSuccess        func(ctx context.Context)
	OnPrivateInteraction func(context.Context, *SensoryEvent)
	OnSessionReset       func(context.Context, *SensoryEvent) error
	OnStop               func(context.Context, *SensoryEvent) bool
}

// NewSuperAgent 组装生命体核心聚合根
func NewSuperAgent(cfg Config) *SuperAgent {
	persona := cfg.Persona
	if persona == nil {
		persona = NewDefaultPersona()
	}
	immune := cfg.Immune
	if immune == nil {
		immune = NewImmuneSystem(cfg.PartnerID)
	}
	channels := cfg.Channels
	if channels == nil {
		channels = NewChannelRegistry()
	}

	return &SuperAgent{
		Persona:              persona,
		Immune:               immune,
		Cognition:            cfg.Cognition,
		Delegates:            cfg.Delegates,
		Channels:             channels,
		Workspaces:           cfg.Workspaces,
		AGYAuth:              cfg.AGYAuth,
		database:             cfg.Database,
		OnAuthSuccess:        cfg.OnAuthSuccess,
		OnPrivateInteraction: cfg.OnPrivateInteraction,
		OnSessionReset:       cfg.OnSessionReset,
		OnStop:               cfg.OnStop,
	}
}

func (a *SuperAgent) databaseManager() *db.Manager {
	if a.database != nil {
		return a.database
	}
	return db.DefaultManager()
}

// Start 唤醒生命体并启动所有物理通信通道
func (a *SuperAgent) Start(ctx context.Context) error {
	if a.Channels == nil {
		return fmt.Errorf("channel registry is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	if a.runCancel != nil || a.stopOp != nil {
		return fmt.Errorf("SuperAgent is already running")
	}
	runCtx, runCancel := context.WithCancel(ctx)
	a.runCancel = runCancel
	name := ""
	if a.Persona != nil {
		name = a.Persona.Name
	}
	slog.Info("Daidai SuperAgent 正在苏醒...", "name", name)
	if a.Delegates != nil {
		if err := a.Delegates.StartQueue(runCtx, a.notifyTicketCompletion); err != nil {
			runCancel()
			a.runCancel = nil
			return err
		}
	}

	if err := a.Channels.StartAll(runCtx, func(event *SensoryEvent) {
		procTimeout := a.processingTimeout()
		if event != nil && event.GroupInterjection {
			procTimeout = groupInterjectionProcessingTimeout
		}
		var procCtx context.Context
		var cancel context.CancelFunc
		if procTimeout > 0 {
			procCtx, cancel = context.WithTimeout(runCtx, procTimeout)
		} else {
			procCtx, cancel = context.WithCancel(runCtx)
		}
		defer cancel()
		a.Process(procCtx, event)
	}); err != nil {
		runCancel()
		var queueStopErr error
		if a.Delegates != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			queueStopErr = a.Delegates.StopQueue(cleanupCtx)
			cleanupCancel()
			if queueStopErr != nil {
				slog.Warn("superagent: 通道启动失败后工单队列未能在期限内停止", "err", queueStopErr)
			}
		}
		var startupErr interface{ rollbackFailed() bool }
		rollbackFailed := errors.As(err, &startupErr) && startupErr.rollbackFailed()
		if !rollbackFailed && queueStopErr == nil {
			a.runCancel = nil
		}
		if queueStopErr != nil {
			err = errors.Join(err, fmt.Errorf("stop agent ticket queue after channel startup failure: %w", queueStopErr))
		}
		return err
	}
	return nil
}

func (a *SuperAgent) processingTimeout() time.Duration {
	return 6 * time.Minute
}

// Stop 优雅休眠生命体
func (a *SuperAgent) Stop(ctx context.Context) error {
	slog.Info("Daidai SuperAgent 正在休眠...")
	if ctx == nil {
		ctx = context.Background()
	}
	a.lifecycleMu.Lock()
	if previous := a.stopOp; previous != nil {
		a.lifecycleMu.Unlock()
		select {
		case <-previous.done:
			return previous.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	operation := &agentStopOperation{done: make(chan struct{})}
	a.stopOp = operation
	runCancel := a.runCancel
	a.lifecycleMu.Unlock()

	if a.AGYAuth != nil {
		a.AGYAuth.Cancel()
	}
	if runCancel != nil {
		runCancel()
	}
	a.cancelAllThinking()
	if a.Delegates != nil {
		a.Delegates.AbortAllContext(ctx)
	}
	var stopErr error
	if a.Delegates != nil {
		stopErr = a.Delegates.StopQueue(ctx)
	}
	if a.Channels != nil {
		stopErr = errors.Join(stopErr, a.Channels.StopAll(ctx))
	}
	a.lifecycleMu.Lock()
	if stopErr == nil {
		a.runCancel = nil
	}
	operation.err = stopErr
	close(operation.done)
	a.stopOp = nil
	a.lifecycleMu.Unlock()
	return stopErr
}
