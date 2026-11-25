package entity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"daidai/internal/db"
)

var errTicketCapacityUnavailable = errors.New("ticket execution capacity is temporarily unavailable")

const maxQueuedAgentTickets = 100

type queuedTicketCleanup func(*Ticket, string, error)
type queuedTicketCompletion func(context.Context, *Ticket, string, error)

// WorkerInfo 工作者摘要信息 (供大脑认知与 Tool 参数自省)
type WorkerInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func workspaceLeaseKey(path string) string {
	path = strings.TrimSpace(path)
	if path == "" || filepath.Clean(path) == "." {
		return ""
	}
	return resolvePathThroughExistingParent(path)
}

func workspacesOverlap(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a = workspaceLeaseKey(a)
	b = workspaceLeaseKey(b)
	if a == "" || b == "" {
		return false
	}
	rel, err := filepath.Rel(a, b)
	if err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))) {
		return true
	}
	rel, err = filepath.Rel(b, a)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func touchTicket(t *Ticket) {
	now := time.Now()
	if !now.After(t.UpdatedAt) || now.UnixMilli() <= t.UpdatedAt.UnixMilli() {
		now = t.UpdatedAt.Add(time.Millisecond)
	}
	t.UpdatedAt = now
}

// Delegates 抽象工单与多工作者调度系统 (保持确定性注册顺序与全局并发资源保护)
type Delegates struct {
	mu               sync.RWMutex
	tickets          map[string]*Ticket
	workers          map[string]Worker
	order            []string
	defaultWorker    string
	maxConcurrent    int
	executionTimeout time.Duration
	nextTicketID     uint64
	workspaceUses    map[string]int
	ticketDB         *db.Manager
	ticketPlatform   string
	queueMu          sync.Mutex
	queueCallbacks   map[string]queuedTicketCleanup
	queueWake        chan struct{}
	queueDone        chan struct{}
	queueCancel      context.CancelFunc
	queueContext     context.Context
	queueStarted     bool
	queueCompletion  queuedTicketCompletion
	queueOrder       []string
}

// AttachTicketStore restores history and marks formerly active work as interrupted.
func (d *Delegates) AttachTicketStore(ctx context.Context, manager *db.Manager, platform string) error {
	if manager == nil || strings.TrimSpace(platform) == "" {
		return fmt.Errorf("ticket store and platform are required")
	}
	if err := manager.InterruptActiveAgentTickets(ctx, platform); err != nil {
		return fmt.Errorf("恢复工单历史失败: %w", err)
	}
	d.mu.Lock()
	d.ticketDB = manager
	d.ticketPlatform = platform
	d.mu.Unlock()
	// Active tickets are deliberately not resumed after restart because a worker
	// may have completed external side effects before the process stopped.
	return nil
}

// NewDelegates 实例化工单与工作者调度系统 (默认全局最大并发为 1)
func NewDelegates(workers ...Worker) *Delegates {
	return NewDelegatesWithLimit(1, workers...)
}

// NewDelegatesWithLimit 指定全局最大工单并发数实例化调度系统
func NewDelegatesWithLimit(maxConcurrent int, workers ...Worker) *Delegates {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	d := &Delegates{
		tickets:          make(map[string]*Ticket),
		workers:          make(map[string]Worker),
		maxConcurrent:    maxConcurrent,
		executionTimeout: time.Hour,
		workspaceUses:    make(map[string]int),
	}
	for _, w := range workers {
		d.Register(w)
	}
	return d
}

// SetMaxConcurrent 动态配置全局最大并发工单数
func (d *Delegates) SetMaxConcurrent(n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if n <= 0 {
		n = 1
	}
	d.maxConcurrent = n
}

// SetExecutionTimeout configures the per-ticket hard timeout. A zero duration
// disables the scheduler deadline; parent cancellation and /stop still apply.
func (d *Delegates) SetExecutionTimeout(timeout time.Duration) error {
	if timeout < 0 {
		return fmt.Errorf("工单执行超时不能为负数")
	}
	d.mu.Lock()
	d.executionTimeout = timeout
	d.mu.Unlock()
	return nil
}

func (d *Delegates) ExecutionTimeout() time.Duration {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.executionTimeout
}

// MaxConcurrent 获取当前全局最大并发工单数
func (d *Delegates) MaxConcurrent() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.maxConcurrent <= 0 {
		return 1
	}
	return d.maxConcurrent
}

// Register 注册一个工作者实体（首个注册的工作者自动成为默认主力）
func (d *Delegates) Register(w Worker) {
	if w == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	name := w.Name()
	if _, exists := d.workers[name]; !exists {
		d.order = append(d.order, name)
	}
	d.workers[name] = w
	if d.defaultWorker == "" {
		d.defaultWorker = name
	}
}

// SetDefault 显式指定默认主力工作者名称
func (d *Delegates) SetDefault(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.defaultWorker = name
}

// DefaultName 获取当前主力默认工作者名称
func (d *Delegates) DefaultName() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.defaultWorker
}

