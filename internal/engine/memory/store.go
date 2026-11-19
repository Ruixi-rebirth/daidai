package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
	"daidai/internal/groupchat"
)

const (
	maxWorkingChars      = 12000
	maxIdleGap           = 2 * time.Hour
	maxBridgeAge         = 24 * time.Hour
	maxBridgeActiveMsgs  = 4
	defaultMaxFacts      = 40
	maxPrevSessionMsgs   = 8
	maxPrevSessionRunes  = 1500
	maxSingleDigestRunes = 160
)

func boundedFactContent(content string) string {
	runes := []rune(content)
	if len(runes) <= core.MaxFactContentRunes {
		return content
	}
	return string(runes[:core.MaxFactContentRunes]) + "…"
}

var (
	defaultEmbedderMu      sync.RWMutex
	defaultEmbedder        core.EmbeddingProvider
	defaultEmbedderVersion uint64
	embedCacheMu           sync.RWMutex
	embedCache             = make(map[embedCacheKey][]float32, 256)
)

type embedCacheKey struct {
	version  uint64
	identity string
	text     string
}

// SetDefaultEmbedder 设置全局默认稠密向量编码器 (如 BAAI/bge-m3)
func SetDefaultEmbedder(emb core.EmbeddingProvider) {
	defaultEmbedderMu.Lock()
	defaultEmbedder = emb
	defaultEmbedderVersion++
	defaultEmbedderMu.Unlock()

	embedCacheMu.Lock()
	embedCache = make(map[embedCacheKey][]float32, 256)
	embedCacheMu.Unlock()
}

// GetDefaultEmbedder 获取全局默认稠密向量编码器
func GetDefaultEmbedder() core.EmbeddingProvider {
	emb, _ := defaultEmbedderSnapshot()
	return emb
}

func defaultEmbedderSnapshot() (core.EmbeddingProvider, uint64) {
	defaultEmbedderMu.RLock()
	defer defaultEmbedderMu.RUnlock()
	return defaultEmbedder, defaultEmbedderVersion
}

// EmbedText 使用当前配置的向量模型编码文本（带内存缓存，未配置或超时时静默返回 nil）
func EmbedText(ctx context.Context, text string) []float32 {
	vec, _ := EmbedTextWithIdentity(ctx, text)
	return vec
}

func EmbedTextWithIdentity(ctx context.Context, text string) ([]float32, string) {
	emb, version := defaultEmbedderSnapshot()
	clean := strings.TrimSpace(text)
	if emb == nil || clean == "" {
		return nil, ""
	}
	identity := embeddingIdentity(emb)
	key := embedCacheKey{version: version, identity: identity, text: clean}

	embedCacheMu.RLock()
	if cached, ok := embedCache[key]; ok {
		embedCacheMu.RUnlock()
		return append([]float32(nil), cached...), identity
	}
	embedCacheMu.RUnlock()

	vec, actualIdentity, err := embedWithIdentity(ctx, emb, clean)
	if err != nil || len(vec) == 0 {
		return nil, ""
	}
	if actualIdentity == "" {
		actualIdentity = identity
	}

	embedCacheMu.Lock()
	if len(embedCache) >= 512 {
		for k := range embedCache {
			delete(embedCache, k)
			break
		}
	}
	if actualIdentity == identity {
		embedCache[key] = append([]float32(nil), vec...)
	}
	embedCacheMu.Unlock()
	return append([]float32(nil), vec...), actualIdentity
}

func embeddingIdentity(emb core.EmbeddingProvider) string {
	if emb == nil {
		return ""
	}
	if identified, ok := emb.(core.IdentifiedEmbeddingProvider); ok {
		if identity := strings.TrimSpace(identified.EmbeddingIdentity()); identity != "" {
			return identity
		}
	}
	return fmt.Sprintf("%T", emb)
}

func embedWithIdentity(ctx context.Context, emb core.EmbeddingProvider, text string) ([]float32, string, error) {
	if identified, ok := emb.(core.IdentifiedEmbeddingProvider); ok {
		return identified.EmbedIdentified(ctx, text)
	}
	vec, err := emb.Embed(ctx, text)
	return vec, embeddingIdentity(emb), err
}

// MemoryEngine 长期事实记忆与会话历史引擎 (直接实现 entity.Memory，纯粹 SQLite 驱动)
type MemoryEngine struct {
	db          *db.Manager
	llm         core.LLMProvider
	embedder    core.EmbeddingProvider
	mu          sync.RWMutex
	userLocksMu sync.Mutex
	userLocks   map[userLockKey]*userLockEntry
	asyncMu     sync.Mutex
	asyncWG     sync.WaitGroup
	asyncCtx    context.Context
	asyncCancel context.CancelFunc
	asyncClosed bool
}

type userLockKey struct {
	platform string
	userID   string
}

type userLockEntry struct {
	gate chan struct{}
	refs int
}

// NewMemoryEngine 实例化长期记忆引擎 (可选注入后台轻量 LLM 驱动实现全自动事实提炼与冲突消解)
func NewMemoryEngine(providers ...core.LLMProvider) *MemoryEngine {
	var p core.LLMProvider
	if len(providers) > 0 {
		p = providers[0]
	}
	var emb core.EmbeddingProvider
	if ep, ok := p.(core.EmbeddingProvider); ok {
		emb = ep
	}
	asyncCtx, asyncCancel := context.WithCancel(context.Background())
	return &MemoryEngine{
		db:          db.DefaultManager(),
		llm:         p,
		embedder:    emb,
		asyncCtx:    asyncCtx,
		asyncCancel: asyncCancel,
	}
}

