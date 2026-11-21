package qq

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
)

type msgCache struct {
	mu            sync.RWMutex
	items         map[string]db.QQQuoteRecord
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
	entry db.QQQuoteRecord
	done  chan struct{}
}

const (
	qqQuoteText     = "text"
	qqQuoteTextCard = "text_card"
	qqQuoteMixed    = "mixed_media"
	qqQuoteImage    = "image"
	qqQuoteFile     = "file"
	qqQuoteUnknown  = "unknown"
)

func qqQuoteCacheKey(msgID, chatType, convID string) string {
	return chatType + "\x00" + convID + "\x00" + msgID
}

func maxQQPassiveReplies(isGroup bool) int {
	return 5
}

func newMsgCache(max int, dbMgr *db.Manager) *msgCache {
	if max <= 0 {
		max = 500
	}
	c := &msgCache{
		items: make(map[string]db.QQQuoteRecord),
		keys:  make([]string, 0, max),
		max:   max,
		db:    dbMgr,
	}
	if dbMgr != nil {
		c.persistCh = make(chan msgCacheWrite, 4096)
		c.persistDone = make(chan struct{})
		c.persistCtx, c.persistCancel = context.WithCancel(context.Background())
		go c.persistLoop(c.persistCh, c.persistDone, c.persistCtx)
	}
	if dbMgr != nil {
		if err := dbMgr.PruneQQQuoteRecords(context.Background(), max); err != nil {
			slog.Warn("qq: 清理过期引用消息缓存失败", "err", err)
		}
		if loaded, err := dbMgr.LoadQQQuoteRecords(max); err == nil && len(loaded) > 0 {
			for _, item := range loaded {
				if entity.IsSensitiveAuthCommand(item.Content) {
					if err := dbMgr.DeleteQQQuoteRecord(item.MsgID, item.ChatType, item.ConvID); err != nil {
						slog.Warn("qq: 删除检测到的 OAuth 授权码缓存失败", "msgID", item.MsgID, "err", err)
					}
					continue
				}
				key := qqQuoteCacheKey(item.MsgID, item.ChatType, item.ConvID)
				c.items[key] = item
				c.keys = append(c.keys, key)
			}
			slog.Info("qq: 成功从 SQLite 载入消息 ID 缓存", "count", len(c.items))
		}
	}
	return c
}

func (c *msgCache) persistLoop(queue chan msgCacheWrite, done chan struct{}, persistCtx context.Context) {
	defer close(done)
	for {
		select {
		case write, ok := <-queue:
			if !ok {
				return
			}
			ctx, cancel := context.WithTimeout(persistCtx, 5*time.Second)
			err := c.db.SaveQQQuoteRecord(ctx, write.entry, c.max)
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("qq: 持久化引用消息缓存失败", "msgID", write.entry.MsgID, "err", err)
			}
			if write.done != nil {
				close(write.done)
			}
		case <-persistCtx.Done():
			// Close cancels only after closing the queue. Release synchronous
			// writers for entries that will not be persisted during shutdown.
			for write := range queue {
				if write.done != nil {
					close(write.done)
				}
			}
			return
		}
	}
}

func (c *msgCache) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.persistMu.Lock()
	if c.persistCh == nil {
		c.persistMu.Unlock()
		return nil
	}
	if !c.persistClosed {
		c.persistClosed = true
		close(c.persistCh)
	}
	done := c.persistDone
	cancel := c.persistCancel
	c.persistMu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		cancel()
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
	if !c.persistClosed {
		c.persistMu.Unlock()
		return
	}
	previousDone := c.persistDone
	c.persistMu.Unlock()
	<-previousDone

	c.persistMu.Lock()
	defer c.persistMu.Unlock()
	if !c.persistClosed {
		return
	}
	c.persistCh = make(chan msgCacheWrite, 4096)
	c.persistDone = make(chan struct{})
	c.persistCtx, c.persistCancel = context.WithCancel(context.Background())
	c.persistClosed = false
	go c.persistLoop(c.persistCh, c.persistDone, c.persistCtx)
}

func (c *msgCache) Set(msgID, content, chatType, convID string) {
	c.set(msgID, content, chatType, convID, "inbound", qqQuoteText, true, true)
}

func (c *msgCache) SetTypedAsync(msgID, content, chatType, convID, contentKind string, textComplete bool) {
	c.set(msgID, content, chatType, convID, "outbound", contentKind, textComplete, false)
}

func (c *msgCache) SetInboundTypedAsync(msgID, content, chatType, convID, contentKind string, textComplete bool) {
	c.set(msgID, content, chatType, convID, "inbound", contentKind, textComplete, false)
}

