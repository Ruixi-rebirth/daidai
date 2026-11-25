package qq

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"daidai/internal/core"
)

const (
	tokenURL            = "https://api.bot.qq.com/app/getAppAccessToken"
	apiBaseURL          = "https://api.bot.qq.com"
	maxAPIResponseBytes = 1 << 20
	maxQQMediaBytes     = core.MaxImageBytes
	maxQQVideoBytes     = core.MaxVideoBytes
)

// QuoteMetadata describes the semantic content associated with a sent QQ message.
type QuoteMetadata struct {
	Text         string // 供引用恢复的语义文字，不是平台显示 payload
	Kind         string // text、text_card、mixed_media、image、file
	TextComplete bool   // Text 是否完整；保存时若被裁剪会自动置为 false
}

// Client 负责与 QQ 开放平台 OpenAPI 进行鉴权和 REST API 通信
type Client struct {
	appID      string
	secret     string
	httpClient *http.Client

	mu          sync.RWMutex
	refreshMu   sync.Mutex
	accessToken string
	expiresAt   time.Time

	msgSeq        uint32
	onMessageSent func(msgID, refIndex, targetType, targetID string, metadata QuoteMetadata)
}

// SetOnMessageSent 注册发送成功后的消息记录回调
func (c *Client) SetOnMessageSent(fn func(msgID, refIndex, targetType, targetID string, metadata QuoteMetadata)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onMessageSent = fn
}

func (c *Client) notifySent(respBytes []byte, targetType, targetID string, metadata QuoteMetadata) {
	c.mu.RLock()
	fn := c.onMessageSent
	c.mu.RUnlock()

	if fn == nil || len(respBytes) == 0 {
		return
	}
	var res struct {
		ID      string `json:"id"`
		ExtInfo struct {
			RefIndex string `json:"ref_idx"`
		} `json:"ext_info"`
	}
	if err := json.Unmarshal(respBytes, &res); err == nil && res.ID != "" {
		slog.Info("qq: 成功登记出站消息 ID", "id", res.ID, "hasRefIndex", res.ExtInfo.RefIndex != "")
		fn(res.ID, res.ExtInfo.RefIndex, targetType, targetID, metadata)
	} else {
		slog.Warn("qq: 发送成功但响应未包含可解析的消息 ID")
	}
}

// NewClient 创建并初始化 QQ API 客户端实例
func NewClient(appID, secret string) *Client {
	// 动态高位初始序号，避免进程热重启后 msg_seq 与腾讯端缓存冲突 (40054005)
	initSeq := uint32(time.Now().UnixNano()%800000 + 100000)
	return &Client{
		appID:  appID,
		secret: secret,
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		msgSeq: initSeq,
	}
}

func (c *Client) nextSeq() uint32 {
	for {
		current := atomic.LoadUint32(&c.msgSeq)
		next := current + 1
		if next == 0 || next > 10000000 {
			next = 100000
		}
		if atomic.CompareAndSwapUint32(&c.msgSeq, current, next) {
			return next
		}
	}
}

// StartTokenRefresher 启动后台周期性检查并刷新 AccessToken 的协程