// SetEmbedder 显式绑定稠密向量编码器 (如硅基流动 BAAI/bge-m3)，并同步设为全局默认编码器
func (m *MemoryEngine) SetEmbedder(emb core.EmbeddingProvider) {
	m.mu.Lock()
	m.embedder = emb
	m.mu.Unlock()
	SetDefaultEmbedder(emb)
}

func (m *MemoryEngine) getEmbedder() core.EmbeddingProvider {
	m.mu.RLock()
	emb := m.embedder
	m.mu.RUnlock()
	if emb != nil {
		return emb
	}
	return GetDefaultEmbedder()
}

func (m *MemoryEngine) lockUser(ctx context.Context, platform, userID string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := userLockKey{platform: db.NormPlatform(platform), userID: userID}
	m.userLocksMu.Lock()
	if m.userLocks == nil {
		m.userLocks = make(map[userLockKey]*userLockEntry)
	}
	entry := m.userLocks[key]
	if entry == nil {
		entry = &userLockEntry{gate: make(chan struct{}, 1)}
		m.userLocks[key] = entry
	}
	entry.refs++
	m.userLocksMu.Unlock()

	releaseRef := func() {
		m.userLocksMu.Lock()
		entry.refs--
		if entry.refs == 0 && m.userLocks[key] == entry {
			delete(m.userLocks, key)
		}
		m.userLocksMu.Unlock()
	}
	select {
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	case entry.gate <- struct{}{}:
		return func() {
			<-entry.gate
			releaseRef()
		}, nil
	}
}

// SetDBManager 设置自定义数据库管理器 (单测或特殊隔离环境使用)
func (m *MemoryEngine) SetDBManager(mgr *db.Manager) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.db = mgr
}

func (m *MemoryEngine) getDB() *db.Manager {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.db
}

// WorkingContext 获取近期滑动窗口工作记忆 (实现 entity.Memory)
func (m *MemoryEngine) WorkingContext(ctx context.Context, event *entity.SensoryEvent, limit int) []core.ChatMessage {
	dbMgr := m.getDB()
	if dbMgr == nil || event == nil || event.Platform == "" {
		return nil
	}

	chatType := "private"
	if event.IsGroup {
		chatType = "group"
	}

	boundary := event.HistoryBoundaryID()
	recents, err := dbMgr.RecentMessagesBefore(ctx, event.Platform, chatType, event.ChannelID, limit, boundary)
	if err != nil || len(recents) == 0 {
		return nil
	}

	anchorTime := event.OccurredAt
	if anchorTime.IsZero() {
		anchorTime = time.Now()
	}

	// 从最新向最旧反向遍历，累计字符预算与时间连续性：超出字符上限或闲置断档超 2 小时即截断，防止陈旧会话污染当前活跃轮次
	var selected []core.ChatMessage
	totalChars := 0
	prevTime := anchorTime
	for i := len(recents) - 1; i >= 0; i-- {
		h := recents[i]
		if !h.CreatedAt.IsZero() && prevTime.Sub(h.CreatedAt) > maxIdleGap {
			break
		}
		if !h.CreatedAt.IsZero() {
			prevTime = h.CreatedAt
		}
		role := core.RoleUser
		content := h.Text
		if h.IsBot {
			role = core.RoleAssistant
		} else if h.IsGroup {
			speaker := h.Name
			if speaker == "" {
				speaker = "群成员#" + shortID(h.UserID)
			}
			content = fmt.Sprintf("[%s]: %s", speaker, h.Text)
		}
		content = core.TruncateRunes(content, core.MaxUserMessageRunes, "\n[历史消息过长，后续内容已截断]")
		cLen := utf8.RuneCountInString(content)
		if len(selected) > 0 && totalChars+cLen > maxWorkingChars {
			break
		}
		totalChars += cLen
		selected = append(selected, core.ChatMessage{
			Role:    role,
			Content: content,
		})
	}

	// 逆序翻转回正序 (按时间自然演进呈现给大模型)
	msgs := make([]core.ChatMessage, len(selected))
	for i, msg := range selected {
		msgs[len(selected)-1-i] = msg
	}
	return msgs
}

func shortID(id string) string {
	if len(id) > 6 {
		return id[:6]
	}
	return id
}

// RecallEpisodic 回忆关于用户的长期情景记忆画像与跨场次历史脉络 (实现 entity.Memory)
func (m *MemoryEngine) RecallEpisodic(ctx context.Context, event *entity.SensoryEvent) string {
	if event == nil || event.Platform == "" || event.SenderID == "" {
		return ""
	}

	var sections []string

	facts, err := m.RecallProfileForEvent(ctx, event, event.Content, defaultMaxFacts)
	if err == nil && len(facts) > 0 {
		var lines []string
		for _, f := range facts {
			lines = append(lines, fmt.Sprintf("- [ID: %d][%s] %s", f.ID, f.ScopeLabel(), boundedFactContent(f.Content)))
		}
		if event.SenderName != "" {
			sections = append(sections, fmt.Sprintf("【当前对话用户 (%s) 的特征与长期记忆】：\n%s", event.SenderName, strings.Join(lines, "\n")))
		} else {
			sections = append(sections, fmt.Sprintf("【用户特征与长期记忆】：\n%s", strings.Join(lines, "\n")))
		}
	}

	if digest := m.recallPreviousSessionDigest(ctx, event); digest != "" {
		sections = append(sections, digest)
	}

	return strings.Join(sections, "\n\n")
}

