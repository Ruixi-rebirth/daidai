package qq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image/png"
	"log/slog"
	"mime"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"daidai/internal/core"
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
}

type dedupCache struct {
	mu   sync.Mutex
	seen map[string]int64
	max  int
}

func newDedupCache(max int) *dedupCache {
	if max <= 0 {
		max = 2000
	}
	return &dedupCache{
		seen: make(map[string]int64),
		max:  max,
	}
}

func (d *dedupCache) CheckAndSet(msgID string) bool {
	if msgID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, exists := d.seen[msgID]; exists {
		return true
	}
	d.rememberLocked(msgID)
	return false
}

func (d *dedupCache) Contains(msgID string) bool {
	if msgID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, exists := d.seen[msgID]
	return exists
}

func (d *dedupCache) Remember(msgID string) {
	if msgID == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rememberLocked(msgID)
}

func (d *dedupCache) rememberLocked(msgID string) {
	now := time.Now().Unix()

	if len(d.seen) >= d.max {
		oldestKey := ""
		var oldestTS int64
		for k, ts := range d.seen {
			if now-ts > 300 {
				delete(d.seen, k)
			} else if oldestKey == "" || ts < oldestTS {
				oldestKey = k
				oldestTS = ts
			}
		}
		if len(d.seen) >= d.max && oldestKey != "" {
			delete(d.seen, oldestKey)
		}
	}
	d.seen[msgID] = now
}

type msgCache struct {
	mu            sync.RWMutex
	items         map[string]db.MsgCacheEntry
	keys          []string
	max           int
	db            *db.Manager
	persistCh     chan msgCacheWrite
	persistDone   chan struct{}
	persistCtx    context.Context
	persistCancel context.CancelFunc
	persistMu     sync.Mutex
	persistClosed bool
}

type msgCacheWrite struct {
	entry db.MsgCacheEntry
	done  chan struct{}
}

type qqOutgoingPart struct {
	text  string
	image []byte
}

type passiveReplyBudget struct {
	used      int
	reserved  int
	limit     int
	expiresAt time.Time
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
const passiveReplyBudgetSweepInterval = time.Minute
const groupInterjectionCooldown = 5 * time.Minute
const groupInterjectionQuietPeriod = 4 * time.Second

func maxQQPassiveReplies(isGroup bool) int {
	if isGroup {
		return 5
	}
	return 4
}

func newMsgCache(max int, dbMgr *db.Manager) *msgCache {
	if max <= 0 {
		max = 500
	}
	c := &msgCache{
		items: make(map[string]db.MsgCacheEntry),
		keys:  make([]string, 0, max),
		max:   max,
		db:    dbMgr,
	}
	if dbMgr != nil {
		c.persistCh = make(chan msgCacheWrite, 4096)
		c.persistDone = make(chan struct{})
		c.persistCtx, c.persistCancel = context.WithCancel(context.Background())
		go c.persistLoop()
	}
	if dbMgr != nil {
		if loaded, err := dbMgr.LoadMsgCache("qq", max); err == nil && len(loaded) > 0 {
			for _, item := range loaded {
				if entity.IsSensitiveAuthCommand(item.Content) {
					if err := dbMgr.DeleteMsgCache("qq", item.MsgID); err != nil {
						slog.Warn("qq: 删除检测到的 OAuth 授权码缓存失败", "msgID", item.MsgID, "err", err)
					}
					continue
				}
				c.items[item.MsgID] = item
				c.keys = append(c.keys, item.MsgID)
			}
			slog.Info("qq: 成功从 SQLite 载入消息 ID 缓存", "count", len(c.items))
		}
	}
	return c
}

func (c *msgCache) persistLoop() {
	defer close(c.persistDone)
	for {
		select {
		case write, ok := <-c.persistCh:
			if !ok {
				return
			}
			ctx, cancel := context.WithTimeout(c.persistCtx, 5*time.Second)
			err := c.db.SaveMsgCache(ctx, "qq", write.entry.MsgID, write.entry.Content, write.entry.ChatType, write.entry.ConvID)
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("qq: 持久化引用消息缓存失败", "msgID", write.entry.MsgID, "err", err)
			}
			if write.done != nil {
				close(write.done)
			}
		case <-c.persistCtx.Done():
			// Close cancels only after closing the queue. Release synchronous
			// writers for entries that will not be persisted during shutdown.
			for write := range c.persistCh {
				if write.done != nil {
					close(write.done)
				}
			}
			return
		}
	}
}

func (c *msgCache) Close(ctx context.Context) error {
	if c.persistCh == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.persistMu.Lock()
	if !c.persistClosed {
		c.persistClosed = true
		close(c.persistCh)
	}
	c.persistMu.Unlock()
	select {
	case <-c.persistDone:
		return nil
	case <-ctx.Done():
		c.persistCancel()
		return ctx.Err()
	}
}

// Reopen restarts the persistence worker after a completed channel shutdown.
// Keep the cache object itself so callbacks installed by NewQQChannel continue
// writing to the same instance across Stop/Start cycles.
func (c *msgCache) Reopen() {
	if c == nil || c.db == nil {
		return
	}
	c.persistMu.Lock()
	defer c.persistMu.Unlock()
	if !c.persistClosed {
		return
	}
	c.persistCh = make(chan msgCacheWrite, 4096)
	c.persistDone = make(chan struct{})
	c.persistCtx, c.persistCancel = context.WithCancel(context.Background())
	c.persistClosed = false
	go c.persistLoop()
}

func (c *msgCache) Set(msgID, content, chatType, convID string) {
	c.set(msgID, content, chatType, convID, true)
}

func (c *msgCache) SetAsync(msgID, content, chatType, convID string) {
	c.set(msgID, content, chatType, convID, false)
}