func (c *msgCache) set(msgID, content, chatType, convID, direction, contentKind string, textComplete, waitForPersistence bool) {
	content = strings.TrimSpace(content)
	if msgID == "" || entity.IsSensitiveAuthCommand(content) ||
		(chatType != "private" && chatType != "group") || strings.TrimSpace(convID) == "" {
		return
	}
	if contentKind == "" {
		contentKind = qqQuoteUnknown
	}
	if len([]rune(content)) > core.MaxUserMessageRunes {
		content = core.TruncateRunes(content, core.MaxUserMessageRunes, "…[引用正文已截断]")
		textComplete = false
	}
	if !qqQuoteTextComplete(content) {
		textComplete = false
	}
	if content == "" {
		textComplete = false
	}
	key := qqQuoteCacheKey(msgID, chatType, convID)
	c.mu.Lock()
	if _, exists := c.items[key]; !exists {
		if len(c.keys) >= c.max {
			oldest := c.keys[0]
			c.keys = c.keys[1:]
			delete(c.items, oldest)
		}
		c.keys = append(c.keys, key)
	}
	entry := db.QQQuoteRecord{MsgID: msgID, Content: content, ChatType: chatType, ConvID: convID, Direction: direction, ContentKind: contentKind, TextComplete: textComplete}
	c.items[key] = entry
	c.mu.Unlock()

	if c.db == nil {
		return
	}
	c.persistMu.Lock()
	if c.persistCh == nil {
		c.persistMu.Unlock()
		return
	}
	if c.persistClosed {
		c.persistMu.Unlock()
		if !waitForPersistence {
			slog.Warn("qq: 忽略通道关闭后的引用消息缓存写入", "msgID", msgID)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := c.db.SaveQQQuoteRecord(ctx, entry, c.max)
		cancel()
		if err != nil {
			slog.Warn("qq: 通道关闭后持久化引用消息缓存失败", "msgID", msgID, "err", err)
		}
		return
	}
	write := msgCacheWrite{entry: entry}
	if waitForPersistence {
		write.done = make(chan struct{})
		c.persistCh <- write
		c.persistMu.Unlock()
		<-write.done
		return
	}
	select {
	case c.persistCh <- write:
		c.persistMu.Unlock()
	default:
		c.persistMu.Unlock()
		slog.Warn("qq: 引用消息缓存持久化队列已满，保留内存缓存但未排入 SQLite", "msgID", msgID)
	}
}

func (c *msgCache) Get(msgID, chatType, convID string) string {
	record, found := c.GetRecord(msgID, chatType, convID)
	if !found {
		return ""
	}
	return record.Content
}

func (c *msgCache) GetRecord(msgID, chatType, convID string) (db.QQQuoteRecord, bool) {
	if msgID == "" || (chatType != "private" && chatType != "group") || strings.TrimSpace(convID) == "" {
		return db.QQQuoteRecord{}, false
	}
	var ctx context.Context
	if c.db != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
	}
	c.mu.RLock()
	entry, exists := c.items[qqQuoteCacheKey(msgID, chatType, convID)]
	c.mu.RUnlock()
	if exists {
		return c.hydrateInboundRecord(ctx, entry), true
	}
	if c.db == nil {
		return db.QQQuoteRecord{}, false
	}
	entry, found, err := c.db.GetQQQuoteRecord(ctx, msgID, chatType, convID)
	if err != nil {
		slog.Warn("qq: 从持久化引用记录读取原消息失败", "msgID", msgID, "err", err)
	}
	if found {
		return c.hydrateInboundRecord(ctx, entry), true
	}
	content, err := c.db.MessageContentByPlatformID(ctx, "qq", chatType, convID, msgID)
	if err != nil {
		slog.Warn("qq: 从历史消息读取引用正文失败", "msgID", msgID, "err", err)
	}
	if content != "" {
		return db.QQQuoteRecord{MsgID: msgID, Content: content, ChatType: chatType, ConvID: convID, Direction: "inbound", ContentKind: qqQuoteText, TextComplete: qqQuoteTextComplete(content)}, true
	}
	return db.QQQuoteRecord{}, false
}

func (c *msgCache) hydrateInboundRecord(ctx context.Context, record db.QQQuoteRecord) db.QQQuoteRecord {
	if record.Direction != "inbound" || c.db == nil {
		return record
	}
	content, err := c.db.MessageContentByPlatformID(ctx, "qq", record.ChatType, record.ConvID, record.MsgID)
	if err != nil {
		slog.Warn("qq: 从正式聊天历史补充入站引用正文失败", "msgID", record.MsgID, "err", err)
		return record
	}
	if content != "" {
		record.Content = content
		record.TextComplete = record.TextComplete && record.ContentKind != qqQuoteImage && record.ContentKind != qqQuoteFile && qqQuoteTextComplete(content)
	}
	return record
}

// QQChannel 官方 QQ 开放平台全双工通道 (直接实现 entity.Channel，零前缀、单层架构)
