package qq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"daidai/internal/db"
	"daidai/internal/entity"
	"daidai/internal/kernel"
	"daidai/internal/media"
)

// Config 官方 QQ 通道配置 (WebSocket + OpenAPI v2)
type Config struct {
	AppID       string
	Secret      string
	OwnerOpenID string
	BotName     string
	AGYCommand  string
}

const (
	// VoiceModeMulti 优先用多模态模型转录原音，失败时回退 QQ 提供的转录文本。
	VoiceModeMulti = "multi"
	// VoiceModeFast 优先使用 QQ 已提供的转录文本，缺失时调用多模态转录。
	VoiceModeFast = "fast"
)

type qqOutgoingPart struct {
	text   string
	image  []byte
	source string
}

type pendingGroupInterjection struct {
	event     *entity.SensoryEvent
	timer     *time.Timer
	scheduled time.Time
}

type groupBotMention struct {
	ID   string
	Name string
}

const maxQQOutgoingParts = 5
const groupInterjectionCooldown = 5 * time.Minute
const groupInterjectionQuietPeriod = 4 * time.Second

type QQChannel struct {
	lifecycleMu         sync.Mutex
	markdownImageMu     sync.Mutex
	proactivePrivate    atomic.Pointer[sendObservation]
	proactiveGroup      atomic.Pointer[sendObservation]
	cfg                 Config
	client              *Client
	debounce            *kernel.DebounceManager
	immune              *entity.ImmuneSystem
	msgCache            *msgCache
	dedup               *dedupCache
	inbox               *media.Inbox
	database            *db.Manager
	stopCancel          context.CancelFunc
	wsWG                sync.WaitGroup
	lifecycleGeneration atomic.Uint64

	wsMu        sync.Mutex
	wsSessionID string
	wsLastSeq   int64
	wsReady     atomic.Bool

	replyBudgetsMu           sync.Mutex
	replyBudgets             map[string]*passiveReplyBudget
	replyBudgetSweepAt       time.Time
	groupInterjectionsMu     sync.Mutex
	groupInterjections       map[string]time.Time
	groupInterjectionSweepAt time.Time
	pendingInterjectionsMu   sync.Mutex
	pendingInterjections     map[string]*pendingGroupInterjection
	knownGroupBotsMu         sync.RWMutex
	knownGroupBots           map[string]map[string]string
	historyRecorder          func(context.Context, *entity.SensoryEvent) error
	voiceModeMu              sync.RWMutex
	voiceMode                string
	groupActiveSessionsMu    sync.Mutex
	groupActiveSessions      map[string]*groupActiveSession
}

// SetHistoryRecorder installs the callback used to persist each raw inbound
// event before debounce can merge it with neighboring messages.
func (q *QQChannel) SetHistoryRecorder(recorder func(context.Context, *entity.SensoryEvent) error) {
	if q != nil {
		q.historyRecorder = recorder
	}
}

func (q *QQChannel) getWSSession() (string, int64) {
	q.wsMu.Lock()
	defer q.wsMu.Unlock()
	return q.wsSessionID, q.wsLastSeq
}

func (q *QQChannel) setWSSessionID(sid string) {
	q.wsMu.Lock()
	defer q.wsMu.Unlock()
	q.wsSessionID = sid
}

func (q *QQChannel) setWSLastSeq(seq int64) {
	q.wsMu.Lock()
	defer q.wsMu.Unlock()
	q.wsLastSeq = seq
}

func (q *QQChannel) clearWSSession() {
	q.wsMu.Lock()
	defer q.wsMu.Unlock()
	q.wsSessionID = ""
	q.wsLastSeq = 0
}

