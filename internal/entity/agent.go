package entity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
)

const groupInterjectionProcessingTimeout = 45 * time.Second

// ChannelRegistry 全双工通道管理器
type ChannelRegistry struct {
	mu          sync.RWMutex
	lifecycleMu sync.Mutex
	channels    map[string]Channel
}

type channelStartError struct {
	startErr    error
	rollbackErr error
}

func (e *channelStartError) Error() string {
	if e.rollbackErr == nil {
		return e.startErr.Error()
	}
	return fmt.Sprintf("%v; channel startup rollback incomplete: %v", e.startErr, e.rollbackErr)
}

func (e *channelStartError) Unwrap() error { return e.startErr }

func (e *channelStartError) rollbackFailed() bool { return e.rollbackErr != nil }

func NewChannelRegistry() *ChannelRegistry {
	return &ChannelRegistry{
		channels: make(map[string]Channel),
	}
}

func (r *ChannelRegistry) Register(ch Channel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channels[ch.Name()] = ch
}

func (r *ChannelRegistry) Get(name string) (Channel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ch, ok := r.channels[name]
	return ch, ok
}

func (r *ChannelRegistry) FormatStatus() string {
	r.mu.RLock()
	names := make([]string, 0, len(r.channels))
	channels := make(map[string]Channel, len(r.channels))
	for name, channel := range r.channels {
		names = append(names, name)
		channels[name] = channel
	}
	r.mu.RUnlock()
	sort.Strings(names)
	if len(names) == 0 {
		return "### 消息通道\n- 未挂载任何消息通道"
	}
	var lines []string
	for _, name := range names {
		state := "连接状态未知"
		if probe, ok := channels[name].(interface{ Ready() bool }); ok {
			if probe.Ready() {
				state = "已连接"
			} else {
				state = "未连接"
			}
		}
		if name == "qq" {
			lines = append(lines, fmt.Sprintf("- QQ 收消息网关（WebSocket）：%s；主动发送接口状态未探测", state))
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s：%s", name, state))
	}
	return "### 消息通道\n" + strings.Join(lines, "\n")
}

func (r *ChannelRegistry) StartAll(ctx context.Context, onSensory func(event *SensoryEvent)) error {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}

	r.mu.RLock()
	names := make([]string, 0, len(r.channels))
	for name := range r.channels {
		names = append(names, name)
	}
	sort.Strings(names)
	channels := make([]Channel, len(names))
	for i, name := range names {
		channels[i] = r.channels[name]
	}
	r.mu.RUnlock()

	started := make([]Channel, 0, len(names))
	rollback := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var rollbackErrs []error
		for i := len(started) - 1; i >= 0; i-- {
			if stopErr := started[i].Stop(cleanupCtx); stopErr != nil {
				slog.Warn("channel: 启动失败后的回滚停止未完全成功", "name", started[i].Name(), "err", stopErr)
				rollbackErrs = append(rollbackErrs, fmt.Errorf("stop channel %s: %w", started[i].Name(), stopErr))
			}
		}
		return errors.Join(rollbackErrs...)
	}
	for i, name := range names {
		if err := ctx.Err(); err != nil {
			return &channelStartError{startErr: fmt.Errorf("channel startup canceled: %w", err), rollbackErr: rollback()}
		}
		ch := channels[i]
		slog.Info("channel: 正在启动物理通道", "name", name)
		started = append(started, ch)
		if err := ch.Start(ctx, onSensory); err != nil {
			return &channelStartError{startErr: fmt.Errorf("failed to start channel %s: %w", name, err), rollbackErr: rollback()}
		}
	}
	if err := ctx.Err(); err != nil {
		return &channelStartError{startErr: fmt.Errorf("channel startup canceled: %w", err), rollbackErr: rollback()}
	}
	return nil
}