// recallPreviousSessionDigest 提取因超过 2 小时闲置断档而退出活跃滑动窗口的上一场对话脉络摘要，避免跨时段失忆
func (m *MemoryEngine) recallPreviousSessionDigest(ctx context.Context, event *entity.SensoryEvent) string {
	dbMgr := m.getDB()
	if dbMgr == nil || event == nil || event.Platform == "" || event.ChannelID == "" {
		return ""
	}

	chatType := "private"
	if event.IsGroup {
		chatType = "group"
	}

	boundary := event.HistoryBoundaryID()
	recents, err := dbMgr.RecentMessagesBefore(ctx, event.Platform, chatType, event.ChannelID, 30, boundary)
	if err != nil || len(recents) == 0 {
		return ""
	}

	anchorTime := event.OccurredAt
	if anchorTime.IsZero() {
		anchorTime = time.Now()
	}

	cutIdx := -1
	prevTime := anchorTime
	for i := len(recents) - 1; i >= 0; i-- {
		h := recents[i]
		if !h.CreatedAt.IsZero() && prevTime.Sub(h.CreatedAt) > maxIdleGap {
			cutIdx = i
			break
		}
		if !h.CreatedAt.IsZero() {
			prevTime = h.CreatedAt
		}
	}

	if cutIdx < 0 {
		return ""
	}

	// 仅在新一场对话刚开始（活跃轮次 <= 4 条）且上一场对话发生在 24 小时以内时提供跨场次过渡脉络；
	// 一旦新话题已深入展开或旧对话已是数天前的陈年记录，不再每轮重复注入旧摘要
	activeMsgs := len(recents) - 1 - cutIdx
	if activeMsgs > maxBridgeActiveMsgs {
		return ""
	}
	if lastOld := recents[cutIdx].CreatedAt; !lastOld.IsZero() && anchorTime.Sub(lastOld) > maxBridgeAge {
		return ""
	}

	older := recents[:cutIdx+1]
	if len(older) > maxPrevSessionMsgs {
		older = older[len(older)-maxPrevSessionMsgs:]
	}

	var lines []string
	totalRunes := 0
	for _, h := range older {
		speaker := "用户"
		if h.IsBot {
			speaker = config.BotName()
		} else if h.Name != "" {
			speaker = h.Name
		}
		text := strings.ReplaceAll(strings.TrimSpace(h.Text), "\n", " ")
		if r := []rune(text); len(r) > maxSingleDigestRunes {
			text = string(r[:maxSingleDigestRunes]) + "..."
		}
		ts := ""
		if !h.CreatedAt.IsZero() {
			ts = h.CreatedAt.Format("01-02 15:04") + " "
		}
		line := fmt.Sprintf("- [%s%s]: %s", ts, speaker, text)
		lRunes := utf8.RuneCountInString(line)
		if totalRunes+lRunes > maxPrevSessionRunes {
			break
		}
		totalRunes += lRunes
		lines = append(lines, line)
	}

	if len(lines) == 0 {
		return ""
	}

	return fmt.Sprintf("【上一场对话历史脉络（距今已间隔超 2 小时，仅供跨场次背景参考）】：\n%s", strings.Join(lines, "\n"))
}

func historyUserText(event *entity.SensoryEvent) string {
	if event == nil {
		return ""
	}
	userText := strings.TrimSpace(core.TruncateRunes(event.Content, core.MaxUserMessageRunes, "\n[消息过长，后续内容已截断]"))
	if entity.IsSensitiveAuthCommand(userText) {
		userText = "[OAuth authorization code redacted]"
	}
	if note := strings.TrimSpace(event.HistoryAttachmentNote); note != "" {
		if userText != "" {
			userText += "\n"
		}
		userText += note
	}
	if summary := strings.TrimSpace(core.TruncateRunes(event.VisionSummary, core.MaxVisionSummaryRunes, "\n[视觉摘要过长，后续内容已截断]")); summary != "" {
		if userText != "" {
			userText += "\n"
		}
		userText += "[图片内容: " + summary + "]"
	}
	return core.TruncateRunes(userText, core.MaxUserMessageRunes, "\n[消息过长，后续内容已截断]")
}

func shouldRecordHistoryEvent(event *entity.SensoryEvent) bool {
	if event == nil {
		return false
	}
	content := strings.TrimSpace(event.Content)
	if strings.HasPrefix(content, "/") || content == "继续" || entity.IsSensitiveAuthCommand(content) {
		return false
	}
	return historyUserText(event) != ""
}

// RecordInbound persists each accepted raw user message before debounce or cognition can fail.
func (m *MemoryEngine) RecordInbound(ctx context.Context, event *entity.SensoryEvent) error {
	if m == nil || event == nil || event.Platform == "" || event.LastHistoryRecordID() > 0 || !shouldRecordHistoryEvent(event) {
		return nil
	}
	dbMgr := m.getDB()
	if dbMgr == nil {
		return nil
	}
	chatType := "private"
	if event.IsGroup {
		chatType = "group"
	}
	id, err := dbMgr.LogInboundMessage(ctx, event.Platform, chatType, event.ChannelID, event.SenderID, event.SenderName, historyUserText(event), event.RawMessageID)
	if err != nil {
		return err
	}
	event.HistoryRecordIDs = []int64{id}
	return nil
}