// NewQQChannel 实例化 QQ 全双工通道
func NewQQChannel(cfg Config, database *db.Manager, immune *entity.ImmuneSystem) *QQChannel {
	cache := newMsgCache(1000, database)
	cli := NewClient(cfg.AppID, cfg.Secret)
	channel := &QQChannel{
		cfg:                 cfg,
		client:              cli,
		immune:              immune,
		msgCache:            cache,
		dedup:               newDedupCache(2000),
		database:            database,
		groupActiveSessions: make(map[string]*groupActiveSession),
	}
	cli.SetOnMessageSent(func(msgID, refIndex, targetType, targetID string, metadata QuoteMetadata) {
		// If QQ reflects an outbound group message through GROUP_MESSAGE_CREATE,
		// its accepted send ID lets the inbound path drop that exact echo.
		if channel.dedup != nil {
			channel.dedup.Remember(msgID)
		}
		chatType := "private"
		if targetType == "groups" {
			chatType = "group"
		}
		cache.SetTypedAsync(msgID, metadata.Text, chatType, targetID, metadata.Kind, metadata.TextComplete)
		if refIndex != "" && refIndex != msgID {
			cache.SetTypedAsync(refIndex, metadata.Text, chatType, targetID, metadata.Kind, metadata.TextComplete)
		}
	})
	return channel
}

func (q *QQChannel) Name() string {
	return "qq"
}

// Ready reports whether the QQ gateway has completed READY or RESUMED.
func (q *QQChannel) Ready() bool {
	return q.wsReady.Load()
}

// SetAttachmentInbox enables short-lived, metadata-only attachment follow-ups.
func (q *QQChannel) SetAttachmentInbox(inbox *media.Inbox) {
	q.inbox = inbox
}

// GetVoiceMode 返回当前的语音识别模式 (fast / multi)
func (q *QQChannel) GetVoiceMode() string {
	if q == nil {
		return VoiceModeMulti
	}
	q.voiceModeMu.RLock()
	defer q.voiceModeMu.RUnlock()
	if q.voiceMode == "" {
		return VoiceModeMulti
	}
	return q.voiceMode
}