func (c *msgCache) set(msgID, content, chatType, convID string, waitForPersistence bool) {
	if msgID == "" || content == "" || entity.IsSensitiveAuthCommand(content) ||
		(chatType != "private" && chatType != "group") || strings.TrimSpace(convID) == "" {
		return
	}
	content = core.TruncateRunes(content, core.MaxUserMessageRunes, "…[消息已截断]")
	c.mu.Lock()
	if _, exists := c.items[msgID]; !exists {
		if len(c.keys) >= c.max {
			oldest := c.keys[0]
			c.keys = c.keys[1:]
			delete(c.items, oldest)
		}
		c.keys = append(c.keys, msgID)
	}
	entry := db.MsgCacheEntry{MsgID: msgID, Content: content, ChatType: chatType, ConvID: convID}
	c.items[msgID] = entry
	c.mu.Unlock()

	if c.persistCh != nil {
		c.persistMu.Lock()
		if c.persistClosed {
			c.persistMu.Unlock()
			if !waitForPersistence {
				slog.Warn("qq: 忽略通道关闭后的引用消息缓存写入", "msgID", msgID)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := c.db.SaveMsgCache(ctx, "qq", msgID, content, chatType, convID)
			cancel()
			if err != nil {
				slog.Warn("qq: 通道关闭后持久化引用消息缓存失败", "msgID", msgID, "err", err)
			}
			return
		}
		write := msgCacheWrite{
			entry: entry,
		}
		if waitForPersistence {
			write.done = make(chan struct{})
			c.persistCh <- write
			c.persistMu.Unlock()
			<-write.done
			return
		} else {
			select {
			case c.persistCh <- write:
				c.persistMu.Unlock()
			default:
				c.persistMu.Unlock()
				slog.Warn("qq: 引用消息缓存持久化队列已满，保留内存缓存但未排入 SQLite", "msgID", msgID)
			}
		}
	}
}

func (c *msgCache) Get(msgID, chatType, convID string) string {
	if msgID == "" || (chatType != "private" && chatType != "group") || strings.TrimSpace(convID) == "" {
		return ""
	}
	c.mu.RLock()
	entry, exists := c.items[msgID]
	c.mu.RUnlock()
	if exists && entry.ChatType == chatType && entry.ConvID == convID {
		return entry.Content
	}
	if c.db == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	content, err := c.db.GetMsgCache(ctx, "qq", msgID, chatType, convID)
	if err != nil {
		slog.Warn("qq: 从持久化引用记录读取原消息失败", "msgID", msgID, "err", err)
	}
	if content != "" {
		return content
	}
	content, err = c.db.MessageContentByPlatformID(ctx, "qq", chatType, convID, msgID)
	if err != nil {
		slog.Warn("qq: 从历史消息读取引用正文失败", "msgID", msgID, "err", err)
	}
	return content
}

// QQChannel 官方 QQ 开放平台全双工通道 (直接实现 entity.Channel，零前缀、单层架构)
type QQChannel struct {
	cfg            Config
	client         *Client
	debounce       *kernel.DebounceManager
	immune         *entity.ImmuneSystem
	msgCache       *msgCache
	dedup          *dedupCache
	inbox          *media.Inbox
	accessRequests *db.Manager
	stopCancel     context.CancelFunc
	wsWG           sync.WaitGroup

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
}

// SetHistoryRecorder installs the callback used to persist each raw inbound
// event before debounce can merge it with neighboring messages.
func (q *QQChannel) SetHistoryRecorder(recorder func(context.Context, *entity.SensoryEvent) error) {
	if q != nil {
		q.historyRecorder = recorder
	}
}

func (q *QQChannel) shouldConsiderGroupInterjection(groupID string) bool {
	if q == nil || strings.TrimSpace(groupID) == "" {
		return false
	}
	now := time.Now()
	q.groupInterjectionsMu.Lock()
	defer q.groupInterjectionsMu.Unlock()
	if q.groupInterjections == nil {
		q.groupInterjections = make(map[string]time.Time)
	}
	if last, exists := q.groupInterjections[groupID]; exists && now.Sub(last) < groupInterjectionCooldown {
		return false
	}
	q.groupInterjections[groupID] = now
	if now.Sub(q.groupInterjectionSweepAt) >= 10*time.Minute || len(q.groupInterjections) > 4096 {
		for id, last := range q.groupInterjections {
			if now.Sub(last) >= groupInterjectionCooldown {
				delete(q.groupInterjections, id)
			}
		}
		q.groupInterjectionSweepAt = now
	}
	if len(q.groupInterjections) > 4096 {
		oldestID := ""
		var oldestAt time.Time
		for id, at := range q.groupInterjections {
			if oldestID == "" || at.Before(oldestAt) {
				oldestID, oldestAt = id, at
			}
		}
		delete(q.groupInterjections, oldestID)
	}
	return true
}

func (q *QQChannel) scheduleGroupInterjection(groupID string, event *entity.SensoryEvent) {
	if q == nil || event == nil || strings.TrimSpace(groupID) == "" || q.debounce == nil {
		return
	}
	q.pendingInterjectionsMu.Lock()
	defer q.pendingInterjectionsMu.Unlock()
	if q.pendingInterjections == nil {
		q.pendingInterjections = make(map[string]*pendingGroupInterjection)
	}
	if pending := q.pendingInterjections[groupID]; pending != nil {
		pending.timer.Stop()
		pending = &pendingGroupInterjection{event: event, scheduled: time.Now()}
		q.pendingInterjections[groupID] = pending
		pending.timer = time.AfterFunc(groupInterjectionQuietPeriod, func() {
			q.dispatchPendingGroupInterjection(groupID, pending)
		})
		return
	}
	pending := &pendingGroupInterjection{event: event, scheduled: time.Now()}
	pending.timer = time.AfterFunc(groupInterjectionQuietPeriod, func() {
		q.dispatchPendingGroupInterjection(groupID, pending)
	})
	q.pendingInterjections[groupID] = pending
	if len(q.pendingInterjections) > 4096 {
		oldestID := ""
		var oldestAt time.Time
		for id, entry := range q.pendingInterjections {
			if oldestID == "" || entry.scheduled.Before(oldestAt) {
				oldestID, oldestAt = id, entry.scheduled
			}
		}
		if oldest := q.pendingInterjections[oldestID]; oldest != nil {
			oldest.timer.Stop()
			delete(q.pendingInterjections, oldestID)
		}
	}
}

func (q *QQChannel) dispatchPendingGroupInterjection(groupID string, pending *pendingGroupInterjection) {
	q.pendingInterjectionsMu.Lock()
	if q.pendingInterjections[groupID] != pending {
		q.pendingInterjectionsMu.Unlock()
		return
	}
	delete(q.pendingInterjections, groupID)
	q.pendingInterjectionsMu.Unlock()

	if !q.shouldConsiderGroupInterjection(groupID) || q.debounce == nil {
		return
	}
	event := pending.event
	event.GroupInterjection = true
	event.GroupParticipants = q.recentGroupParticipants(groupID)
	if !q.debounce.Push(event) {
		slog.Warn("qq_channel: 群聊插话候选未能进入处理队列", "channelID", groupID, "msgID", event.RawMessageID)
	}
}

func (q *QQChannel) cancelPendingGroupInterjection(groupID string) {
	q.pendingInterjectionsMu.Lock()
	if pending := q.pendingInterjections[groupID]; pending != nil {
		pending.timer.Stop()
		delete(q.pendingInterjections, groupID)
	}
	q.pendingInterjectionsMu.Unlock()
}

func (q *QQChannel) loadKnownGroupBots(ctx context.Context) error {
	value, err := db.DefaultManager().GetRuntimeState(ctx, "qq", "known_group_bots")
	if err != nil {
		return err
	}
	known := make(map[string]map[string]string)
	if strings.TrimSpace(value) != "" {
		if err := json.Unmarshal([]byte(value), &known); err != nil {
			return fmt.Errorf("decode known group bots: %w", err)
		}
	}
	q.knownGroupBotsMu.Lock()
	q.knownGroupBots = known
	q.knownGroupBotsMu.Unlock()
	return nil
}

func (q *QQChannel) isKnownGroupBot(groupID, memberID string) bool {
	q.knownGroupBotsMu.RLock()
	defer q.knownGroupBotsMu.RUnlock()
	return q.knownGroupBots[groupID][memberID] != ""
}

func (q *QQChannel) setKnownGroupBot(ctx context.Context, groupID string, bot groupBotMention, marked bool) error {
	q.knownGroupBotsMu.Lock()
	defer q.knownGroupBotsMu.Unlock()
	value, err := db.DefaultManager().GetRuntimeState(ctx, "qq", "known_group_bots")
	if err != nil {
		return err
	}
	known := make(map[string]map[string]string)
	if strings.TrimSpace(value) != "" {
		if err := json.Unmarshal([]byte(value), &known); err != nil {
			return fmt.Errorf("decode known group bots: %w", err)
		}
	}
	if known[groupID] == nil {
		known[groupID] = make(map[string]string)
	}
	if marked {
		if _, exists := known[groupID][bot.ID]; !exists && len(known[groupID]) >= 100 {
			return fmt.Errorf("群机器人标记已达到每群 100 个的上限")
		}
		known[groupID][bot.ID] = bot.Name
	} else {
		delete(known[groupID], bot.ID)
		if len(known[groupID]) == 0 {
			delete(known, groupID)
		}
	}
	updated, err := json.Marshal(known)
	if err != nil {
		return err
	}
	if err := db.DefaultManager().SetRuntimeState(ctx, "qq", "known_group_bots", string(updated)); err != nil {
		return err
	}
	q.knownGroupBots = known
	return nil
}

func groupBotRegistryRequest(content string) (marked, matched bool) {
	if !strings.Contains(content, "机器人") {
		return false, false
	}
	if strings.Contains(content, "取消") || strings.Contains(content, "解除") || strings.Contains(content, "不是") {
		return false, true
	}
	return true, strings.Contains(content, "标记") || strings.Contains(content, "记住") || strings.Contains(content, "是机器人")
}

func qqMentionedGroupMembers(eventData map[string]any) []groupBotMention {
	mentions, ok := eventData["mentions"].([]any)
	if !ok {
		return nil
	}
	seen := make(map[string]struct{}, len(mentions))
	var members []groupBotMention
	for _, rawMention := range mentions {
		mention, ok := rawMention.(map[string]any)
		if !ok {
			continue
		}
		if isYou, _ := mention["is_you"].(bool); isYou {
			continue
		}
		memberID, _ := mention["member_openid"].(string)
		if memberID == "" {
			memberID, _ = mention["id"].(string)
		}
		memberID = strings.TrimSpace(memberID)
		if memberID == "" {
			continue
		}
		if _, exists := seen[memberID]; exists {
			continue
		}
		seen[memberID] = struct{}{}
		name, _ := mention["username"].(string)
		members = append(members, groupBotMention{ID: memberID, Name: strings.TrimSpace(name)})
	}
	return members
}

func (q *QQChannel) handleGroupBotRegistryRequest(ctx context.Context, groupID, messageID, senderID string, role entity.UserRole, content string, eventData map[string]any) {
	marked, _ := groupBotRegistryRequest(content)
	if role != entity.RolePartner {
		q.replyGroupMessage(ctx, groupID, messageID, "只有核心搭档可以管理群聊中的机器人标记。")
		return
	}
	members := qqMentionedGroupMembers(eventData)
	if len(members) == 0 {
		q.replyGroupMessage(ctx, groupID, messageID, "我没有识别到被标记的群成员。请 @ 呆呆，并同时 @ 要标记或取消标记的机器人。")
		return
	}
	if q.historyRecorder != nil {
		recordCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := q.historyRecorder(recordCtx, &entity.SensoryEvent{
			Platform: "qq", ChannelID: groupID, IsGroup: true, SenderID: senderID,
			Content: content, RawMessageID: messageID, OccurredAt: time.Now(), Role: role,
		})
		cancel()
		if err != nil {
			slog.Warn("qq_channel: 保存群机器人标记操作失败", "channelID", groupID, "msgID", messageID, "err", err)
		}
	}
	var updated []string
	for _, member := range members {
		if err := q.setKnownGroupBot(ctx, groupID, member, marked); err != nil {
			slog.Warn("qq_channel: 保存群机器人标记失败", "channelID", groupID, "memberID", member.ID, "err", err)
			continue
		}
		name := member.Name
		if name == "" {
			name = "该成员"
		}
		updated = append(updated, name)
	}
	if len(updated) == 0 {
		q.replyGroupMessage(ctx, groupID, messageID, "机器人标记未能保存，请稍后重试。")
		return
	}
	action := "已标记为机器人，后续群消息不会触发呆呆回复。"
	if !marked {
		action = "已取消机器人标记，后续群消息会按普通成员消息处理。"
	}
	q.replyGroupMessage(ctx, groupID, messageID, strings.Join(updated, "、")+action)
}

func (q *QQChannel) recentGroupParticipants(groupID string) []core.GroupParticipant {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	recent, err := db.DefaultManager().RecentMessages(ctx, "qq", "group", groupID, 30)
	if err != nil {
		slog.Warn("qq_channel: 读取插话点名候选失败", "channelID", groupID, "err", err)
		return nil
	}
	seen := make(map[string]struct{}, len(recent))
	participants := make([]core.GroupParticipant, 0, len(recent))
	for _, message := range recent {
		if message.IsBot || message.UserID == "" || strings.TrimSpace(message.Name) == "" {
			continue
		}
		if _, exists := seen[message.UserID]; exists {
			continue
		}
		seen[message.UserID] = struct{}{}
		participants = append(participants, core.GroupParticipant{ID: message.UserID, Name: message.Name})
	}
	return participants
}

func (q *QQChannel) replyGroupMessage(ctx context.Context, groupID, messageID, text string) {
	replyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := q.Transmit(replyCtx, &entity.Utterance{
		Platform: "qq", IsGroup: true, ChannelID: groupID, QuoteMsgID: messageID, Text: text,
	}); err != nil {
		slog.Warn("qq_channel: 群机器人标记操作的确认消息发送失败", "channelID", groupID, "err", err)
	}
}

func (q *QQChannel) replyPrivateMessage(userID, messageID, text string) {
	replyCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := q.Transmit(replyCtx, &entity.Utterance{
		Platform: "qq", ChannelID: userID, QuoteMsgID: messageID, Text: text,
	}); err != nil {
		slog.Warn("qq_channel: 访客私聊权限提示发送失败", "channelID", userID, "err", err)
	}
}

func (q *QQChannel) notifyOwnerOfPrivateAccessRequest(userID, displayName, message string) {
	ownerID := strings.TrimSpace(q.cfg.OwnerOpenID)
	if ownerID == "" || ownerID == strings.TrimSpace(userID) {
		return
	}
	name := strings.TrimSpace(displayName)
	if name == "" {
		name = "未提供昵称"
	}
	message = core.TruncateRunes(strings.TrimSpace(message), 200, "…")
	if message == "" {
		message = "（空消息或附件消息）"
	}
	body := fmt.Sprintf("有新的 QQ 私聊权限申请\n昵称：%s\nOpenID：%s\n最近消息：%s\n\n如需授权，可直接告诉我“将 OpenID %s 加为协作者”。", core.TruncateRunes(name, 80, "…"), userID, message, userID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := q.Transmit(ctx, &entity.Utterance{Platform: "qq", ChannelID: ownerID, Text: body, Proactive: true})
	if err == nil {
		cancel()
		return
	}
	slog.Warn("qq_channel: 新私聊权限申请主动通知失败", "channelID", ownerID, "userID", userID, "err", err)
	if q.accessRequests != nil {
		dedupeKey := fmt.Sprintf("private-access:%s:%d", userID, time.Now().UnixNano())
		if _, queueErr := q.accessRequests.EnqueuePendingNotification(ctx, "qq", ownerID, dedupeKey, "private_access_request", body); queueErr != nil {
			slog.Warn("qq_channel: 新私聊权限申请通知入队失败", "channelID", ownerID, "userID", userID, "err", queueErr)
		}
	}
	cancel()
}

func (q *QQChannel) reservePassiveReplyBudget(msgID string, count, limit int, window time.Duration) (*passiveReplyBudget, int, bool, error) {
	if msgID == "" {
		return nil, count, false, nil
	}
	if limit <= 0 || window <= 0 {
		return nil, 0, false, fmt.Errorf("invalid QQ passive reply budget: limit=%d window=%s", limit, window)
	}
	now := time.Now()
	q.replyBudgetsMu.Lock()
	if q.replyBudgets == nil {
		q.replyBudgets = make(map[string]*passiveReplyBudget)
	}
	if !now.Before(q.replyBudgetSweepAt) {
		for key, budget := range q.replyBudgets {
			if !now.Before(budget.expiresAt) && budget.reserved == 0 {
				delete(q.replyBudgets, key)
			}
		}
		q.replyBudgetSweepAt = now.Add(passiveReplyBudgetSweepInterval)
	}
	budget := q.replyBudgets[msgID]
	if budget == nil {
		budget = &passiveReplyBudget{limit: limit, expiresAt: now.Add(window)}
		q.replyBudgets[msgID] = budget
	} else if budget.limit != limit {
		q.replyBudgetsMu.Unlock()
		return nil, 0, false, fmt.Errorf("QQ message reply limit changed while processing one inbound message")
	}
	if !now.Before(budget.expiresAt) {
		if budget.reserved == 0 {
			budget.used = 0
			budget.expiresAt = now.Add(window)
		}
	}
	if budget.used+budget.reserved+count > budget.limit {
		available := budget.limit - budget.used - budget.reserved
		if available <= 0 {
			used := budget.used + budget.reserved
			q.replyBudgetsMu.Unlock()
			return nil, 0, false, fmt.Errorf("QQ 入站消息的被动回复额度已使用或预留 %d/%d 条；本次未发送，请用新消息继续请求", used, budget.limit)
		}
		budget.reserved += available
		q.replyBudgetsMu.Unlock()
		return budget, available, true, nil
	}
	budget.reserved += count
	q.replyBudgetsMu.Unlock()
	return budget, count, false, nil
}

func (q *QQChannel) commitPassiveReplyAttempt(budget *passiveReplyBudget) {
	if budget == nil {
		return
	}
	q.replyBudgetsMu.Lock()
	if budget.reserved > 0 {
		budget.reserved--
		budget.used++
	}
	q.replyBudgetsMu.Unlock()
}

func (q *QQChannel) reserveAdditionalPassiveReply(budget *passiveReplyBudget) bool {
	if budget == nil {
		return true
	}
	q.replyBudgetsMu.Lock()
	defer q.replyBudgetsMu.Unlock()
	if budget.used+budget.reserved >= budget.limit {
		return false
	}
	budget.reserved++
	return true
}

func (q *QQChannel) releasePassiveReplyReservations(budget *passiveReplyBudget, count int) {
	if budget == nil || count <= 0 {
		return
	}
	q.replyBudgetsMu.Lock()
	budget.reserved -= count
	if budget.reserved < 0 {
		budget.reserved = 0
	}
	q.replyBudgetsMu.Unlock()
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
func NewQQChannel(cfg Config, immune *entity.ImmuneSystem) *QQChannel {
	cache := newMsgCache(1000, db.DefaultManager())
	cli := NewClient(cfg.AppID, cfg.Secret)
	channel := &QQChannel{
		cfg:      cfg,
		client:   cli,
		immune:   immune,
		msgCache: cache,
		dedup:    newDedupCache(2000),
	}
	cli.SetOnMessageSent(func(msgID, content, targetType, targetID string) {
		// If QQ reflects an outbound group message through GROUP_MESSAGE_CREATE,
		// its accepted send ID lets the inbound path drop that exact echo.
		if channel.dedup != nil {
			channel.dedup.Remember(msgID)
		}
		chatType := "private"
		if targetType == "groups" {
			chatType = "group"
		}
		cache.SetAsync(msgID, content, chatType, targetID)
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

func (q *QQChannel) SetAccessRequestStore(database *db.Manager) {
	q.accessRequests = database
}

func (q *QQChannel) Start(ctx context.Context, onSensory func(event *entity.SensoryEvent)) error {
	q.wsReady.Store(false)
	if q.msgCache != nil {
		q.msgCache.Reopen()
	}
	loadBotsCtx, loadBotsCancel := context.WithTimeout(ctx, 3*time.Second)
	if err := q.loadKnownGroupBots(loadBotsCtx); err != nil {
		slog.Warn("qq_channel: 已标记的群机器人清单读取失败", "err", err)
	}
	loadBotsCancel()
	chCtx, cancel := context.WithCancel(ctx)
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
		if err := q.client.StartTokenRefresher(chCtx); err != nil {
			return err
		}
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

// dispatchEvent 解析 WebSocket 网关下发的消息事件并推入防抖队列 (零前缀纯净 ID)
func (q *QQChannel) dispatchEvent(payload map[string]any) bool {
	d, ok := payload["d"].(map[string]any)
	if !ok {
		return true
	}
	eventType, _ := payload["t"].(string)

	var (
		convID         string
		userID         string
		senderName     string
		content        string
		msgID          string
		isGroup        bool
		fullGroupEvent bool
		botMentioned   bool
		images         []*core.Attachment
		omittedImages  int
		files          []*core.Attachment
		omittedFiles   int
	)

	msgID, _ = d["id"].(string)
	rawContent, _ := d["content"].(string)
	content = cleanQQContent(rawContent)

	// 只有明确标记为图片的附件进入视觉模型；类型不明的附件按普通文件处理，须用户明确要求后才读取。
	if attList, ok := d["attachments"].([]any); ok {
		for _, item := range attList {
			if attMap, ok := item.(map[string]any); ok {
				fileURL, _ := attMap["url"].(string)
				contentType, _ := attMap["content_type"].(string)
				fileName, _ := attMap["filename"].(string)
				if fileName == "" {
					fileName, _ = attMap["name"].(string)
				}
				fileName = core.TruncateRunes(strings.TrimSpace(fileName), 160, "…")
				contentType = strings.ToLower(strings.TrimSpace(core.TruncateRunes(contentType, 128, "")))
				baseContentType := strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])
				if fileName != "" && (baseContentType == "" || baseContentType == "application/octet-stream" || baseContentType == "binary/octet-stream") {
					guessedType := strings.ToLower(strings.TrimSpace(strings.SplitN(mime.TypeByExtension(filepath.Ext(fileName)), ";", 2)[0]))
					if guessedType != "" {
						baseContentType = guessedType
					}
				}
				contentType = core.TruncateRunes(baseContentType, 128, "…")
				if fileURL == "" {
					continue
				}
				if len(fileURL) > core.MaxAttachmentURLBytes {
					if strings.HasPrefix(contentType, "image/") {
						omittedImages++
					} else {
						omittedFiles++
					}
					continue
				}
				if strings.HasPrefix(contentType, "image/") {
					if len(images) >= core.MaxVisionImagesPerRequest {
						omittedImages++
						continue
					}
					images = append(images, &core.Attachment{Kind: core.AttachmentImage, URL: fileURL, MimeType: contentType, Name: fileName, SourceMessageID: msgID})
					continue
				}
				if len(files) >= core.MaxIncomingFilesPerRequest {
					omittedFiles++
					continue
				}
				files = append(files, &core.Attachment{Kind: core.AttachmentFile, URL: fileURL, MimeType: contentType, Name: fileName, SourceMessageID: msgID})
			}
		}
	}

	switch eventType {
	case "GROUP_AT_MESSAGE_CREATE": // 群聊中被 @ 的消息
		isGroup = true
		botMentioned = true
		convID, _ = d["group_openid"].(string)
		if author, ok := d["author"].(map[string]any); ok {
			userID, _ = author["member_openid"].(string)
			senderName, _ = author["username"].(string)
		}
	case "GROUP_MESSAGE_CREATE": // 群聊全量消息；仅唤醒 @ 机器人的消息
		isGroup = true
		fullGroupEvent = true
		botMentioned = qqMessageMentionsBot(d)
		convID, _ = d["group_openid"].(string)
		if author, ok := d["author"].(map[string]any); ok {
			userID, _ = author["member_openid"].(string)
			senderName, _ = author["username"].(string)
		}

	case "C2C_MESSAGE_CREATE": // 好友私聊单聊消息
		isGroup = false
		if author, ok := d["author"].(map[string]any); ok {
			userID, _ = author["user_openid"].(string)
			senderName, _ = author["username"].(string)
		}
		convID = userID

	default:
		return true
	}
	if qqMessageAuthorIsBot(d) || (isGroup && q.isKnownGroupBot(convID, userID)) {
		if isGroup {
			q.cancelPendingGroupInterjection(convID)
		}
		slog.Debug("qq: 忽略机器人消息，避免机器人之间互相触发", "eventType", eventType, "channelID", convID, "msgID", msgID)
		return true
	}
	if msgID != "" && q.dedup != nil && q.dedup.Contains(msgID) {
		slog.Debug("qq: 拦截到重复投递的事件消息", "msgID", msgID)
		return true
	}
	if msgID == "" {
		slog.Warn("qq: 忽略缺少原消息 ID 的事件，无法使用被动回复凭证", "eventType", eventType, "channelID", convID)
		return true
	}

	var replyCtx *entity.SensoryEvent
	if ref, ok := d["message_reference"].(map[string]any); ok {
		var refContent string
		if c, ok := ref["content"].(string); ok && c != "" {
			refContent = c
		} else if msgObj, ok := ref["message"].(map[string]any); ok {
			refContent, _ = msgObj["content"].(string)
		}

		refMsgID, _ := ref["message_id"].(string)
		if refContent == "" && refMsgID != "" && q.msgCache != nil {
			chatType := "private"
			if isGroup {
				chatType = "group"
			}
			refContent = q.msgCache.Get(refMsgID, chatType, convID)
		}
		refContent = entity.SanitizeQuotedMessageContent(refContent)

		slog.Info("qq: 处理引用消息", "msgID", msgID, "refMsgID", refMsgID, "hitCache", refContent != "")

		if refContent != "" {
			replyCtx = &entity.SensoryEvent{
				Content:      strings.TrimSpace(refContent),
				RawMessageID: refMsgID,
			}
		}
	}

	// 适配 QQ 官方 v2 协议原生引用消息 (message_type: 103)
	if replyCtx == nil {
		if elements, ok := d["msg_elements"].([]any); ok {
			for _, el := range elements {
				if elMap, ok := el.(map[string]any); ok {
					var elType int
					switch vt := elMap["message_type"].(type) {
					case float64:
						elType = int(vt)
					case int:
						elType = vt
					}
					if elType == 103 {
						if c, ok := elMap["content"].(string); ok && strings.TrimSpace(c) != "" {
							refMsgID, _ := elMap["id"].(string)
							c = entity.SanitizeQuotedMessageContent(c)
							replyCtx = &entity.SensoryEvent{
								Content:      c,
								RawMessageID: refMsgID,
							}
							slog.Info("qq: 从 v2 msg_elements 中成功解析出引用消息", "msgID", msgID)
							break
						}
					}
				}
			}
		}
	}

	if convID != "" && userID == "" {
		slog.Warn("qq: 忽略缺少发送者 ID 的消息事件", "eventType", eventType, "msgID", msgID, "channelID", convID)
		return true
	}

	var pendingFiles []*core.Attachment
	var pendingImages []*core.Attachment
	var omittedPendingFiles int
	var omittedPendingImages int
	var attachmentScope string
	if q.inbox != nil && convID != "" && userID != "" {
		attachmentScope = media.AttachmentScope("qq", isGroup, convID, userID)
		pendingFiles, omittedPendingFiles = q.inbox.PendingKind(attachmentScope, core.AttachmentFile)
		pendingImages, omittedPendingImages = q.inbox.PendingKind(attachmentScope, core.AttachmentImage)
	}
	if convID != "" && userID != "" && (content != "" || len(images) > 0 || len(files) > 0 || len(pendingFiles) > 0 || len(pendingImages) > 0 || omittedPendingFiles > 0 || omittedPendingImages > 0) {
		role := entity.RoleGuest
		if q.immune != nil {
			role = q.immune.ResolveRole(userID)
		}
		if !isGroup && role == entity.RoleGuest {
			if q.dedup != nil {
				q.dedup.Remember(msgID)
			}
			shouldNotifyOwner := false
			accessMessage := strings.TrimSpace(content)
			if entity.IsSensitiveAuthCommand(accessMessage) {
				accessMessage = "[敏感认证指令内容已隐藏]"
			} else if accessMessage == "" {
				accessMessage = attachmentHistoryNote(len(images), omittedImages, len(files), omittedFiles)
			}
			accessMessage = core.TruncateRunes(accessMessage, 500, "…")
			if q.accessRequests != nil {
				accessCtx, accessCancel := context.WithTimeout(context.Background(), 2*time.Second)
				var recordErr error
				shouldNotifyOwner, recordErr = q.accessRequests.RecordPrivateAccessRequest(accessCtx, "qq", userID, senderName, accessMessage)
				if recordErr != nil {
					slog.Warn("qq: 保存访客私聊申请失败", "userID", userID, "err", recordErr)
				}
				accessCancel()
			}
			slog.Info("qq: 拒绝未授权访客私聊", "userID", userID, "senderName", senderName, "messageID", msgID)
			q.replyPrivateMessage(userID, msgID, "暂未开通私聊权限")
			if shouldNotifyOwner {
				q.notifyOwnerOfPrivateAccessRequest(userID, senderName, accessMessage)
			}
			return true
		}
		botAddressed := botMentioned || (fullGroupEvent && !strings.HasPrefix(strings.TrimSpace(content), "/") && strings.Contains(content, "呆呆"))
		trimmedContent := strings.TrimSpace(content)
		if trimmedContent != "" && !strings.HasPrefix(trimmedContent, "/") && trimmedContent != "继续" && q.msgCache != nil && !entity.IsSensitiveAuthCommand(content) {
			chatType := "private"
			if isGroup {
				chatType = "group"
			}
			q.msgCache.SetAsync(msgID, content, chatType, convID)
		}
		if q.inbox != nil && (len(files) > 0 || len(images) > 0) {
			q.inbox.RememberPending(attachmentScope, append(append([]*core.Attachment(nil), files...), images...))
		}
		if isGroup && botAddressed {
			if _, matched := groupBotRegistryRequest(content); matched {
				q.cancelPendingGroupInterjection(convID)
				manageCtx, manageCancel := context.WithTimeout(context.Background(), 10*time.Second)
				q.handleGroupBotRegistryRequest(manageCtx, convID, msgID, userID, role, content, d)
				manageCancel()
				if q.dedup != nil {
					q.dedup.Remember(msgID)
				}
				return true
			}
		}
		if fullGroupEvent && !botAddressed {
			historyAttachmentNote := attachmentHistoryNote(len(images), omittedImages, len(files), omittedFiles)
			if content == "" && historyAttachmentNote == "" {
				if q.dedup != nil {
					q.dedup.Remember(msgID)
				}
				return true
			}
			// Ordinary group messages are saved to formal group history. Attachments
			// stay pending and slash commands never execute without an @ mention.
			canInterject := len(images) == 0 && omittedImages == 0 && len(files) == 0 && omittedFiles == 0 && len(pendingImages) == 0 && len(pendingFiles) == 0 && omittedPendingImages == 0 && omittedPendingFiles == 0 && !strings.HasPrefix(content, "/") && content != "继续"
			observedEvent := &entity.SensoryEvent{
				Platform:              "qq",
				ChannelID:             convID,
				IsGroup:               true,
				SenderID:              userID,
				SenderName:            strings.TrimSpace(senderName),
				Role:                  role,
				Content:               strings.TrimSpace(content),
				HistoryAttachmentNote: historyAttachmentNote,
				ReplyContext:          replyCtx,
				RawMessageID:          msgID,
				OccurredAt:            time.Now(),
				GroupMessageObserved:  true,
			}
			accepted := q.IngestRaw(observedEvent)
			if accepted && q.dedup != nil {
				q.dedup.Remember(msgID)
			}
			if accepted && canInterject {
				candidate := &entity.SensoryEvent{
					Platform:             observedEvent.Platform,
					ChannelID:            observedEvent.ChannelID,
					IsGroup:              observedEvent.IsGroup,
					SenderID:             observedEvent.SenderID,
					SenderName:           observedEvent.SenderName,
					Role:                 observedEvent.Role,
					Content:              observedEvent.Content,
					ReplyContext:         observedEvent.ReplyContext,
					RawMessageID:         observedEvent.RawMessageID,
					HistoryRecordID:      observedEvent.HistoryRecordID,
					OccurredAt:           observedEvent.OccurredAt,
					GroupMessageObserved: true,
				}
				q.scheduleGroupInterjection(convID, candidate)
			} else {
				q.cancelPendingGroupInterjection(convID)
			}
			return accepted
		}
		if fullGroupEvent && botAddressed {
			q.cancelPendingGroupInterjection(convID)
		}
		accepted := q.IngestRaw(&entity.SensoryEvent{
			Platform:              "qq",
			ChannelID:             convID,
			IsGroup:               isGroup,
			SenderID:              userID,
			SenderName:            strings.TrimSpace(senderName),
			Role:                  role,
			Content:               strings.TrimSpace(content),
			HistoryAttachmentNote: attachmentHistoryNote(len(images), omittedImages, len(files), omittedFiles),
			Images:                images,
			OmittedImages:         omittedImages,
			PendingImages:         pendingImages,
			OmittedPendingImages:  omittedPendingImages,
			Files:                 files,
			PendingFiles:          pendingFiles,
			OmittedFiles:          omittedFiles,
			OmittedPendingFiles:   omittedPendingFiles,
			ReplyContext:          replyCtx,
			RawMessageID:          msgID,
			OccurredAt:            time.Now(),
		})
		if !accepted {
			return false
		}
		if q.dedup != nil {
			q.dedup.Remember(msgID)
		}
	}
	return true
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

func attachmentHistoryNote(images, omittedImages int, files, omittedFiles int) string {
	var kinds []string
	if count := images + omittedImages; count > 0 {
		kinds = append(kinds, fmt.Sprintf("图片 %d 张", count))
	}
	if count := files + omittedFiles; count > 0 {
		kinds = append(kinds, fmt.Sprintf("文件 %d 个", count))
	}
	if len(kinds) == 0 {
		return ""
	}
	return "[附件：" + strings.Join(kinds, "，") + "；未保存附件本体]"
}

// Transmit 物理发声：投递全息表达体到 QQ 客户端 (支持大文本自动分片与图文混合发送)
func (q *QQChannel) Transmit(ctx context.Context, u *entity.Utterance) error {
	if u == nil {
		return nil
	}
	if q.client == nil {
		return fmt.Errorf("QQ client is unavailable")
	}

	images := u.Images
	if len(images) == 0 && len(u.CardData) > 0 {
		images = [][]byte{u.CardData}
	} else if len(images) > 0 {
		validImages := make([][]byte, 0, len(images))
		for _, image := range images {
			if len(image) > 0 {
				validImages = append(validImages, image)
			}
		}
		images = validImages
	}
	content := u.Text
	if content == "" && len(images) == 0 {
		return nil
	}

	if u.ChannelID == "" {
		return fmt.Errorf("empty channelID in utterance")
	}
	if u.Proactive && u.QuoteMsgID != "" {
		return fmt.Errorf("QQ 主动消息不能同时携带被动回复引用 ID")
	}
	if u.GroupInterjection && (!u.IsGroup || u.Proactive || u.QuoteMsgID == "") {
		return fmt.Errorf("QQ群聊插话必须关联触发它的群消息，并通过被动回复发送")
	}
	if u.QuoteMsgID == "" && !u.Proactive {
		return fmt.Errorf("QQ 消息缺少入站引用 ID，必须显式标记为主动发送")
	}
	partLimit := maxQQOutgoingParts
	replyWindow := time.Duration(0)
	if !u.Proactive {
		partLimit = maxQQPassiveReplies(u.IsGroup)
		replyWindow = time.Hour
		if u.IsGroup {
			replyWindow = 5 * time.Minute
		}
	}
	if u.InlineImages {
		return q.transmitInlineImages(ctx, u, images, content, partLimit, replyWindow)
	}

	chunks := core.SplitMarkdownChunks(content, 1200)
	if len(chunks) == 0 {
		chunks = []string{""}
	}
	if len(chunks) > partLimit {
		return fmt.Errorf("%w: QQ 单次发送最多 %d 条，当前文字需要 %d 条，请缩短回复", entity.ErrMessageTooLong, partLimit, len(chunks))
	}
	parts := make([]qqOutgoingPart, 0, len(chunks)+len(images)-1)
	for i, chunk := range chunks {
		part := qqOutgoingPart{text: chunk}
		if i == 0 && len(images) > 0 {
			part.image = images[0]
		}
		parts = append(parts, part)
	}
	if len(images) > 1 {
		for _, image := range images[1:] {
			parts = append(parts, qqOutgoingPart{image: image})
		}
	}
	if len(parts) > partLimit {
		return fmt.Errorf("%w: 图文内容需要发送 %d 条，超过 QQ 单次最多 %d 条的限制", entity.ErrMessageTooLong, len(parts), partLimit)
	}
	budget, reservedCount, limitedByBudget, err := q.reservePassiveReplyBudget(u.QuoteMsgID, len(parts), partLimit, replyWindow)
	if err != nil {
		return err
	}
	defer func() { q.releasePassiveReplyReservations(budget, reservedCount) }()

	sendPart := func(part qqOutgoingPart) error {
		if u.IsGroup {
			if len(part.image) > 0 {
				return q.client.SendGroupMediaMessage(ctx, u.ChannelID, part.text, part.image, u.QuoteMsgID)
			}
			return q.client.SendGroupMessage(ctx, u.ChannelID, part.text, u.QuoteMsgID)
		}
		if len(part.image) > 0 {
			return q.client.SendC2CMediaMessage(ctx, u.ChannelID, part.text, part.image, u.QuoteMsgID)
		}
		return q.client.SendC2CMessage(ctx, u.ChannelID, part.text, u.QuoteMsgID)
	}
	sentParts := 0
	sendTrackedPart := func(part qqOutgoingPart) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if budget != nil {
			// Reserve before sending: a network error may arrive after QQ accepted it.
			q.commitPassiveReplyAttempt(budget)
			reservedCount--
		}
		if sentParts == 0 && u.IsGroup && len(u.MentionUserIDs) > 0 && len(part.image) == 0 {
			if at := qqMentionMarkup(u.MentionUserIDs); at != "" {
				part.text = at + part.text
				if len([]rune(part.text)) > 1200 {
					return fmt.Errorf("群聊回复的 @ 提及标记超过 QQ 单条消息长度限制")
				}
				return q.client.SendGroupMessage(ctx, u.ChannelID, part.text, u.QuoteMsgID)
			}
		}
		return sendPart(part)
	}

	deliveredText := func() string {
		var delivered strings.Builder
		for _, part := range parts[:sentParts] {
			if part.text != "" {
				if delivered.Len() > 0 {
					delivered.WriteString("\n\n")
				}
				delivered.WriteString(part.text)
			}
			if len(part.image) > 0 {
				if delivered.Len() > 0 {
					delivered.WriteString("\n")
				}
				delivered.WriteString("[图片已发送]")
			}
		}
		return delivered.String()
	}
	undeliveredText := func(from int) string {
		var remaining strings.Builder
		for _, part := range parts[from:] {
			if part.text != "" {
				if remaining.Len() > 0 {
					remaining.WriteString("\n\n")
				}
				remaining.WriteString(part.text)
			}
			if len(part.image) > 0 {
				if remaining.Len() > 0 {
					remaining.WriteString("\n\n")
				}
				remaining.WriteString("[图片未送达；文字续发无法恢复图片，请告诉我重新发送。]")
			}
		}
		return remaining.String()
	}
	partsToSend := len(parts)
	if limitedByBudget {
		partsToSend = reservedCount
		if partsToSend > 0 {
			parts[partsToSend-1].text = strings.TrimSpace(parts[partsToSend-1].text + "\n[剩余内容未发送；回复“继续”可接着看。]")
		}
	}
	for i, part := range parts[:partsToSend] {
		if i > 0 {
			select {
			case <-ctx.Done():
				if sentParts > 0 {
					return &entity.PartialDeliveryError{Err: ctx.Err(), DeliveredText: deliveredText(), UndeliveredText: undeliveredText(sentParts)}
				}
				return ctx.Err()
			case <-time.After(150 * time.Millisecond):
			}
		}
		err := sendTrackedPart(part)
		if err != nil {
			slog.Error("qq_channel: 消息分片发送失败", "channelID", u.ChannelID, "isGroup", u.IsGroup, "quoteID", u.QuoteMsgID, "sentParts", sentParts, "err", err)
			if sentParts > 0 {
				partialText := deliveredText()
				if ctx.Err() != nil {
					return &entity.PartialDeliveryError{
						Err:             ctx.Err(),
						DeliveredText:   partialText,
						UndeliveredText: undeliveredText(sentParts),
					}
				}
				q.releasePassiveReplyReservations(budget, reservedCount)
				reservedCount = 0
				canSendNotice := sentParts+1 < partLimit
				if budget != nil {
					canSendNotice = q.reserveAdditionalPassiveReply(budget)
					if canSendNotice {
						reservedCount++
					}
				}
				if canSendNotice {
					notice := "部分内容发送失败，前面的内容已送达。"
					if len(part.image) > 0 {
						notice = "部分图片发送失败，前面的内容已送达。"
					}
					noticeErr := sendTrackedPart(qqOutgoingPart{text: notice})
					if noticeErr == nil {
						partialText = strings.TrimSpace(partialText + "\n\n" + notice)
					} else {
						return &entity.PartialDeliveryError{
							Err:             fmt.Errorf("send part: %v; send partial-delivery notice: %w", err, noticeErr),
							DeliveredText:   partialText,
							UndeliveredText: undeliveredText(sentParts),
						}
					}
				}
				return &entity.PartialDeliveryError{Err: err, DeliveredText: partialText, UndeliveredText: undeliveredText(sentParts)}
			}
			return err
		}
		sentParts++
	}
	if partsToSend < len(parts) {
		return &entity.PartialDeliveryError{
			Err:             fmt.Errorf("QQ 被动回复额度仅剩 %d 条，已发送前 %d 条；剩余内容等待续发", partsToSend, sentParts),
			DeliveredText:   deliveredText(),
			UndeliveredText: undeliveredText(sentParts),
		}
	}

	slog.Info("qq_channel: 表达成功送达客户端", "channelID", u.ChannelID, "isGroup", u.IsGroup, "hasMedia", u.HasMedia())
	return nil
}

func (q *QQChannel) transmitInlineImages(ctx context.Context, u *entity.Utterance, images [][]byte, content string, partLimit int, replyWindow time.Duration) error {
	if len(images) == 0 {
		return fmt.Errorf("inline image message has no images")
	}
	if len(images) > 4 {
		return fmt.Errorf("inline image message exceeds the four-page limit")
	}
	var markdown strings.Builder
	if u.IsGroup {
		if at := qqMentionMarkup(u.MentionUserIDs); at != "" {
			markdown.WriteString(at)
			markdown.WriteString("\n\n")
		}
	}
	imageIDs := make([]string, 0, len(images))
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, imageID := range imageIDs {
			if err := q.client.DeleteMarkdownImage(cleanupCtx, imageID); err != nil {
				slog.Warn("qq_channel: failed to release temporary Markdown image material", "imageID", imageID, "err", err)
			}
		}
	}
	for i, image := range images {
		imageID, imageURL, err := q.client.UploadMarkdownImage(ctx, image)
		if err != nil {
			cleanup()
			return fmt.Errorf("upload inline image %d: %w", i+1, err)
		}
		imageIDs = append(imageIDs, imageID)
		dimensions, err := png.DecodeConfig(bytes.NewReader(image))
		if err != nil {
			cleanup()
			return fmt.Errorf("read inline image %d dimensions: %w", i+1, err)
		}
		if markdown.Len() > 0 {
			markdown.WriteString("\n\n")
		}
		fmt.Fprintf(&markdown, "![第 %d 页 #%dpx #%dpx](%s)", i+1, dimensions.Width, dimensions.Height, imageURL)
	}
	if strings.TrimSpace(content) != "" {
		markdown.WriteString("\n\n")
		markdown.WriteString(content)
	}
	if markdown.Len() > 4000 {
		cleanup()
		return fmt.Errorf("inline Markdown message exceeds QQ's 4000-character limit")
	}
	budget, reservedCount, limitedByBudget, err := q.reservePassiveReplyBudget(u.QuoteMsgID, 1, partLimit, replyWindow)
	if err != nil {
		cleanup()
		return err
	}
	defer func() { q.releasePassiveReplyReservations(budget, reservedCount) }()
	if limitedByBudget && reservedCount < 1 {
		cleanup()
		return fmt.Errorf("QQ 被动回复额度已用完")
	}
	if err := ctx.Err(); err != nil {
		cleanup()
		return err
	}
	if budget != nil {
		q.commitPassiveReplyAttempt(budget)
		reservedCount--
	}
	if u.IsGroup {
		err = q.client.SendGroupMessage(ctx, u.ChannelID, markdown.String(), u.QuoteMsgID)
	} else {
		err = q.client.SendC2CMessage(ctx, u.ChannelID, markdown.String(), u.QuoteMsgID)
	}
	if err != nil {
		return fmt.Errorf("send QQ Markdown message with inline images: %w", err)
	}
	slog.Info("qq_channel: 单条 Markdown 卡片消息已接受，保留素材供 QQ 客户端加载", "channelID", u.ChannelID, "pages", len(images))
	return nil
}

func qqMentionMarkup(userIDs []string) string {
	var at strings.Builder
	for _, userID := range userIDs {
		if strings.TrimSpace(userID) == "" {
			continue
		}
		fmt.Fprintf(&at, `<qqbot-at-user id="%s" /> `, html.EscapeString(userID))
	}
	return at.String()
}

func (q *QQChannel) Stop(ctx context.Context) error {
	q.wsReady.Store(false)
	q.pendingInterjectionsMu.Lock()
	for groupID, pending := range q.pendingInterjections {
		pending.timer.Stop()
		delete(q.pendingInterjections, groupID)
	}
	q.pendingInterjectionsMu.Unlock()
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
	return firstErr
}

// cleanQQContent 剥离 QQ 官方网关在群聊 @ 消息中注入的 <@!bot_id> 标签并清理首尾空白
func cleanQQContent(s string) string {
	for {
		start := strings.Index(s, "<@")
		if start == -1 {
			break
		}
		end := strings.Index(s[start:], ">")
		if end == -1 {
			break
		}
		s = s[:start] + s[start+end+1:]
	}
	return core.TruncateRunes(strings.TrimSpace(s), core.MaxUserMessageRunes, "\n[消息过长，后续内容已截断]")
}