// GetWorker 动态路由获取指定名称的工作者；若名称为空则返回主力默认工作者
func (d *Delegates) GetWorker(name string) (Worker, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if name == "" {
		name = d.defaultWorker
	}
	w, ok := d.workers[name]
	return w, ok
}

// ListWorkers 按注册顺序返回所有已挂载的工作者摘要
func (d *Delegates) ListWorkers() []WorkerInfo {
	d.mu.RLock()
	defer d.mu.RUnlock()

	list := make([]WorkerInfo, 0, len(d.order))
	for _, name := range d.order {
		if w, ok := d.workers[name]; ok {
			list = append(list, WorkerInfo{
				Name:        w.Name(),
				Description: w.Description(),
			})
		}
	}
	return list
}

// WorkerNames 按注册顺序返回所有已注册的工作者标识列表
func (d *Delegates) WorkerNames() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	names := make([]string, len(d.order))
	copy(names, d.order)
	return names
}

// CreateTicket 创建并记录新工单 (保留空的 workspace 以允许下游 Worker 使用其配置的 DefaultDir)
func (d *Delegates) CreateTicket(requesterID, channelID, goal, model, workspace, workerName string, allowSensitive ...bool) *Ticket {
	d.mu.RLock()
	platform := d.ticketPlatform
	d.mu.RUnlock()
	return d.CreateTicketForPlatform(platform, requesterID, channelID, goal, model, workspace, workerName, allowSensitive...)
}

// CreateTicketForPlatform creates a ticket scoped to the platform that received
// the request. This keeps status queries and cancellation isolated when channel
// identifiers overlap across adapters.
func (d *Delegates) CreateTicketForPlatform(platform, requesterID, channelID, goal, model, workspace, workerName string, allowSensitive ...bool) *Ticket {
	d.mu.Lock()

	// 清理已可靠持久化的终态工单，保留只有内存副本的结果。
	if len(d.tickets) > 200 {
		for id, old := range d.tickets {
			if old.Status != TicketRunning && old.Status != TicketStopping && old.Status != TicketPending && old.persistenceWarning == "" {
				delete(d.tickets, id)
			}
		}
	}

	d.nextTicketID++
	ticketID := ""
	for attempt := 0; attempt < 8; attempt++ {
		var randomID [3]byte
		if _, err := rand.Read(randomID[:]); err == nil {
			candidate := "tkt-" + hex.EncodeToString(randomID[:])
			if _, exists := d.tickets[candidate]; !exists {
				ticketID = candidate
				break
			}
		}
	}
	if ticketID == "" {
		ticketID = fmt.Sprintf("tkt-%06x", d.nextTicketID)
	}

	canSeeSensitive := len(allowSensitive) > 0 && allowSensitive[0]
	t := &Ticket{
		ID:             ticketID,
		Goal:           goal,
		RequesterID:    requesterID,
		Platform:       strings.TrimSpace(platform),
		ChannelID:      channelID,
		Worker:         strings.TrimSpace(workerName),
		Workspace:      strings.TrimSpace(workspace),
		Model:          strings.TrimSpace(model),
		Status:         TicketPending,
		AllowSensitive: canSeeSensitive,
		executionToken: d.nextTicketID,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}

	d.tickets[ticketID] = t
	copy := *t
	d.mu.Unlock()
	if !d.persistTicket(copy) {
		d.mu.Lock()
		delete(d.tickets, ticketID)
		d.mu.Unlock()
		return nil
	}
	// Do not expose the scheduler-owned mutable record. Callers may keep or
	// modify this snapshot without racing with cancellation or state updates.
	return &copy
}

// SetTicketAdditionalDirs grants the selected worker access to explicit
// input directories for a pending ticket. These paths are runtime-only and
// are intentionally not persisted with ticket history.
func (d *Delegates) SetTicketAdditionalDirs(ticketID string, dirs []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	ticket := d.tickets[ticketID]
	if ticket == nil {
		return fmt.Errorf("工单不存在")
	}
	if ticket.Status != TicketPending {
		return fmt.Errorf("工单已不在待执行状态")
	}
	cleaned := make([]string, 0, len(dirs))
	seen := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		absDir, err := filepath.Abs(filepath.Clean(dir))
		if err != nil {
			return fmt.Errorf("解析工单输入目录失败: %w", err)
		}
		if _, ok := seen[absDir]; ok {
			continue
		}
		seen[absDir] = struct{}{}
		cleaned = append(cleaned, absDir)
	}
	ticket.AdditionalDirs = cleaned
	return nil
}

func (d *Delegates) SetTicketGroup(ticketID string, isGroup bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	ticket := d.tickets[ticketID]
	if ticket == nil || ticket.Status != TicketPending {
		return fmt.Errorf("工单不存在或已不在待执行状态")
	}
	ticket.IsGroup = isGroup
	return nil
}

// StartQueue starts the in-process dispatcher. Pending work is deliberately
// not restored after restart; AttachTicketStore marks it interrupted first.