// SetVoiceMode 设置并持久化语音识别模式
func (q *QQChannel) SetVoiceMode(mode string) error {
	if q == nil {
		return errors.New("nil channel")
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != VoiceModeFast && mode != VoiceModeMulti {
		return fmt.Errorf("unknown voice mode: %s (expected %s or %s)", mode, VoiceModeFast, VoiceModeMulti)
	}
	q.voiceModeMu.Lock()
	q.voiceMode = mode
	q.voiceModeMu.Unlock()

	if q.database == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := q.database.SetRuntimeState(ctx, "qq", "voice_mode", mode); err != nil {
		slog.Warn("qq: failed to persist voice mode to runtime_state", "err", err)
		return err
	}
	return nil
}

func (q *QQChannel) Start(ctx context.Context, onSensory func(event *entity.SensoryEvent)) error {
	q.lifecycleMu.Lock()
	defer q.lifecycleMu.Unlock()
	if q.stopCancel != nil {
		return fmt.Errorf("qq channel already started")
	}
	q.wsReady.Store(false)
	loadBotsCtx, loadBotsCancel := context.WithTimeout(ctx, 3*time.Second)
	if err := q.loadKnownGroupBots(loadBotsCtx); err != nil {
		slog.Warn("qq_channel: 已标记的群机器人清单读取失败", "err", err)
	}
	loadBotsCancel()

	var storedVoiceMode string
	var voiceErr error
	if q.database != nil {
		loadVoiceCtx, loadVoiceCancel := context.WithTimeout(ctx, 3*time.Second)
		storedVoiceMode, voiceErr = q.database.GetRuntimeState(loadVoiceCtx, "qq", "voice_mode")
		loadVoiceCancel()
	}
	if q.database != nil && voiceErr == nil && (storedVoiceMode == VoiceModeFast || storedVoiceMode == VoiceModeMulti) {
		q.voiceModeMu.Lock()
		q.voiceMode = storedVoiceMode
		q.voiceModeMu.Unlock()
		slog.Info("qq: 已加载持久化语音识别模式", "mode", storedVoiceMode)
	} else {
		q.voiceModeMu.Lock()
		if q.voiceMode == "" {
			q.voiceMode = VoiceModeMulti
		}
		q.voiceModeMu.Unlock()
	}
	chCtx, cancel := context.WithCancel(ctx)
	if q.cfg.AppID != "" && q.cfg.Secret != "" && q.client != nil {
		if err := q.client.StartTokenRefresher(chCtx); err != nil {
			cancel()
			return err
		}
	}
	if q.msgCache != nil {
		q.msgCache.Reopen()
	}
	q.groupInterjectionsMu.Lock()
	q.lifecycleGeneration.Add(1)
	q.groupInterjectionsMu.Unlock()
	q.stopCancel = cancel

	q.debounce = kernel.NewDebounceManager(kernel.DebounceConfig{
		MergeWindow: 2 * time.Second,
	}, func(event *entity.SensoryEvent) {
		if event == nil || onSensory == nil {
			return
		}
		event.Content = strings.TrimSpace(event.Content)
		if event.Content == "" && event.HistoryAttachmentNote == "" && len(event.Images) == 0 && event.OmittedImages == 0 && len(event.Files) == 0 && event.OmittedFiles == 0 &&
			len(event.PendingImages) == 0 && event.OmittedPendingImages == 0 && len(event.PendingFiles) == 0 && event.OmittedPendingFiles == 0 {
			return
		}
		onSensory(event)
	})

	if q.cfg.AppID != "" && q.cfg.Secret != "" && q.client != nil {
		q.StartWebSocket(chCtx)
		slog.Info("已挂载 QQ 官方物理神经 (WebSocket + OpenAPI v2)")
	}

	return nil
}

// IngestRaw 接收解析后的感知事件并推入防抖队列
func (q *QQChannel) IngestRaw(event *entity.SensoryEvent) bool {
	if event == nil {
		return false
	}
	if q.debounce == nil {
		return false
	}
	if q.historyRecorder != nil {
		recordCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := q.historyRecorder(recordCtx, event); err != nil {
			cancel()
			slog.Warn("qq_channel: 保存入站消息历史失败", "channelID", event.ChannelID, "msgID", event.RawMessageID, "err", err)
			return false
		}
		cancel()
	}
	return q.debounce.Push(event)
}

func qqMessageMentionsBot(eventData map[string]any) bool {
	mentions, ok := eventData["mentions"].([]any)
	if !ok {
		return false
	}
	for _, rawMention := range mentions {
		mention, ok := rawMention.(map[string]any)
		if !ok {
			continue
		}
		if isYou, ok := mention["is_you"].(bool); ok && isYou {
			return true
		}
	}
	return false
}

func qqMessageAuthorIsBot(eventData map[string]any) bool {
	author, ok := eventData["author"].(map[string]any)
	if !ok {
		return false
	}
	for _, key := range []string{"bot", "is_bot"} {
		if isBot, ok := author[key].(bool); ok && isBot {
			return true
		}
	}
	return false
}

func (q *QQChannel) Stop(ctx context.Context) error {
	q.lifecycleMu.Lock()
	defer q.lifecycleMu.Unlock()

	q.groupInterjectionsMu.Lock()
	q.lifecycleGeneration.Add(1)
	q.groupInterjectionsMu.Unlock()
	q.wsReady.Store(false)
	if q.stopCancel != nil {
		q.stopCancel()
	}
	wsDone := make(chan struct{})
	go func() {
		q.wsWG.Wait()
		close(wsDone)
	}()
	var firstErr error
	select {
	case <-wsDone:
	case <-ctx.Done():
		firstErr = ctx.Err()
	}
	q.pendingInterjectionsMu.Lock()
	for groupID, pending := range q.pendingInterjections {
		pending.timer.Stop()
		delete(q.pendingInterjections, groupID)
	}
	q.pendingInterjectionsMu.Unlock()
	q.groupActiveSessionsMu.Lock()
	q.groupActiveSessions = nil
	q.groupActiveSessionsMu.Unlock()
	if q.debounce != nil {
		if err := q.debounce.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if q.msgCache != nil {
		if err := q.msgCache.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		q.stopCancel = nil
	}
	return firstErr
}