// Commit stores only delivered bot output and attaches derived vision text to the already persisted user row.
func (m *MemoryEngine) Commit(ctx context.Context, event *entity.SensoryEvent, replyText string) {
	dbMgr := m.getDB()
	if dbMgr == nil || event == nil || event.Platform == "" {
		return
	}

	chatType := "private"
	if event.IsGroup {
		chatType = "group"
	}

	visionSummary := core.TruncateRunes(event.VisionSummary, core.MaxVisionSummaryRunes, "\n[视觉摘要过长，后续内容已截断]")
	replyText = core.TruncateRunes(replyText, core.MaxUserMessageRunes, "\n[消息过长，后续内容已截断]")

	var userMsgID, botMsgID int64
	userText := ""
	if shouldRecordHistoryEvent(event) {
		if event.LastHistoryRecordID() == 0 {
			if err := m.RecordInbound(ctx, event); err != nil {
				slog.Warn("memory: 保存入站消息失败", "platform", event.Platform, "channel", event.ChannelID, "err", err)
			}
		}
		userMsgID = event.LastHistoryRecordID()
		var err error
		if userMsgID > 0 && visionSummary != "" {
			userText, err = dbMgr.AppendMessageText(ctx, event.Platform, userMsgID, "[图片内容: "+visionSummary+"]")
		} else if userMsgID > 0 {
			userText, err = dbMgr.MessageTextByID(ctx, event.Platform, userMsgID)
		}
		if err != nil {
			slog.Warn("memory: 更新入站消息记录失败", "platform", event.Platform, "channel", event.ChannelID, "err", err)
		}
	}

	if replyText != "" {
		var err error
		historyText := replyText
		if event.IsGroup && event.GroupExpression == groupchat.Dream {
			historyText = fmt.Sprintf("[%s的梦境联想：虚构故事，不是群成员的真实经历]\n%s", config.BotName(), replyText)
		}
		botMsgID, err = dbMgr.LogBotReply(ctx, event.Platform, chatType, event.ChannelID, historyText, event.SenderID, userMsgID)
		if err != nil {
			slog.Warn("memory: 保存机器人回复失败", "platform", event.Platform, "channel", event.ChannelID, "err", err)
		}
	}

	emb := m.getEmbedder()
	needExtract := m.llm != nil && event.SenderID != "" && shouldExtractFacts(event)
	if needExtract || emb != nil {
		m.asyncMu.Lock()
		if m.asyncClosed {
			m.asyncMu.Unlock()
			return
		}
		m.asyncWG.Add(1)
		asyncCtx := m.asyncCtx
		m.asyncMu.Unlock()

		evCopy := *event
		evCopy.HistoryRecordIDs = append([]int64(nil), event.HistoryRecordIDs...)
		evCopy.Content = core.TruncateRunes(evCopy.Content, core.MaxUserMessageRunes, "\n[消息过长，后续内容已截断]")
		evCopy.VisionSummary = visionSummary
		if evCopy.ReplyContext != nil {
			replyCopy := *evCopy.ReplyContext
			replyCopy.Content = entity.SanitizeQuotedMessageContent(replyCopy.Content)
			evCopy.ReplyContext = &replyCopy
		}
		go func(ev entity.SensoryEvent, uText, rText string, uID, bID int64) {
			defer m.asyncWG.Done()
			if needExtract {
				factContext := rText
				if ev.GroupExpression != "" {
					factContext = ""
				}
				m.consolidateUserFacts(asyncCtx, &ev, factContext)
			}
			if emb == nil || asyncCtx.Err() != nil {
				return
			}
			bgCtx, cancel := context.WithTimeout(asyncCtx, 25*time.Second)
			defer cancel()

			if uID > 0 && len([]rune(uText)) >= 4 {
				if vec, identity, err := embedWithIdentity(bgCtx, emb, uText); err == nil && len(vec) > 0 && identity != "" && bgCtx.Err() == nil {
					_ = dbMgr.UpdateMessageEmbedding(bgCtx, ev.Platform, uID, vec, identity)
				}
			}
			if bID > 0 && ev.GroupExpression == "" && len([]rune(rText)) >= 6 {
				if vec, identity, err := embedWithIdentity(bgCtx, emb, rText); err == nil && len(vec) > 0 && identity != "" && bgCtx.Err() == nil {
					_ = dbMgr.UpdateMessageEmbedding(bgCtx, ev.Platform, bID, vec, identity)
				}
			}
			if bgCtx.Err() != nil {
				return
			}

			if ev.SenderID != "" {
				m.backfillUserFactEmbeddings(bgCtx, &ev)
			}
		}(evCopy, userText, replyText, userMsgID, botMsgID)
	}
}

