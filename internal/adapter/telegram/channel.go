package telegram

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"daidai/internal/db"
	"daidai/internal/entity"
	"daidai/internal/kernel"
	"daidai/internal/media"
)

type Config struct {
	Token         string
	OwnerUsername string
	BotName       string
}
type user struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	IsBot     bool   `json:"is_bot"`
}
type message struct {
	ThreadID int64 `json:"message_thread_id"`
	Quote    *struct {
		Text string `json:"text"`
	} `json:"quote"`
	ID   int64 `json:"message_id"`
	From *user `json:"from"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Text     string   `json:"text"`
	Caption  string   `json:"caption"`
	Reply    *message `json:"reply_to_message"`
	Photo    []file   `json:"photo"`
	Document *file    `json:"document"`
	Audio    *file    `json:"audio"`
	Voice    *file    `json:"voice"`
	Video    *file    `json:"video"`
	Date     int64    `json:"date"`
}
type file struct {
	ID   string `json:"file_id"`
	Name string `json:"file_name"`
	MIME string `json:"mime_type"`
	Size int64  `json:"file_size"`
}
type update struct {
	ID      int64    `json:"update_id"`
	Message *message `json:"message"`
}
type Channel struct {
	groups   map[string]*groupState
	stopping bool
	Inbox    *media.Inbox
	cfg      Config
	database *db.Manager
	http     *http.Client
	cancel   context.CancelFunc
	done     chan struct{}
	ready    atomic.Bool
	bot      user
	mu       sync.Mutex
	recorder func(context.Context, *entity.SensoryEvent) error
	immune   *entity.ImmuneSystem
	queue    *kernel.DebounceManager
}

func New(cfg Config, database *db.Manager, recorder func(context.Context, *entity.SensoryEvent) error, immune *entity.ImmuneSystem) *Channel {
	return &Channel{cfg: cfg, database: database, recorder: recorder, immune: immune, http: &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Channel) Name() string { return "telegram" }
func (c *Channel) Ready() bool  { return c.ready.Load() }