func (r *ChannelRegistry) StopAll(ctx context.Context) error {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}

	r.mu.RLock()
	var firstErr error
	names := make([]string, 0, len(r.channels))
	for name := range r.channels {
		names = append(names, name)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	channels := make([]Channel, len(names))
	for i, name := range names {
		channels[i] = r.channels[name]
	}
	r.mu.RUnlock()
	for i := range names {
		ch := channels[i]
		if err := ch.Stop(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *ChannelRegistry) Transmit(ctx context.Context, u *Utterance) error {
	if u == nil {
		return nil
	}
	if u.Platform == "" {
		return fmt.Errorf("empty platform in utterance")
	}

	if u.IsGroup {
		u.Text = core.RedactSensitiveIPs(u.Text)
		u.FallbackText = core.RedactSensitiveIPs(u.FallbackText)
	}

	ch, ok := r.Get(u.Platform)
	if !ok {
		return fmt.Errorf("channel %q not mounted", u.Platform)
	}

	return ch.Transmit(ctx, u)
}

// SuperAgent 呆呆超级数字生命体核心聚合根 (仅持有实际调度的核心职责)
type SuperAgent struct {
	Persona                    *Persona
	Immune                     *ImmuneSystem
	Cognition                  Cognition
	Delegates                  *Delegates
	Channels                   *ChannelRegistry
	Workspaces                 *WorkspaceManager
	AGYAuth                    AGYAuthenticator
	OnAuthSuccess              func(ctx context.Context)
	pendingNotificationLocksMu sync.Mutex
	pendingNotificationLocks   map[pendingNotificationKey]*sessionLockEntry
	continuationMu             sync.Mutex
	continuations              map[thinkSessionKey]pendingReplyContinuation
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

type pendingReplyContinuation struct {
	text      string
	updatedAt time.Time
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
	PartnerID     string
	Persona       *Persona
	Immune        *ImmuneSystem
	Cognition     Cognition
	Delegates     *Delegates
	Channels      *ChannelRegistry
	Workspaces    *WorkspaceManager
	AGYAuth       AGYAuthenticator
	OnAuthSuccess func(ctx context.Context)
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
		Persona:       persona,
		Immune:        immune,
		Cognition:     cfg.Cognition,
		Delegates:     cfg.Delegates,
		Channels:      channels,
		Workspaces:    cfg.Workspaces,
		AGYAuth:       cfg.AGYAuth,
		OnAuthSuccess: cfg.OnAuthSuccess,
	}
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
		var startupErr interface{ rollbackFailed() bool }
		if !errors.As(err, &startupErr) || !startupErr.rollbackFailed() {
			a.runCancel = nil
		}
		return err
	}
	return nil
}

func (a *SuperAgent) processingTimeout() time.Duration {
	const defaultTimeout = 6 * time.Minute
	const completionGrace = 5 * time.Minute
	const maxDuration = time.Duration(1<<63 - 1)

	if a.Delegates == nil {
		return defaultTimeout
	}
	ticketTimeout := a.Delegates.ExecutionTimeout()
	if ticketTimeout == 0 {
		return 0
	}
	if ticketTimeout <= defaultTimeout-completionGrace {
		return defaultTimeout
	}
	if ticketTimeout > maxDuration-completionGrace {
		return maxDuration
	}
	return ticketTimeout + completionGrace
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
	if a.Channels != nil {
		stopErr = a.Channels.StopAll(ctx)
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

func (a *SuperAgent) transmitReplyText(ctx context.Context, event *SensoryEvent, text string) error {
	_, err := a.transmitReplyTextDelivered(ctx, event, text)
	return err
}

func (a *SuperAgent) transmitReplyTextDelivered(ctx context.Context, event *SensoryEvent, text string) (string, error) {
	if a.Channels == nil {
		err := fmt.Errorf("channel registry is not configured")
		slog.Warn("superagent: 文本回复发送失败", "platform", event.Platform, "channel", event.ChannelID, "err", err)
		return "", err
	}
	if !event.IsPartnerPrivate() {
		text = core.RedactSensitiveIPs(text)
	}
	delivered, err := a.transmitUtterance(ctx, &Utterance{
		Platform:   event.Platform,
		IsGroup:    event.IsGroup,
		ChannelID:  event.ChannelID,
		QuoteMsgID: event.RawMessageID,
		Text:       text,
	}, event.SenderID)
	if err != nil {
		slog.Warn("superagent: 文本回复发送失败", "platform", event.Platform, "channel", event.ChannelID, "err", err)
	}
	return delivered, err
}

func (a *SuperAgent) transmitUtterance(ctx context.Context, utterance *Utterance, senderID string) (string, error) {
	if utterance == nil {
		return "", nil
	}
	if a.Channels == nil {
		return "", fmt.Errorf("channel registry is not configured")
	}
	var partialErr *PartialDeliveryError
	if err := a.Channels.Transmit(ctx, utterance); err == nil {
		return utterance.Text, nil
	} else if errors.As(err, &partialErr) {
		slog.Warn("superagent: 消息部分送达，不重复发送正文", "channel", utterance.ChannelID, "err", err)
		if ctx.Err() == nil && strings.TrimSpace(partialErr.UndeliveredText) != "" {
			a.setReplyContinuation(ctx, senderID, utterance, partialErr.UndeliveredText)
		}
		return partialErr.DeliveredText, nil
	} else if ctx.Err() != nil {
		return "", ctx.Err()
	} else if errors.Is(err, ErrMessageTooLong) {
		fallbackText := utterance.Text
		if strings.TrimSpace(utterance.FallbackText) != "" {
			fallbackText = utterance.FallbackText
		}
		if utterance.HasMedia() {
			fallbackText = strings.TrimSpace(fallbackText + "\n[图文内容超出平台单次发送限制，已改为文字版]")
		}
		if strings.TrimSpace(fallbackText) == "" {
			fallbackText = "回复内容超过平台单次发送上限。"
		}
		return a.transmitTextFallback(ctx, utterance, fallbackText, senderID)
	} else {
		if !utterance.HasMedia() {
			return "", err
		}
		slog.Warn("superagent: 图片消息发送失败，回退为文字", "channel", utterance.ChannelID, "err", err)
		fallbackText := ""
		if strings.TrimSpace(utterance.FallbackText) != "" {
			fallbackText = strings.TrimSpace(utterance.FallbackText + "\n[图片无法发送，以下为文字版]")
		} else if strings.TrimSpace(utterance.Text) == "" {
			fallbackText = "图片发送失败，请稍后重试。"
		} else {
			fallbackText = strings.TrimSpace(utterance.Text + "\n[图片发送失败，以下改用文字回复]")
		}
		deliveredText, fallbackErr := a.transmitTextFallback(ctx, utterance, fallbackText, senderID)
		if fallbackErr != nil {
			return "", fmt.Errorf("send image failed: %v; text fallback failed: %w", err, fallbackErr)
		}
		return deliveredText, nil
	}
}

func (a *SuperAgent) transmitTextFallback(ctx context.Context, utterance *Utterance, text, senderID string) (string, error) {
	return a.transmitTextFallbackWithContinuationContext(ctx, ctx, utterance, text, senderID)
}

func (a *SuperAgent) transmitTextFallbackWithContinuationContext(ctx, continuationCtx context.Context, utterance *Utterance, text, senderID string) (string, error) {
	if a.Channels == nil {
		return "", fmt.Errorf("channel registry is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if continuationCtx == nil {
		continuationCtx = ctx
	}
	fallback := *utterance
	fallback.FallbackText = ""
	fallback.CardData = nil
	fallback.Images = nil
	chunks := core.SplitMarkdownChunks(text, 1100)
	if len(chunks) <= 5 {
		fallback.Text = text
		if err := a.Channels.Transmit(ctx, &fallback); err != nil {
			var partialErr *PartialDeliveryError
			if errors.As(err, &partialErr) {
				if continuationCtx.Err() == nil && strings.TrimSpace(partialErr.UndeliveredText) != "" {
					a.setReplyContinuation(continuationCtx, senderID, utterance, partialErr.UndeliveredText)
				}
				return partialErr.DeliveredText, nil
			}
			return "", err
		}
		return fallback.Text, nil
	}

	remaining := strings.Join(chunks[5:], "\n\n")
	marker := "\n[已达到平台单次发送上限，后续内容未发送；发送“继续”可接着看。]"
	page := append([]string(nil), chunks[:5]...)
	page[len(page)-1] += marker
	fallback.Text = strings.Join(page, "\n\n")
	if err := a.Channels.Transmit(ctx, &fallback); err != nil {
		var partialErr *PartialDeliveryError
		if errors.As(err, &partialErr) {
			pending := partialErr.UndeliveredText
			if remaining != "" {
				pending = strings.TrimSpace(pending + "\n\n" + remaining)
			}
			if continuationCtx.Err() == nil && strings.TrimSpace(pending) != "" {
				a.setReplyContinuation(continuationCtx, senderID, utterance, pending)
			}
			return partialErr.DeliveredText, nil
		}
		return "", err
	}
	a.setReplyContinuation(continuationCtx, senderID, utterance, remaining)
	return fallback.Text, nil
}

func continuationKey(senderID string, utterance *Utterance) thinkSessionKey {
	return thinkSessionKey{
		session:   fmt.Sprintf("%s:%v:%s", utterance.Platform, utterance.IsGroup, utterance.ChannelID),
		requester: senderID,
	}
}

func (a *SuperAgent) setReplyContinuation(ctx context.Context, senderID string, utterance *Utterance, text string) {
	if senderID == "" || utterance == nil || strings.TrimSpace(text) == "" {
		return
	}
	key := continuationKey(senderID, utterance)
	a.continuationMu.Lock()
	defer a.continuationMu.Unlock()
	if ctx != nil && ctx.Err() != nil {
		return
	}
	a.storeReplyContinuationLocked(key, text, time.Now())
}

func (a *SuperAgent) storeReplyContinuationLocked(key thinkSessionKey, text string, now time.Time) {
	if a.continuations == nil {
		a.continuations = make(map[thinkSessionKey]pendingReplyContinuation)
	}
	for existingKey, continuation := range a.continuations {
		if now.Sub(continuation.updatedAt) > 24*time.Hour {
			delete(a.continuations, existingKey)
		}
	}
	if _, exists := a.continuations[key]; !exists && len(a.continuations) >= 128 {
		var oldestKey thinkSessionKey
		var oldest time.Time
		for existingKey, continuation := range a.continuations {
			if oldest.IsZero() || continuation.updatedAt.Before(oldest) {
				oldestKey, oldest = existingKey, continuation.updatedAt
			}
		}
		delete(a.continuations, oldestKey)
	}
	continuation := a.continuations[key]
	if continuation.text != "" {
		continuation.text += "\n\n"
	}
	continuation.text += text
	continuation.updatedAt = now
	a.continuations[key] = continuation
}

func (a *SuperAgent) takeReplyContinuation(event *SensoryEvent) (string, bool) {
	if event == nil || event.SenderID == "" {
		return "", false
	}
	a.continuationMu.Lock()
	defer a.continuationMu.Unlock()
	key := thinkKey(event)
	continuation, ok := a.continuations[key]
	if !ok {
		return "", false
	}
	if time.Since(continuation.updatedAt) > 24*time.Hour {
		delete(a.continuations, key)
		return "", false
	}
	delete(a.continuations, key)
	return continuation.text, true
}

func (a *SuperAgent) restoreReplyContinuation(ctx context.Context, event *SensoryEvent, text string) {
	if event == nil || event.SenderID == "" || strings.TrimSpace(text) == "" {
		return
	}
	key := thinkKey(event)
	a.continuationMu.Lock()
	defer a.continuationMu.Unlock()
	if ctx != nil && ctx.Err() != nil {
		return
	}
	now := time.Now()
	continuation := a.continuations[key]
	if continuation.text != "" {
		text += "\n\n" + continuation.text
	}
	continuation.text = text
	continuation.updatedAt = now
	a.continuations[key] = continuation
}

func (a *SuperAgent) clearReplyContinuation(event *SensoryEvent) {
	if event == nil || event.SenderID == "" {
		return
	}
	a.continuationMu.Lock()
	delete(a.continuations, thinkKey(event))
	a.continuationMu.Unlock()
}

func (a *SuperAgent) replyText(ctx context.Context, event *SensoryEvent, text string) {
	_ = a.transmitReplyText(ctx, event, text)
}

func (a *SuperAgent) transmitInfo(ctx context.Context, event *SensoryEvent, caption, content string) error {
	if event == nil {
		return fmt.Errorf("cannot send information without an event")
	}
	if a.Channels == nil {
		return fmt.Errorf("channel registry is not configured")
	}
	if !event.IsPartnerPrivate() {
		content = core.RedactSensitiveIPs(content)
	}
	renderer, ok := a.Cognition.(interface {
		RenderCard(context.Context, *SensoryEvent, string) ([]byte, error)
	})
	if !ok {
		return a.transmitReplyText(ctx, event, content)
	}
	if policy, ok := a.Cognition.(interface {
		ShouldRenderCard(context.Context, string) bool
	}); ok && !policy.ShouldRenderCard(ctx, content) {
		return a.transmitReplyText(ctx, event, content)
	}
	cardCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	card, err := renderer.RenderCard(cardCtx, event, content)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("superagent: 信息卡片渲染失败，回退为文字", "platform", event.Platform, "channel", event.ChannelID, "err", err)
		return a.transmitReplyText(ctx, event, content)
	}
	cardUtterance := &Utterance{
		Platform:     event.Platform,
		IsGroup:      event.IsGroup,
		ChannelID:    event.ChannelID,
		QuoteMsgID:   event.RawMessageID,
		Text:         caption,
		FallbackText: content,
		CardData:     card,
	}
	if _, err := a.transmitUtterance(ctx, cardUtterance, event.SenderID); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("superagent: 信息卡片发送失败，回退为文字", "platform", event.Platform, "channel", event.ChannelID, "err", err)
		return err
	}
	return nil
}

// transmitPendingNotification sends a durable notification without moving
// unsent content into the process-local conversational continuation buffer.
func (a *SuperAgent) transmitPendingNotification(ctx context.Context, event *SensoryEvent, caption, content string) (bool, string, error) {
	if event == nil || a.Channels == nil {
		return false, "", fmt.Errorf("notification delivery is not configured")
	}
	if !event.IsPartnerPrivate() {
		content = core.RedactSensitiveIPs(content)
	}
	const maxNotificationParts = 3
	notificationPartLimit := maxNotificationParts
	if renderer, ok := a.Cognition.(interface {
		RenderCard(context.Context, *SensoryEvent, string) ([]byte, error)
	}); ok {
		shouldRender := true
		if policy, ok := a.Cognition.(interface {
			ShouldRenderCard(context.Context, string) bool
		}); ok {
			shouldRender = policy.ShouldRenderCard(ctx, content)
		}
		if shouldRender {
			cardCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			card, err := renderer.RenderCard(cardCtx, event, content)
			cancel()
			if err == nil && len(card) > 0 {
				cardUtterance := &Utterance{
					Platform:     event.Platform,
					IsGroup:      event.IsGroup,
					ChannelID:    event.ChannelID,
					QuoteMsgID:   event.RawMessageID,
					Text:         caption,
					FallbackText: content,
					CardData:     card,
				}
				if sendErr := a.Channels.Transmit(ctx, cardUtterance); sendErr == nil {
					return true, "", nil
				} else {
					if ctx.Err() != nil {
						return false, "", ctx.Err()
					}
					// QQ reserves a passive-reply attempt before sending. A failed
					// media attempt still consumes one slot, so reduce the text
					// fallback by one part and preserve a slot for the user's reply.
					notificationPartLimit--
					slog.Warn("superagent: 待发送通知卡片失败，改发文本", "notificationChannel", event.ChannelID, "err", sendErr)
				}
			} else if err != nil {
				if ctx.Err() != nil {
					return false, "", ctx.Err()
				}
				slog.Warn("superagent: 待发送通知卡片渲染失败，改发文本", "notificationChannel", event.ChannelID, "err", err)
			}
		}
	}

	chunks := core.SplitMarkdownChunks(content, 1100)
	if len(chunks) == 0 {
		return true, "", nil
	}
	var remaining string
	// QQ allows four passive replies to one private message. Keep one slot for
	// the answer to the message that triggered this notification delivery.
	if len(chunks) > notificationPartLimit {
		contentParts := notificationPartLimit - 1
		remaining = strings.Join(chunks[contentParts:], "\n\n")
		chunks = append(append([]string(nil), chunks[:contentParts]...), "📨 报告还有剩余内容，将在你下次私聊时继续送达。")
	}
	textUtterance := &Utterance{
		Platform:   event.Platform,
		IsGroup:    event.IsGroup,
		ChannelID:  event.ChannelID,
		QuoteMsgID: event.RawMessageID,
		Text:       strings.Join(chunks, "\n\n"),
	}
	if err := a.Channels.Transmit(ctx, textUtterance); err != nil {
		var partialErr *PartialDeliveryError
		if !errors.As(err, &partialErr) {
			return false, "", err
		}
		pending := strings.TrimSpace(partialErr.UndeliveredText)
		if remaining != "" {
			pending = strings.TrimSuffix(pending, "📨 报告还有剩余内容，将在你下次私聊时继续送达。")
			pending = strings.TrimSpace(pending)
			if pending != "" {
				pending += "\n\n"
			}
			pending += remaining
		}
		if pending == "" {
			return true, "", nil
		}
		return false, pending, nil
	}
	return remaining == "", remaining, nil
}

func (a *SuperAgent) replyInfo(ctx context.Context, event *SensoryEvent, caption, content string) {
	if err := a.transmitInfo(ctx, event, caption, content); err != nil {
		slog.Warn("superagent: 信息发送失败", "platform", event.Platform, "channel", event.ChannelID, "err", err)
	}
}

func (a *SuperAgent) deliverPendingNotifications(ctx context.Context, event *SensoryEvent) {
	if event == nil || !event.IsPartnerPrivate() || event.RawMessageID == "" {
		return
	}
	unlock := a.lockPendingNotification(event.Platform, event.SenderID)
	defer unlock()
	loadCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	notifications, err := db.DefaultManager().ListPendingNotifications(loadCtx, event.Platform, event.SenderID, 1)
	cancel()
	if err != nil {
		slog.Warn("superagent: 读取待发送通知失败", "err", err)
		return
	}
	if len(notifications) == 0 {
		return
	}
	notice := notifications[0]
	caption := "📨 延迟送达的通知"
	switch notice.Category {
	case "oci_daily":
		caption = "📊 延迟送达的 OCI 账单日报"
	case "oci_alert":
		caption = "⚠️ 延迟送达的 OCI 资源告警"
	}
	sendCtx, cancelSend := context.WithTimeout(ctx, 25*time.Second)
	complete, remainingBody, err := a.transmitPendingNotification(sendCtx, event, caption, notice.Body)
	cancelSend()
	if err != nil {
		slog.Warn("superagent: 待发送通知仍未送达", "notificationID", notice.ID, "category", notice.Category, "err", err)
		return
	}
	deleteCtx, cancelDelete := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelDelete()
	if !complete {
		if err := db.DefaultManager().UpdatePendingNotificationBody(deleteCtx, event.Platform, event.SenderID, notice.ID, remainingBody); err != nil {
			slog.Error("superagent: 通知分段送达但未能保存剩余内容", "notificationID", notice.ID, "err", err)
		} else {
			slog.Info("superagent: 通知已分段送达，剩余内容保留待下次私聊", "notificationID", notice.ID, "remainingBytes", len(remainingBody))
		}
		return
	}
	if err := db.DefaultManager().DeletePendingNotification(deleteCtx, event.Platform, event.SenderID, notice.ID); err != nil {
		slog.Error("superagent: 通知已发送但无法从待发队列删除", "notificationID", notice.ID, "err", err)
	}
}

func (a *SuperAgent) delegationPermissionError(role UserRole) error {
	if a.Immune == nil {
		return fmt.Errorf("权限校验未配置，拒绝执行此操作")
	}
	return a.Immune.VerifyDelegationPerm(role)
}

func parseAGYAuthCodeCommand(content string) (string, bool) {
	parts := strings.Fields(strings.TrimSpace(content))
	if len(parts) < 3 || !strings.EqualFold(parts[0], "/agy") ||
		(!strings.EqualFold(parts[1], "code") && !strings.EqualFold(parts[1], "auth")) {
		return "", false
	}
	return strings.Trim(strings.Join(parts[2:], " "), "`'\""), true
}

func (a *SuperAgent) commitObservedGroupMessage(event *SensoryEvent) {
	if event == nil || (event.StillValid != nil && !event.StillValid()) {
		return
	}
	committer, ok := a.Cognition.(interface {
		CommitDeliveredReply(context.Context, *SensoryEvent, string)
	})
	if !ok {
		return
	}
	commitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	committer.CommitDeliveredReply(commitCtx, event, "")
}

// Process 核心认知信息流：物理感知 -> 控制面快筛 -> 认知大脑 -> 物理通道发声
func (a *SuperAgent) Process(ctx context.Context, event *SensoryEvent) {
	if event == nil {
		return
	}
	if event.StillValid != nil && !event.StillValid() {
		return
	}
	if event.HistoryRecordID == 0 {
		if recorder, ok := a.Cognition.(interface {
			RecordInbound(context.Context, *SensoryEvent) error
		}); ok {
			recordCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := recorder.RecordInbound(recordCtx, event); err != nil {
				slog.Warn("superagent: 保存入站消息历史失败", "channel", event.ChannelID, "err", err)
			}
			cancel()
		}
	}

	content := strings.TrimSpace(event.Content)
	slog.Info("superagent: 感知事件流入大脑", "channel", event.ChannelID, "isGroup", event.IsGroup, "sender", event.SenderName, "role", event.Role, "contentBytes", len(content))
	if event.GroupMessageObserved && (!event.GroupInterjection || strings.HasPrefix(content, "/") || content == "继续" || a.Cognition == nil) {
		unlock := a.lockSession(event)
		defer unlock()
		if event.StillValid == nil || event.StillValid() {
			a.commitObservedGroupMessage(event)
		}
		return
	}
	if content == "继续" || content == "/continue" {
		continueCtx, continueCancel := context.WithCancel(ctx)
		cancellation := &thinkCancellation{key: thinkKey(event)}
		a.sessionCancels.Store(cancellation, continueCancel)
		defer func() {
			a.sessionCancels.Delete(cancellation)
			continueCancel()
		}()
		unlock := a.lockSession(event)
		defer unlock()
		if continueCtx.Err() != nil || (event.StillValid != nil && !event.StillValid()) {
			return
		}
		continuation, ok := a.takeReplyContinuation(event)
		if !ok {
			a.replyText(continueCtx, event, "当前没有待续发的长回复。")
			return
		}
		if !event.IsPartnerPrivate() {
			continuation = core.RedactSensitiveIPs(continuation)
		}
		sendCtx, cancel := context.WithTimeout(continueCtx, 45*time.Second)
		continuationUtterance := &Utterance{
			Platform:   event.Platform,
			IsGroup:    event.IsGroup,
			ChannelID:  event.ChannelID,
			QuoteMsgID: event.RawMessageID,
		}
		deliveredText, err := a.transmitTextFallbackWithContinuationContext(sendCtx, continueCtx, continuationUtterance, continuation, event.SenderID)
		cancel()
		if err != nil {
			if continueCtx.Err() == nil {
				a.restoreReplyContinuation(continueCtx, event, continuation)
				slog.Warn("superagent: 续发长回复失败，已保留续发内容", "channel", event.ChannelID, "err", err)
			}
		} else if committer, ok := a.Cognition.(interface {
			CommitDeliveredReply(context.Context, *SensoryEvent, string)
		}); ok {
			commitCtx, cancelCommit := context.WithTimeout(continueCtx, 15*time.Second)
			committer.CommitDeliveredReply(commitCtx, event, deliveredText)
			cancelCommit()
		}
		return
	}
	if content == "/task" || strings.HasPrefix(content, "/task ") {
		if err := a.delegationPermissionError(event.Role); err != nil {
			a.replyText(ctx, event, err.Error())
			return
		}
		parts := strings.Fields(content)
		if (len(parts) != 2 && len(parts) != 3) || a.Delegates == nil {
			a.replyText(ctx, event, "用法：/task <工单编号> [页码]")
			return
		}
		page := 1
		if len(parts) == 3 {
			parsedPage, parseErr := strconv.Atoi(parts[2])
			if parseErr != nil || parsedPage < 1 {
				a.replyText(ctx, event, "页码必须是大于 0 的整数。用法：/task <工单编号> [页码]")
				return
			}
			page = parsedPage
		}
		ticket, err := a.Delegates.TicketFor(ctx, event.Platform, event.SenderID, event.ChannelID, parts[1])
		if ticket == nil {
			if err != nil {
				slog.Warn("superagent: 查询工单失败", "channel", event.ChannelID, "err", err)
				a.replyText(ctx, event, "读取工单状态失败，请稍后重试。")
				return
			}
			a.replyText(ctx, event, "当前会话中没有找到这张工单。")
			return
		}
		output := strings.TrimSpace(ticket.Output)
		if output == "" {
			output = "（暂无执行输出）"
		}
		if !event.IsPartnerPrivate() {
			output = core.RedactSensitiveIPs(output)
		}
		const outputPageSize = 3500
		pages := core.SplitMarkdownChunks(output, outputPageSize)
		pageCount := len(pages)
		if page > pageCount {
			a.replyText(ctx, event, fmt.Sprintf("这张工单共 %d 页，请选择 1 到 %d。", pageCount, pageCount))
			return
		}
		pageOutput := pages[page-1]
		goal := core.TruncateRunes(strings.TrimSpace(ticket.Goal), 300, "…")
		detail := fmt.Sprintf("【工单 %s · %s · 第 %d/%d 页】\n目标：%s\n更新时间：%s\n\n%s", ticket.ID, ticket.Status, page, pageCount, goal, ticket.UpdatedAt.Format("2006-01-02 15:04:05"), pageOutput)
		if err != nil {
			detail += "\n\n[警告：此工单最新状态尚未可靠写入 SQLite，当前显示的是进程内快照]"
		}
		a.replyText(ctx, event, detail)
		return
	}

	// 极速控制面 (0 Token 拦截通道)
	if strings.HasPrefix(content, "/") {
		switch content {
		case "/tasks":
			if err := a.delegationPermissionError(event.Role); err != nil {
				a.replyText(ctx, event, err.Error())
				return
			}
			if a.Delegates == nil {
				a.replyText(ctx, event, "工单调度器未配置。")
				return
			}
			tickets, err := a.Delegates.TicketsFor(ctx, event.Platform, event.SenderID, event.ChannelID, 10)
			if err != nil {
				slog.Warn("superagent: 查询工单历史失败", "channel", event.ChannelID, "err", err)
			}
			if err != nil && len(tickets) == 0 {
				a.replyText(ctx, event, "读取工单历史失败，请稍后重试。")
				return
			}
			if len(tickets) == 0 {
				a.replyText(ctx, event, "当前会话没有你的近期工单。")
				return
			}
			lines := make([]string, 0, len(tickets))
			for _, ticket := range tickets {
				goal := strings.TrimSpace(ticket.Goal)
				if len([]rune(goal)) > 100 {
					goal = string([]rune(goal)[:100]) + "…"
				}
				lines = append(lines, fmt.Sprintf("- %s [%s] %s：%s", ticket.ID, ticket.Status, ticket.UpdatedAt.Format("2006-01-02 15:04"), goal))
			}
			reply := "【当前会话的近期工单】\n" + strings.Join(lines, "\n")
			if err != nil {
				reply += "\n\n[警告：SQLite 历史或部分最新状态不可用；以上可能只包含进程内快照]"
			}
			a.replyInfo(ctx, event, "📋 近期工单", reply)
			return

		case "/status", "/sysinfo":
			var sections []string
			sections = append(sections, "### 呆呆生命体当前机能状态")
			if a.Channels != nil {
				sections = append(sections, a.Channels.FormatStatus())
			}
			if a.Cognition != nil {
				if mindStatus := strings.TrimSpace(a.Cognition.MindStatus()); mindStatus != "" {
					sections = append(sections, mindStatus)
				}
			}
			sections = append(sections, core.FormatSystemStatusWithPerm(event.IsPartnerPrivate()))
			if event.Role != RoleGuest {
				if a.Workspaces != nil {
					sections = append(sections, a.Workspaces.FormatCurrent(event.ChannelID))
				}
				if a.Delegates != nil {
					active := a.Delegates.ActiveTicketsFor(event.Platform, event.SenderID, event.ChannelID)
					if len(active) == 0 {
						sections = append(sections, "后台工程工单：当前空闲 (0 个活跃)")
					} else {
						var tLines []string
						for _, t := range active {
							tLines = append(tLines, fmt.Sprintf("- %s [%s · %s]: %s (已持续 %s)", t.ID, t.Worker, t.Status, t.Goal, time.Since(t.CreatedAt).Truncate(time.Second)))
						}
						sections = append(sections, fmt.Sprintf("后台工程工单 (%d 个活跃):\n%s", len(active), strings.Join(tLines, "\n")))
					}
				}
			}
			a.replyInfo(ctx, event, "📊 当前状态", strings.Join(sections, "\n\n"))
			return

		case "/usage", "/model", "/models":
			if err := a.delegationPermissionError(event.Role); err != nil {
				a.replyText(ctx, event, err.Error())
				return
			}
			if a.Cognition == nil {
				return
			}
			switch content {
			case "/model":
				status := strings.TrimSpace(a.Cognition.MindStatus())
				if status == "" {
					status = "当前没有可用的激活模型状态。"
				}
				a.replyInfo(ctx, event, "🧠 当前模型", status)
			case "/usage":
				var usage string
				if contextual, ok := a.Cognition.(interface{ ModelUsageContext(context.Context) string }); ok {
					usage = contextual.ModelUsageContext(ctx)
				} else {
					usage = a.Cognition.ModelUsage()
				}
				a.replyInfo(ctx, event, "📊 模型用量", usage)
			default:
				var models string
				if contextual, ok := a.Cognition.(interface{ ModelListContext(context.Context) string }); ok {
					models = contextual.ModelListContext(ctx)
				} else {
					models = a.Cognition.ModelList()
				}
				a.replyInfo(ctx, event, "🧠 可用模型", models)
			}
			return

		case "/agy login", "/agy auth", "/agy login force", "/agy login --force":
			if event.IsGroup {
				a.replyText(ctx, event, "AGY 认证链接包含账号授权信息，请在与呆呆的私聊中发起登录。")
				return
			}
			if err := a.delegationPermissionError(event.Role); err != nil {
				a.replyText(ctx, event, err.Error())
				return
			}
			if a.AGYAuth == nil {
				a.replyText(ctx, event, "AGY 认证管理器未挂载。")
				return
			}
			isForce := strings.HasSuffix(content, "force")
			if !isForce {
				if st := a.AGYAuth.GetStatus(); st.IsLoggedIn {
					a.replyText(ctx, event, fmt.Sprintf("当前已登录 Google 账号 (%s)。如需重新登录或切换账号，请发送 `/agy logout` 注销后再登录，或发送 `/agy login force` 强制重新认证。", st.Email))
					return
				}
			} else {
				if err := a.AGYAuth.Logout(); err != nil {
					slog.Warn("superagent: 强制重新认证前清理 AGY 凭据失败", "err", err)
					a.replyText(ctx, event, "清理现有 AGY 登录状态失败，未发起重新认证。请检查服务日志后重试。")
					return
				}
			}
			a.replyText(ctx, event, "正在发起 Google 账号认证，请稍候获取授权链接...")
			authURL, err := a.AGYAuth.StartLogin(ctx)
			if err != nil {
				slog.Warn("superagent: 启动 AGY 登录失败", "err", err)
				a.replyText(ctx, event, "没能启动 AGY 登录流程，请检查服务日志中的具体原因后重试。")
				return
			}
			reply := fmt.Sprintf("【Google Antigravity 登录授权】\n请在浏览器或手机中打开以下链接，登录 Google 账号并完成授权：\n\n```\n%s\n```\n\n授权成功后，页面会显示一串授权码 (Authorization Code)。\n请直接回复：\n`/agy code <授权码>`\n（授权会话 5 分钟内有效）", authURL)
			a.replyText(ctx, event, reply)
			return

		case "/agy status":
			if event.IsGroup {
				a.replyText(ctx, event, "AGY 账号状态包含私人信息，请在与呆呆的私聊中查询。")
				return
			}
			if err := a.delegationPermissionError(event.Role); err != nil {
				a.replyText(ctx, event, err.Error())
				return
			}
			if a.AGYAuth == nil {
				a.replyText(ctx, event, "AGY 认证管理器未挂载。")
				return
			}
			st := a.AGYAuth.GetStatus()
			if !st.IsLoggedIn {
				a.replyInfo(ctx, event, "🔐 AGY 认证状态", "【AGY 认证状态】\n当前尚未登录 Google 账号或凭证已失效。可发送 `/agy login` 发起登录。")
				return
			}
			expiryStr := "长期有效"
			if !st.Expiry.IsZero() {
				expiryStr = st.Expiry.Local().Format("2006-01-02 15:04:05")
			}
			reply := fmt.Sprintf("【AGY 认证状态】\n- 状态：已登录在线\n- 账号：%s\n- Token 有效期至：%s", st.Email, expiryStr)
			a.replyInfo(ctx, event, "🔐 AGY 认证状态", reply)
			return

		case "/agy logout":
			if event.IsGroup {
				a.replyText(ctx, event, "账号注销操作请在与呆呆的私聊中进行。")
				return
			}
			if err := a.delegationPermissionError(event.Role); err != nil {
				a.replyText(ctx, event, err.Error())
				return
			}
			if a.AGYAuth == nil {
				a.replyText(ctx, event, "AGY 认证管理器未挂载。")
				return
			}
			st := a.AGYAuth.GetStatus()
			if err := a.AGYAuth.Logout(); err != nil {
				slog.Warn("superagent: 注销 AGY 账号失败", "err", err)
				a.replyText(ctx, event, "注销未完成，凭据状态未确认。请检查服务日志后重试。")
				return
			}
			if !st.IsLoggedIn {
				a.replyText(ctx, event, "【AGY 登录已取消】\n当前没有已登录账号；等待中的登录会话已清理。如需使用可发送 `/agy login` 重新认证。")
				return
			}
			emailHint := ""
			if st.Email != "" {
				emailHint = fmt.Sprintf(" (%s)", st.Email)
			}
			a.replyText(ctx, event, fmt.Sprintf("【AGY 账号已注销】\n已安全清除 Google 账号凭据%s。如需再次使用可发送 `/agy login` 重新认证。", emailHint))
			return

		case "/stop":
			if err := a.delegationPermissionError(event.Role); err != nil {
				a.replyText(ctx, event, err.Error())
				return
			}
			stoppedThinking := a.cancelThinkingRequester(event)
			// A concurrently cancelling continuation may have restored its body
			// just before the cancellation took effect. Clear again afterwards.
			a.clearReplyContinuation(event)
			aborted := 0
			stopping := 0
			if a.Delegates != nil {
				aborted = a.Delegates.AbortByRequester(event.Platform, event.ChannelID, event.SenderID)
				for _, ticket := range a.Delegates.ActiveTicketsFor(event.Platform, event.SenderID, event.ChannelID) {
					if ticket.Status == TicketStopping {
						stopping++
					}
				}
			}
			msg := "当前没有正在运行中的思考任务或后台工单。"
			if aborted > 0 && stoppedThinking {
				msg = fmt.Sprintf("已紧急中断当前思考进程，并向 %d 个后台工单发出停止请求（尚未启动的任务已取消；运行中任务退出后释放执行容量）。", aborted)
			} else if aborted > 0 {
				msg = fmt.Sprintf("已向 %d 个后台工单发出停止请求（尚未启动的任务已取消；运行中任务退出后释放执行容量）。", aborted)
			} else if stopping > 0 && stoppedThinking {
				msg = fmt.Sprintf("已紧急中断当前思考进程；另有 %d 个后台工单仍在停止过程中，工作者退出后才会释放执行容量。", stopping)
			} else if stopping > 0 {
				msg = fmt.Sprintf("仍有 %d 个后台工单正在停止；工作者退出后才会释放执行容量。", stopping)
			} else if stoppedThinking {
				msg = "已紧急中断当前正在进行的思考与工具调用。"
			}
			a.replyText(ctx, event, msg)
			return

		case "/reset", "/new", "/clear", "/ws clear", "/ws reset":
			if err := a.delegationPermissionError(event.Role); err != nil {
				a.replyText(ctx, event, err.Error())
				return
			}

			// 先取消思考上下文以解除会话锁等待；后台工单等工作区校验后再处理。
			a.cancelThinkingSession(event)

			unlock := a.lockSession(event)
			defer unlock()
			// Clear after acquiring the lock too, so a cancelled continuation
			// cannot restore old content after the reset's first clear.
			a.clearReplyContinuation(event)

			currWs := ""
			wsName := ""
			if a.Workspaces != nil {
				var workspaceErr error
				currWs, wsName, workspaceErr = a.Workspaces.GetCurrentChecked(event.ChannelID)
				if workspaceErr != nil {
					a.replyText(ctx, event, fmt.Sprintf("【会话重开失败】\n无法确认当前工作区，近期历史和 AGY 会话均未重置：%v", workspaceErr))
					return
				}
			}
			// 先确认工作区可读，再中断后台工单，避免失败的预检查产生副作用。
			if a.Delegates != nil {
				_ = a.Delegates.AbortByChannel(event.Platform, event.ChannelID)
			}

			chatType := "private"
			if event.IsGroup {
				chatType = "group"
			}
			if err := db.DefaultManager().ResetSessionHistoryInChat(event.Platform, chatType, event.ChannelID); err != nil {
				slog.Error("superagent: 重开会话时清理近期历史失败", "platform", event.Platform, "channelID", event.ChannelID, "err", err)
				a.replyText(ctx, event, "【会话重开失败】\n近期对话历史未能清理，AGY 会话记录未清空；已中断的任务不会自动恢复。请稍后重试。")
				return
			}
			if attachmentCleaner, ok := a.Cognition.(interface {
				ClearPendingAttachments(*SensoryEvent)
			}); ok {
				attachmentCleaner.ClearPendingAttachments(event)
			}
			if a.Delegates != nil {
				if err := a.Delegates.ResetSession(event.ChannelID, currWs); err != nil {
					slog.Error("superagent: 重开会话时清理 AGY 会话失败", "platform", event.Platform, "channelID", event.ChannelID, "workspace", currWs, "err", err)
					a.replyText(ctx, event, fmt.Sprintf("【会话重开未完成】\n近期历史游标已重置，但 AGY 会话未能确认清除：%v。请先检查服务日志，再重试 `/reset`。", err))
					return
				}
			}

			feedback := fmt.Sprintf("【当前会话已完成重开】\n已清空工作空间 `%s` (`%s`) 的 agy 会话与近期对话记忆，长期画像与项目文件完好保留。", wsName, currWs)
			a.replyText(ctx, event, feedback)
			return

		case "/pwd":
			if a.Workspaces == nil {
				a.replyText(ctx, event, "工作空间管理器未配置。")
				return
			}
			unlock := a.lockSession(event)
			workspaceStatus := a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "current", "", a.Delegates)
			unlock()
			a.replyInfo(ctx, event, "📁 当前工作空间", workspaceStatus)
			return

		case "/workspaces", "/ws":
			if a.Workspaces == nil {
				a.replyText(ctx, event, "工作空间管理器未配置。")
				return
			}
			unlock := a.lockSession(event)
			workspaceList := a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "list", "", a.Delegates)
			unlock()
			a.replyInfo(ctx, event, "📚 工作空间书签", workspaceList)
			return

		case "/ws exit", "/workspaces exit":
			unlock := a.lockSession(event)
			defer unlock()
			if a.Workspaces == nil {
				a.replyText(ctx, event, "工作空间管理器未配置。")
				return
			}
			a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "exit", "", a.Delegates))
			return

		case "/ws clean", "/clean":
			unlock := a.lockSession(event)
			defer unlock()
			if a.Workspaces == nil {
				a.replyText(ctx, event, "工作空间管理器未配置。")
				return
			}
			a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "clean", "", a.Delegates))
			return

		case "/help":
			helpText := fmt.Sprintf(`【%s · 控制指令指引】
- /help                         : 显示这份指令列表
- /status 或 /sysinfo           : 查看运行状态、系统指标与活跃工单
- /model                       : 查看当前模型
- /models                      : 查看可用模型
- /usage                       : 查看模型用量
- /tasks                        : 查看你在当前会话的近期工单
- /task <ID> [页码]             : 查看工单状态和输出
- 继续 或 /continue             : 续发未发送完的长回复
- /stop                         : 中断你发起的思考和工单
- /reset、/new 或 /clear        : 重开当前会话并清除近期历史
- /agy login [force]            : 发起或强制重新进行 Google 认证（私聊）
- /agy code <授权码>             : 提交 Google 授权码（私聊）
- /agy status                   : 查看 AGY 认证状态（私聊）
- /agy logout                   : 注销 AGY 并清除凭据（私聊）
- /pwd                          : 查看当前工作空间
- /cd <目标>                    : 切换工作空间
- /ws 或 /workspaces            : 列出已纳管的工作空间
- /ws rm <名称>                 : 移除工作空间书签
- /ws exit                      : 退出当前工作空间
- /ws clean 或 /clean           : 清理默认 sandbox 临时文件

工单、模型与工作空间指令需要相应权限；群聊中请 @ 呆呆后发送控制指令。`, a.Persona.Name)
			a.replyText(ctx, event, helpText)
			return

		default:
			if code, isAuthCodeCommand := parseAGYAuthCodeCommand(content); isAuthCodeCommand {
				if event.IsGroup {
					a.replyText(ctx, event, "授权码属于私人凭据，请勿在群聊中发送。请在与呆呆的私聊中提交授权码。")
					return
				}
				if err := a.delegationPermissionError(event.Role); err != nil {
					a.replyText(ctx, event, err.Error())
					return
				}
				if a.AGYAuth == nil {
					a.replyText(ctx, event, "AGY 认证管理器未挂载。")
					return
				}
				a.replyText(ctx, event, "正在验证并换取凭证，请稍候...")
				email, err := a.AGYAuth.SubmitCode(ctx, code)
				if err != nil {
					slog.Warn("superagent: AGY 授权码交换失败", "err", err)
					a.replyText(ctx, event, "认证未完成。请确认授权码来自最近一次登录链接且尚未过期；若仍失败，请检查服务日志。")
					return
				}
				if a.OnAuthSuccess != nil {
					a.OnAuthSuccess(ctx)
				}
				a.replyText(ctx, event, fmt.Sprintf("【AGY 登录成功】\n已成功绑定 Google 账号：%s！\n心智模型池已自动刷新就绪。", email))
				return
			}
			if strings.HasPrefix(content, "/ws rm ") || strings.HasPrefix(content, "/workspaces rm ") {
				unlock := a.lockSession(event)
				defer unlock()
				if a.Workspaces == nil {
					a.replyText(ctx, event, "工作空间管理器未配置。")
					return
				}
				target := strings.TrimSpace(strings.TrimPrefix(content, "/workspaces rm "))
				if strings.HasPrefix(content, "/ws rm ") {
					target = strings.TrimSpace(strings.TrimPrefix(content, "/ws rm "))
				}
				a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "remove", target, a.Delegates))
				return
			}

			if strings.HasPrefix(content, "/cd ") || content == "/cd" {
				unlock := a.lockSession(event)
				defer unlock()
				if a.Workspaces == nil {
					a.replyText(ctx, event, "工作空间管理器未配置。")
					return
				}
				target := strings.TrimSpace(strings.TrimPrefix(content, "/cd"))
				if target == "" {
					target = "~"
				}
				a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "switch", target, a.Delegates))
				return
			}
		}
	}

	// QQ attachments are never inspected just because they were uploaded. Ask
	// for an explicit task before invoking vision, cognition, or any tool.
	if event.Platform == "qq" && content == "" && (len(event.Images) > 0 || event.OmittedImages > 0 || len(event.Files) > 0 || event.OmittedFiles > 0) {
		attachmentCtx, attachmentCancel := context.WithCancel(ctx)
		cancellation := &thinkCancellation{key: thinkKey(event)}
		a.sessionCancels.Store(cancellation, attachmentCancel)
		defer func() {
			a.sessionCancels.Delete(cancellation)
			attachmentCancel()
		}()
		unlock := a.lockSession(event)
		defer unlock()
		if attachmentCtx.Err() != nil || (event.StillValid != nil && !event.StillValid()) {
			return
		}
		if event.IsPartnerPrivate() {
			a.deliverPendingNotifications(attachmentCtx, event)
			if attachmentCtx.Err() != nil || (event.StillValid != nil && !event.StillValid()) {
				return
			}
		}
		if len(event.Images) == 0 && len(event.Files) == 0 {
			message := "我收到附件消息，但本轮附件没有接收完整。请重新发送要处理的附件，并告诉我希望怎么处理。"
			if event.OmittedImages > 0 || event.OmittedFiles > 0 {
				message = fmt.Sprintf("我收到附件消息，但本轮有 %d 张图片和 %d 个文件因数量上限未接收。请重新发送要处理的附件，并告诉我希望怎么处理。", event.OmittedImages, event.OmittedFiles)
			}
			if attachmentCtx.Err() == nil && (event.StillValid == nil || event.StillValid()) {
				a.replyText(attachmentCtx, event, message)
			}
			return
		}
		message := "我收到了附件，还没有查看或读取。你想让我怎么处理？例如描述图片、总结文件、提取信息或转换格式。"
		if event.OmittedImages > 0 {
			message += fmt.Sprintf("另外有 %d 张图片因本轮数量上限未接收；需要处理的话请重新发送。", event.OmittedImages)
		}
		if event.OmittedFiles > 0 {
			message += fmt.Sprintf("另外有 %d 个文件因本轮数量上限未接收；需要处理的话请重新发送。", event.OmittedFiles)
		}
		if attachmentCtx.Err() == nil && (event.StillValid == nil || event.StillValid()) {
			a.replyText(attachmentCtx, event, message)
		}
		return
	}

	var thinkCtx context.Context
	if a.Cognition != nil {
		var thinkCancel context.CancelFunc
		thinkCtx, thinkCancel = context.WithCancel(ctx)
		if a.Channels != nil {
			thinkCtx = WithChannelStatus(thinkCtx, a.Channels.FormatStatus())
		}
		cancellation := &thinkCancellation{key: thinkKey(event)}
		a.sessionCancels.Store(cancellation, thinkCancel)
		defer func() {
			a.sessionCancels.Delete(cancellation)
			thinkCancel()
		}()
	}

	// Register cancellation before waiting on the shared channel lock so /stop
	// can also invalidate this request during the lock-acquisition window.
	unlock := a.lockSession(event)
	defer unlock()
	if thinkCtx != nil && thinkCtx.Err() != nil {
		return
	}
	if event.StillValid != nil && !event.StillValid() {
		return
	}
	// Control commands remain immediate. Ordinary private messages deliver
	// durable notifications only after acquiring the session lock and validating
	// their queue generation, so a stale pre-reset event cannot trigger a send.
	if content != "继续" && content != "/continue" && !strings.HasPrefix(content, "/") {
		notificationCtx := ctx
		if thinkCtx != nil {
			notificationCtx = thinkCtx
		}
		a.deliverPendingNotifications(notificationCtx, event)
		if notificationCtx.Err() != nil {
			return
		}
	}

	if event.OnProgress == nil {
		event.OnProgress = func(progressText string) {
			progressCtx := ctx
			if thinkCtx != nil {
				progressCtx = thinkCtx
			}
			if strings.TrimSpace(progressText) == "" || progressCtx.Err() != nil {
				return
			}
			pCtx, cancel := context.WithTimeout(progressCtx, 15*time.Second)
			defer cancel()
			a.replyText(pCtx, event, progressText)
		}
	}

	if a.Cognition != nil {
		utterance, err := a.Cognition.Think(thinkCtx, event)
		if err != nil {
			if event.GroupMessageObserved {
				slog.Warn("superagent: 群聊插话判断失败，已静默记录消息", "channel", event.ChannelID, "err", err)
				a.commitObservedGroupMessage(event)
				return
			}
			if thinkCtx.Err() != nil || (event.StillValid != nil && !event.StillValid()) {
				slog.Info("superagent: 当前会话思考上下文已结束", "channel", event.ChannelID, "err", thinkCtx.Err())
				return
			}
			event.ProcessedAttachments = nil
			slog.Error("superagent: 思考过程发生异常", "err", err)
			errorReply := "这次处理没有成功，详细错误已记入本机日志。请稍后重试；如果连续失败，我可以继续排查。"
			sendCtx, cancel := context.WithTimeout(thinkCtx, 15*time.Second)
			defer cancel()
			if sendErr := a.transmitReplyText(sendCtx, event, errorReply); sendErr != nil {
				return
			}
			if committer, ok := a.Cognition.(interface {
				CommitDeliveredReply(context.Context, *SensoryEvent, string)
			}); ok {
				commitCtx, commitCancel := context.WithTimeout(thinkCtx, 15*time.Second)
				committer.CommitDeliveredReply(commitCtx, event, errorReply)
				commitCancel()
			}
			return
		}
		if utterance == nil {
			if event.GroupMessageObserved {
				a.commitObservedGroupMessage(event)
			}
			return
		}

		if utterance != nil {
			if thinkCtx.Err() != nil {
				return
			}
			if !event.IsPartnerPrivate() {
				utterance.Text = core.RedactSensitiveIPs(utterance.Text)
				utterance.FallbackText = core.RedactSensitiveIPs(utterance.FallbackText)
			}
			slog.Info("superagent: 认知合成完毕准备发声", "channel", utterance.ChannelID, "textBytes", len(utterance.Text), "hasMedia", utterance.HasMedia())
			sendCtx, cancel := context.WithTimeout(thinkCtx, 30*time.Second)
			deliveredText, sendErr := a.transmitUtterance(sendCtx, utterance, event.SenderID)
			cancel()

			replyText := deliveredText
			if sendErr != nil {
				slog.Error("superagent: 回复发送失败", "channel", utterance.ChannelID, "err", sendErr)
				replyText = ""
			}
			event.ReplyDelivered = sendErr == nil
			if committer, ok := a.Cognition.(interface {
				CommitDeliveredReply(context.Context, *SensoryEvent, string)
			}); ok {
				commitCtx, commitCancel := context.WithTimeout(ctx, 15*time.Second)
				committer.CommitDeliveredReply(commitCtx, event, replyText)
				commitCancel()
			}
		}
	}
}
