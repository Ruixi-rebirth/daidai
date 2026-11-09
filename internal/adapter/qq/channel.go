package qq

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
	"daidai/internal/kernel"
)

// Config 官方 QQ 通道配置 (WebSocket + OpenAPI v2)
type Config struct {
	AppID  string
	Secret string
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

	now := time.Now().Unix()
	if _, exists := d.seen[msgID]; exists {
		return true
	}

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
	return false
}

type msgCache struct {
	mu    sync.RWMutex
	items map[string]string
	keys  []string
	max   int
	db    *db.Manager
}

func newMsgCache(max int, dbMgr *db.Manager) *msgCache {
	if max <= 0 {
		max = 500
	}
	c := &msgCache{
		items: make(map[string]string),
		keys:  make([]string, 0, max),
		max:   max,
		db:    dbMgr,
	}
	if dbMgr != nil {
		if loaded, err := dbMgr.LoadMsgCache("qq", max); err == nil && len(loaded) > 0 {
			for _, item := range loaded {
				c.items[item.MsgID] = item.Content
				c.keys = append(c.keys, item.MsgID)
			}
			slog.Info("qq: 成功从 SQLite 载入消息 ID 缓存", "count", len(c.items))
		}
	}
	return c
}

func (c *msgCache) Set(msgID, content string) {
	if msgID == "" || content == "" {
		return
	}
	c.mu.Lock()
	if _, exists := c.items[msgID]; !exists {
		if len(c.keys) >= c.max {
			oldest := c.keys[0]
			c.keys = c.keys[1:]
			delete(c.items, oldest)
		}
		c.keys = append(c.keys, msgID)
	}
	c.items[msgID] = content
	c.mu.Unlock()

	if c.db != nil {
		_ = c.db.SaveMsgCache("qq", msgID, content)
	}
}