// Shutdown 停止后台记忆任务，并等待其退出或达到调用方的停机期限。
func (m *MemoryEngine) Shutdown(ctx context.Context) error {
	m.asyncMu.Lock()
	m.asyncClosed = true
	if m.asyncCancel != nil {
		m.asyncCancel()
	}
	m.asyncMu.Unlock()

	done := make(chan struct{})
	go func() {
		m.asyncWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *MemoryEngine) backfillUserFactEmbeddings(ctx context.Context, event *entity.SensoryEvent) {
	dbMgr := m.getDB()
	emb := m.getEmbedder()
	if dbMgr == nil || emb == nil || event == nil || event.Platform == "" || event.SenderID == "" {
		return
	}
	visibleFacts, err := dbMgr.ListVisibleFacts(ctx, event.Platform, event.SenderID, event.ChannelID, event.IsGroup)
	if err != nil {
		return
	}
	for _, f := range visibleFacts {
		if ctx.Err() != nil {
			return
		}
		if f.EmbeddingModel != embeddingIdentity(emb) && f.Content != "" {
			if vec, identity, err := embedWithIdentity(ctx, emb, f.Content); err == nil && len(vec) > 0 && identity != "" {
				if ctx.Err() != nil {
					return
				}
				_ = dbMgr.UpdateFactEmbedding(ctx, event.Platform, f.ID, f.Content, vec, identity)
			}
		}
	}
}

func shouldExtractFacts(event *entity.SensoryEvent) bool {
	if event == nil {
		return false
	}
	text := strings.TrimSpace(event.Content)
	if text == "" || strings.HasPrefix(text, "/") {
		return false
	}
	meaningful := 0
	for _, r := range text {
		if !isSeparatorRune(r) {
			meaningful++
		}
	}
	return meaningful >= 4 || event.VisionSummary != ""
}

type factExtractionResponse struct {
	Mutations []db.FactMutation `json:"mutations"`
}

const (
	maxFactMutationResponseBytes = 64 << 10
	maxFactMutationsPerTurn      = 20
	maxFactContentRunes          = 40
)

func (m *MemoryEngine) consolidateUserFacts(ctx context.Context, event *entity.SensoryEvent, replyText string) {
	dbMgr := m.getDB()
	if dbMgr == nil || m.llm == nil || event == nil || event.SenderID == "" {
		return
	}

	unlock, err := m.lockUser(ctx, event.Platform, event.SenderID)
	if err != nil {
		return
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	activeFacts, err := dbMgr.ListVisibleFacts(ctx, event.Platform, event.SenderID, event.ChannelID, event.IsGroup)
	if err != nil {
		return
	}

	turnQuery := event.Content + " " + replyText
	rankCtx, rankCancel := context.WithTimeout(ctx, 3*time.Second)
	relevantFacts := RankFactsByBM25(rankCtx, turnQuery, activeFacts, 0)
	rankCancel()
	if len(relevantFacts) > 25 {
		relevantFacts = relevantFacts[:25]
	}

	var existingLines []string
	for _, f := range relevantFacts {
		existingLines = append(existingLines, fmt.Sprintf("[ID: %d][%s] %s", f.ID, f.ScopeLabel(), boundedFactContent(f.Content)))
	}
	existingBlock := "（暂无已记录事实）"
	if len(existingLines) > 0 {
		existingBlock = strings.Join(existingLines, "\n")
	}

	var turnBuilder strings.Builder
	if event.ReplyContext != nil && strings.TrimSpace(event.ReplyContext.Content) != "" {
		quotedContent := entity.SanitizeQuotedMessageContent(event.ReplyContext.Content)
		turnBuilder.WriteString("用户引用的消息（说话人可能不是当前用户，仅供语境参考）: " + quotedContent + "\n")
	}
	turnBuilder.WriteString("用户发言: " + strings.TrimSpace(event.Content))
	if event.VisionSummary != "" {
		turnBuilder.WriteString("\n用户附图摘要（仅供语境参考，不能单独证明内容属于当前用户）: " + event.VisionSummary)
	}
	if r := []rune(strings.TrimSpace(replyText)); len(r) > 0 {
		if len(r) > 240 {
			r = r[:240]
		}
		turnBuilder.WriteString(fmt.Sprintf("\n%s回应(仅作语境参考): %s", config.BotName(), string(r)))
	}

	chatType := "private"
	if event.IsGroup {
		chatType = "group"
	}
	recentMsgs, _ := dbMgr.RecentMessages(ctx, event.Platform, chatType, event.ChannelID, 6)
	var historyBuilder strings.Builder
	if len(recentMsgs) > 0 {
		for _, msg := range recentMsgs {
			if msg.IsBot {
				continue
			}
			if msg.UserID == event.SenderID && strings.TrimSpace(msg.Text) == strings.TrimSpace(event.Content) {
				continue
			}
			speaker := strings.TrimSpace(msg.Name)
			if speaker == "" {
				speaker = msg.UserID
			}
			trimmed := core.TruncateRunes(strings.TrimSpace(msg.Text), 100, "...")
			if trimmed != "" {
				historyBuilder.WriteString(fmt.Sprintf("- %s: %s\n", speaker, trimmed))
			}
		}
	}
	if historyBuilder.Len() > 0 {
		turnBuilder.WriteString("\n\n【近期上下文语境】（帮助判断发言讨论的真正主体是外部话题/他人还是当前用户）：\n" + historyBuilder.String())
	}

	sysPrompt := fmt.Sprintf(`你是%s的长期记忆提炼与冲突消解引擎。
请根据【用户已知事实列表】、【本轮对话】与【近期上下文语境】，深入结合语境判断是否有关于当前发言用户的长期个人特征、身份背景、技术栈、生活偏好、重要计划或近况变化值得记录或修正。

智能语义与语境分析准则：
1. 【深入结合上下文判断陈述主体与指代】：
   - 必须结合【近期上下文语境】分析本轮发言的真实讨论对象。口语交流中常会省略主语，你必须根据对话流准确判定：
     * 若当前群聊/对话正在讨论外部话题、特定群体（如“上海考生”、“大厂员工”、“他们/大家”）、社会现象、影视游戏或外部组件，且用户的发言是在阐述、评论、吐槽或补充该外部对象（例如讨论高难度考试时说“从小就看英文书籍电影游戏啊，你拿头打，对他们来说英文和中文没差的”），其陈述主体显然是外部群体而非发言者本人，严禁将其误提取为发言者的个人事实！
     * 若结合上下文，发言明显是关于用户【本人】的真实经历、技能、偏好或近况（例如“平时主力用 NixOS”、“家里养了只布偶猫”、“准备考研”），即使省略了“我”，只要在语境下确指用户本人，均应提炼。
   - 若结合上下文仍不足以确定主体是指代用户本人还是泛指他人，保持克制，不要主观臆造事实。
2. 引用消息中的说话人可能是其他人，图片摘要也可能描述他人或外部内容；二者只能作语境参考，不能单独证明事实属于当前用户。%s的回复同样不能作为事实证据。
3. 对照【用户已知事实列表】：
   - 只能使用【用户已知事实列表】中当前用户可见的事实；群聊只会收到“当前用户在本群记录的个人事实”，只允许更新或删除这些事实，不得读取或修改私聊中的全局事实；
   - 若新信息与当前可修改范围内的某条旧事实属同一维度且发生变更或补充，输出 UPDATE 并填写对应 fact_id 与合并后的完整精炼陈述；
   - 若用户明确否定或废除当前可修改范围内的旧事实，输出 DELETE 并填写对应 fact_id；
   - 若是全新的个人事实且列表中未包含，输出 ADD；
   - 若本轮对话无任何值得沉淀的长期事实，返回空数组 {"mutations": []}。
4. 每条 fact 陈述必须简明扼要（不超过 40 字），只输出合法 JSON 对象，格式如下：
{"mutations": [{"action": "ADD|UPDATE|DELETE", "fact_id": 0, "fact": "..."}]}`, config.BotName(), config.BotName())

	userPrompt := fmt.Sprintf("【用户已知事实列表】：\n%s\n\n【本轮对话】：\n%s", existingBlock, turnBuilder.String())

	rawResp, err := m.llm.Chat(ctx, []core.ChatMessage{
		{Role: core.RoleSystem, Content: sysPrompt},
		{Role: core.RoleUser, Content: userPrompt},
	}, &core.ChatOptions{
		Temperature: 0.1,
		MaxTokens:   512,
	})
	if err != nil {
		slog.Debug("memory: 后台记忆提炼请求跳过", "err", err)
		return
	}
	if ctx.Err() != nil {
		return
	}

	mutations := parseFactMutations(rawResp)
	if len(mutations) == 0 {
		return
	}

	normalized := normalizeFactMutations(event, relevantFacts, mutations)

	if len(normalized) > 0 {
		if ctx.Err() != nil {
			return
		}
		if err := dbMgr.ApplyFactMutations(ctx, event.Platform, event.SenderID, normalized); err != nil {
			slog.Warn("memory: 后台应用事实演进失败", "err", err)
		} else {
			slog.Info("memory: 后台自动沉淀用户记忆完成", "userID", event.SenderID, "count", len(normalized))
		}
	}
}

// normalizeFactMutations binds model decisions to the facts actually supplied
// to it. Similarity is useful for recall, but cannot authorize an overwrite.
// Exact ADD deduplication and evidence linking belong to the DB transaction.
func normalizeFactMutations(event *entity.SensoryEvent, known []*core.Fact, mutations []db.FactMutation) []db.FactMutation {
	scopeType, scopeID := factScope(event)
	seenIDs := make(map[int64]bool)
	seenAdds := make(map[string]bool)
	normalized := make([]db.FactMutation, 0, len(mutations))
	for _, mutation := range mutations {
		bound := db.FactMutation{
			Action: mutation.Action, Fact: mutation.Fact,
			SourceMessageIDs: append([]int64(nil), event.HistoryRecordIDs...),
			ScopeType:        scopeType, ScopeID: scopeID, Quote: event.Content,
		}
		switch mutation.Action {
		case "ADD":
			if mutation.Fact == "" || seenAdds[mutation.Fact] {
				continue
			}
			seenAdds[mutation.Fact] = true
		case "UPDATE", "DELETE":
			old := findFact(known, mutation.FactID)
			if old == nil || !sameFactScope(old, event) || seenIDs[old.ID] {
				continue
			}
			if mutation.Action == "UPDATE" && mutation.Fact == "" {
				continue
			}
			bound.FactID, bound.ExpectedFact = old.ID, old.Content
			seenIDs[old.ID] = true
		default:
			continue
		}
		normalized = append(normalized, bound)
	}
	return normalized
}

func parseFactMutations(raw string) []db.FactMutation {
	text := strings.TrimSpace(raw)
	if text == "" || len(text) > maxFactMutationResponseBytes {
		return nil
	}
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start >= 0 && end > start {
		text = text[start : end+1]
	}
	var parsed factExtractionResponse
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return nil
	}
	if len(parsed.Mutations) > maxFactMutationsPerTurn {
		return nil
	}

	valid := make([]db.FactMutation, 0, len(parsed.Mutations))
	seenFactIDs := make(map[int64]struct{})
	for _, mut := range parsed.Mutations {
		mut.Action = strings.ToUpper(strings.TrimSpace(mut.Action))
		mut.Fact = strings.TrimSpace(mut.Fact)
		switch mut.Action {
		case "ADD":
			if mut.Fact == "" || utf8.RuneCountInString(mut.Fact) > maxFactContentRunes {
				continue
			}
		case "UPDATE":
			if mut.FactID <= 0 || mut.Fact == "" || utf8.RuneCountInString(mut.Fact) > maxFactContentRunes {
				continue
			}
			if _, exists := seenFactIDs[mut.FactID]; exists {
				continue
			}
			seenFactIDs[mut.FactID] = struct{}{}
		case "DELETE":
			if mut.FactID <= 0 {
				continue
			}
			if _, exists := seenFactIDs[mut.FactID]; exists {
				continue
			}
			seenFactIDs[mut.FactID] = struct{}{}
		default:
			continue
		}
		valid = append(valid, mut)
	}
	return valid
}

type scoredFact struct {
	fact  *core.Fact
	score float32
}

// ScoreFactRelevance 计算单条事实与查询词的综合相关度
func ScoreFactRelevance(query, content string) float32 {
	scores := ScoreBM25(query, []string{content})
	if len(scores) == 0 {
		return 0
	}
	return scores[0]
}

// RankFactsByBM25 对候选事实列表执行 BM25 稀疏检索 + BAAI/bge-m3 稠密向量 RRF 双路混合重排 + 时间衰减，并按阈值过滤
func RankFactsByBM25(ctx context.Context, query string, facts []*core.Fact, minScore float32) []*core.Fact {
	if len(facts) == 0 {
		return nil
	}
	docs := make([]string, len(facts))
	hasFactEmb := false
	queryIdentity := embeddingIdentity(GetDefaultEmbedder())
	for i, f := range facts {
		docs[i] = f.Content
		if len(f.Embedding) > 0 && f.EmbeddingModel == queryIdentity {
			hasFactEmb = true
		}
	}
	bm25Scores := ScoreBM25(query, docs)

	// 仅在主动检索过滤 (minScore > 0) 或存量事实数超过常驻窗口上限需要淘汰截断时请求查询向量；
	// 若全量事实本就少于常驻上限且无需过滤，直接本地 0ms 完成 BM25 + 时间衰减排序，避免主对话热路径发生不必要的网络等待
	var vecScores []float32
	if hasFactEmb && strings.TrimSpace(query) != "" && (minScore > 0 || len(facts) > defaultMaxFacts) {
		if qVec, identity := EmbedTextWithIdentity(ctx, query); len(qVec) > 0 && identity == queryIdentity {
			vecScores = make([]float32, len(facts))
			for i, f := range facts {
				if len(f.Embedding) > 0 && f.EmbeddingModel == identity {
					vecScores[i] = CosineSimilarity(qVec, f.Embedding)
				}
			}
		}
	}

	fusedScores := FuseRRF(bm25Scores, vecScores, 60.0)
	now := time.Now()

	scored := make([]scoredFact, 0, len(facts))
	for i, f := range facts {
		var s float32
		if i < len(fusedScores) {
			s = fusedScores[i]
		}
		if s < minScore {
			continue
		}
		if !f.UpdatedAt.IsZero() {
			days := now.Sub(f.UpdatedAt).Hours() / 24.0
			if days < 0 {
				days = 0
			}
			s += float32(0.15 * math.Exp(-days/90.0))
		}
		scored = append(scored, scoredFact{
			fact:  f,
			score: s,
		})
	}

	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].fact.ID > scored[j].fact.ID
	})

	results := make([]*core.Fact, 0, len(scored))
	for _, item := range scored {
		results = append(results, item.fact)
	}
	return results
}