func (c *msgCache) Get(msgID string) string {
	if msgID == "" {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.items[msgID]
}

// QQChannel 官方 QQ 开放平台全双工通道 (直接实现 entity.Channel，零前缀、单层架构)
type QQChannel struct {
	cfg        Config
	client     *Client
	debounce   *kernel.DebounceManager
	immune     *entity.ImmuneSystem
	msgCache   *msgCache
	dedup      *dedupCache
	stopCancel context.CancelFunc

	wsMu        sync.Mutex
	wsSessionID string
	wsLastSeq   int64
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
	cli.SetOnMessageSent(func(msgID, content string) {
		cache.Set(msgID, content)
	})

	return &QQChannel{
		cfg:      cfg,
		client:   cli,
		immune:   immune,
		msgCache: cache,
		dedup:    newDedupCache(2000),
	}
}

func (q *QQChannel) Name() string {
	return "qq"
}

func (q *QQChannel) Start(ctx context.Context, onSensory func(event *entity.SensoryEvent)) error {
	chCtx, cancel := context.WithCancel(ctx)
	q.stopCancel = cancel

	q.debounce = kernel.NewDebounceManager(kernel.DebounceConfig{
		MergeWindow: 2 * time.Second,
	}, func(event *entity.SensoryEvent) {
		if event == nil || onSensory == nil {
			return
		}
		event.Content = strings.TrimSpace(event.Content)
		if event.Content == "" && len(event.Images) == 0 {
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
func (q *QQChannel) IngestRaw(event *entity.SensoryEvent) {
	if q.debounce != nil && event != nil {
		q.debounce.Push(event)
	}
}

// dispatchEvent 解析 WebSocket 网关下发的消息事件并推入防抖队列 (零前缀纯净 ID)
func (q *QQChannel) dispatchEvent(payload map[string]any) {
	d, ok := payload["d"].(map[string]any)
	if !ok {
		return
	}
	eventType, _ := payload["t"].(string)

	var (
		convID     string
		userID     string
		senderName string
		content    string
		msgID      string
		isGroup    bool
		images     []*core.Attachment
	)

	msgID, _ = d["id"].(string)
	rawContent, _ := d["content"].(string)
	content = cleanQQContent(rawContent)

	if msgID != "" && q.dedup != nil && q.dedup.CheckAndSet(msgID) {
		slog.Debug("qq: 拦截到重复投递的事件消息", "msgID", msgID)
		return
	}

	if msgID != "" && content != "" && q.msgCache != nil {
		q.msgCache.Set(msgID, content)
	}

	// 解析附件图片
	if attList, ok := d["attachments"].([]any); ok {
		for _, item := range attList {
			if attMap, ok := item.(map[string]any); ok {
				fileURL, _ := attMap["url"].(string)
				contentType, _ := attMap["content_type"].(string)
				if fileURL != "" {
					images = append(images, &core.Attachment{
						Kind:     core.AttachmentImage,
						URL:      fileURL,
						MimeType: contentType,
					})
				}
			}
		}
	}

	switch eventType {
	case "GROUP_AT_MESSAGE_CREATE": // 群聊中被 @ 的消息
		isGroup = true
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
		return
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
			refContent = q.msgCache.Get(refMsgID)
		}

		slog.Info("qq: 处理引用消息", "msgID", msgID, "refMsgID", refMsgID, "hitCache", refContent != "")

		if refContent != "" {
			replyCtx = &entity.SensoryEvent{
				Content: strings.TrimSpace(refContent),
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
							replyCtx = &entity.SensoryEvent{
								Content: strings.TrimSpace(c),
							}
							slog.Info("qq: 从 v2 msg_elements 中成功解析出引用消息", "msgID", msgID)
							break
						}
					}
				}
			}
		}
	}

	if convID != "" && (content != "" || len(images) > 0) {
		role := entity.RoleGuest
		if q.immune != nil {
			role = q.immune.ResolveRole(userID)
		}
		q.IngestRaw(&entity.SensoryEvent{
			Platform:     "qq",
			ChannelID:    convID,
			IsGroup:      isGroup,
			SenderID:     userID,
			SenderName:   strings.TrimSpace(senderName),
			Role:         role,
			Content:      strings.TrimSpace(content),
			Images:       images,
			ReplyContext: replyCtx,
			RawMessageID: msgID,
			OccurredAt:   time.Now(),
		})
	}
}

// Transmit 物理发声：投递全息表达体到 QQ 客户端 (支持大文本自动分片与图文混合发送)
func (q *QQChannel) Transmit(ctx context.Context, u *entity.Utterance) error {
	if u == nil || q.client == nil {
		return nil
	}

	imgData := u.CardData
	content := u.Text
	if content == "" && len(imgData) == 0 {
		return nil
	}

	if u.ChannelID == "" {
		return fmt.Errorf("empty channelID in utterance")
	}

	chunks := core.SplitMarkdownChunks(content, 1200)
	if len(chunks) == 0 {
		chunks = []string{""}
	}

	for i, chunk := range chunks {
		if i > 0 {
			time.Sleep(150 * time.Millisecond)
		}

		quoteID := ""
		var media []byte
		if i == 0 {
			quoteID = u.QuoteMsgID
			media = imgData
		}

		var err error
		if u.IsGroup {
			if len(media) > 0 {
				err = q.client.SendGroupMediaMessage(ctx, u.ChannelID, chunk, media, quoteID)
			} else {
				err = q.client.SendGroupMessage(ctx, u.ChannelID, chunk, quoteID)
			}
		} else {
			if len(media) > 0 {
				err = q.client.SendC2CMediaMessage(ctx, u.ChannelID, chunk, media, quoteID)
			} else {
				err = q.client.SendC2CMessage(ctx, u.ChannelID, chunk, quoteID)
			}
		}

		if err != nil {
			slog.Error("qq_channel: 发送失败", "channelID", u.ChannelID, "isGroup", u.IsGroup, "quoteID", quoteID, "err", err)
			return err
		}
	}

	slog.Info("qq_channel: 表达成功送达客户端", "channelID", u.ChannelID, "isGroup", u.IsGroup, "hasMedia", u.HasMedia())
	return nil
}

func (q *QQChannel) Stop(ctx context.Context) error {
	if q.stopCancel != nil {
		q.stopCancel()
	}
	if q.debounce != nil {
		q.debounce.Stop()
	}
	return nil
}

// cleanQQContent 剥离 QQ 官方网关在群聊 @ 消息中注入的 <@!bot_id> 标签并清理首尾空白
func cleanQQContent(s string) string {
	for {
		start := strings.Index(s, "<@!")
		if start == -1 {
			break
		}
		end := strings.Index(s[start:], ">")
		if end == -1 {
			break
		}
		s = s[:start] + s[start+end+1:]
	}
	return strings.TrimSpace(s)
}