// SearchMessagesByBM25 从 SQLite 执行 BM25 稀疏词元 + BAAI/bge-m3 稠密向量双路召回，经 RRF 融合精选 Top-K（最终按时间正序返回）
func SearchMessagesByBM25(ctx context.Context, dbMgr *db.Manager, platform, convID, query string, limit int) ([]db.StoredMessage, error) {
	return searchMessagesByBM25(ctx, dbMgr, platform, "", convID, query, limit)
}

// SearchMessagesInChatByBM25 keeps lexical and vector recall inside one chat type.
func SearchMessagesInChatByBM25(ctx context.Context, dbMgr *db.Manager, platform, chatType, convID, query string, limit int) ([]db.StoredMessage, error) {
	return searchMessagesByBM25(ctx, dbMgr, platform, chatType, convID, query, limit)
}

func searchMessagesByBM25(ctx context.Context, dbMgr *db.Manager, platform, chatType, convID, query string, limit int) ([]db.StoredMessage, error) {
	if dbMgr == nil || convID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}

	terms := ExtractSearchTerms(query)
	var candidates []db.StoredMessage
	seenIDs := make(map[int64]struct{})

	if len(terms) > 0 {
		var lexMsgs []db.StoredMessage
		var err error
		if chatType == "" {
			lexMsgs, err = dbMgr.SearchMessages(ctx, platform, convID, terms, limit*5)
		} else {
			lexMsgs, err = dbMgr.SearchMessagesInChat(ctx, platform, chatType, convID, terms, limit*5)
		}
		if err != nil {
			return nil, err
		}
		for _, m := range lexMsgs {
			candidates = append(candidates, m)
			if m.ID > 0 {
				seenIDs[m.ID] = struct{}{}
			}
		}
	}

	qVec, modelIdentity := EmbedTextWithIdentity(ctx, query)
	if len(qVec) > 0 {
		var vecMsgs []db.StoredMessage
		var err error
		if chatType == "" {
			vecMsgs, err = dbMgr.RecentVectorMessages(ctx, platform, convID, modelIdentity, 500)
		} else {
			vecMsgs, err = dbMgr.RecentVectorMessagesInChat(ctx, platform, chatType, convID, modelIdentity, 500)
		}
		if err == nil {
			for _, m := range vecMsgs {
				if m.ID > 0 {
					if _, exists := seenIDs[m.ID]; exists {
						continue
					}
				}
				if m.EmbeddingModel == modelIdentity && CosineSimilarity(qVec, m.Embedding) >= 0.48 {
					candidates = append(candidates, m)
					if m.ID > 0 {
						seenIDs[m.ID] = struct{}{}
					}
				}
			}
		}
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	docs := make([]string, len(candidates))
	var vecScores []float32
	if len(qVec) > 0 {
		vecScores = make([]float32, len(candidates))
	}
	for i, m := range candidates {
		docs[i] = m.Text
		if len(qVec) > 0 && len(m.Embedding) > 0 && m.EmbeddingModel == modelIdentity {
			vecScores[i] = CosineSimilarity(qVec, m.Embedding)
		}
	}
	bm25Scores := ScoreBM25(query, docs)
	scores := FuseRRF(bm25Scores, vecScores, 60.0)

	type indexedMsg struct {
		idx   int
		score float32
	}
	ranked := make([]indexedMsg, len(candidates))
	for i := range candidates {
		ranked[i] = indexedMsg{idx: i, score: scores[i]}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return candidates[ranked[i].idx].ID > candidates[ranked[j].idx].ID
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	sort.Slice(ranked, func(i, j int) bool {
		if candidates[ranked[i].idx].ID != candidates[ranked[j].idx].ID {
			return candidates[ranked[i].idx].ID < candidates[ranked[j].idx].ID
		}
		return ranked[i].idx < ranked[j].idx
	})

	res := make([]db.StoredMessage, len(ranked))
	for i, item := range ranked {
		res[i] = candidates[item.idx]
	}
	return res, nil
}

// RecallProfile 获取用于对话背景注入的用户画像事实 (BM25 稀疏检索 + 时间衰减混合重排 + 核心基础事实保底)
func (m *MemoryEngine) RecallProfile(ctx context.Context, platform, userID, query string, maxFacts int) ([]*core.Fact, error) {
	dbMgr := m.getDB()
	if dbMgr == nil || platform == "" {
		return nil, nil
	}

	activeFacts, err := dbMgr.ListVisibleFacts(ctx, platform, userID, "", false)
	if err != nil || len(activeFacts) == 0 {
		return nil, err
	}
	return rankProfileFacts(ctx, query, activeFacts, maxFacts), nil
}

// RecallProfileForEvent 只注入当前会话可见范围内的长期事实。
func (m *MemoryEngine) RecallProfileForEvent(ctx context.Context, event *entity.SensoryEvent, query string, maxFacts int) ([]*core.Fact, error) {
	if event == nil || event.Platform == "" || event.SenderID == "" {
		return nil, nil
	}
	dbMgr := m.getDB()
	if dbMgr == nil {
		return nil, nil
	}
	facts, err := dbMgr.ListVisibleFacts(ctx, event.Platform, event.SenderID, event.ChannelID, event.IsGroup)
	if err != nil || len(facts) == 0 {
		return nil, err
	}
	return rankProfileFacts(ctx, query, facts, maxFacts), nil
}

func rankProfileFacts(ctx context.Context, query string, activeFacts []*core.Fact, maxFacts int) []*core.Fact {
	if maxFacts <= 0 {
		maxFacts = defaultMaxFacts
	}

	ranked := RankFactsByBM25(ctx, query, activeFacts, 0)
	if len(ranked) <= maxFacts {
		return ranked
	}

	// 当事实总数超出 maxFacts 时：优先选取 BM25 + 时间衰减最高的前 (maxFacts - coreKeep) 条，
	// 同时保底纳入最早建立的 coreKeep 条基础身份画像，避免长线老用户的核心设定被近期琐事挤出窗口
	coreKeep := maxFacts / 4
	if coreKeep > 10 {
		coreKeep = 10
	}
	dynamicKeep := maxFacts - coreKeep

	selected := make([]*core.Fact, 0, maxFacts)
	seen := make(map[int64]struct{}, maxFacts)
	for _, f := range ranked[:dynamicKeep] {
		selected = append(selected, f)
		seen[f.ID] = struct{}{}
	}
	for _, f := range activeFacts {
		if len(selected) >= maxFacts {
			break
		}
		if _, ok := seen[f.ID]; !ok {
			selected = append(selected, f)
			seen[f.ID] = struct{}{}
		}
	}
	for _, f := range ranked[dynamicKeep:] {
		if len(selected) >= maxFacts {
			break
		}
		if _, ok := seen[f.ID]; !ok {
			selected = append(selected, f)
			seen[f.ID] = struct{}{}
		}
	}
	return selected
}

func factScope(event *entity.SensoryEvent) (string, string) {
	if event != nil && event.IsGroup {
		return "group", strings.TrimSpace(event.ChannelID)
	}
	return "global", ""
}

func sameFactScope(fact *core.Fact, event *entity.SensoryEvent) bool {
	if fact == nil {
		return false
	}
	scopeType, scopeID := factScope(event)
	if fact.ScopeType == "" {
		return scopeType == "global"
	}
	factScopeID := fact.ScopeID
	if factScopeID == "" {
		factScopeID = fact.ConvID
	}
	return fact.ScopeType == scopeType && factScopeID == scopeID
}

func findFact(facts []*core.Fact, id int64) *core.Fact {
	for _, fact := range facts {
		if fact != nil && fact.ID == id {
			return fact
		}
	}
	return nil
}
