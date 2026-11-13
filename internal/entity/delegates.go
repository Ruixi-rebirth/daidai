package entity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"daidai/internal/core"
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
		executionTimeout: 30 * time.Minute,
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
	var randomID [8]byte
	randomSuffix := ""
	if _, err := rand.Read(randomID[:]); err != nil {
		randomSuffix = fmt.Sprintf("%x", d.nextTicketID)
	} else {
		randomSuffix = hex.EncodeToString(randomID[:])
	}
	ticketID := fmt.Sprintf("tkt_%x_%s", time.Now().UnixMilli(), randomSuffix)

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
func (d *Delegates) StartQueue(ctx context.Context, onComplete func(context.Context, *Ticket, string, error)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	d.queueMu.Lock()
	if d.queueStarted {
		d.queueMu.Unlock()
		return fmt.Errorf("工单队列已启动")
	}
	queueCtx, cancel := context.WithCancel(ctx)
	d.queueStarted = true
	d.queueCancel = cancel
	d.queueWake = make(chan struct{}, 1)
	d.queueDone = make(chan struct{})
	d.queueCallbacks = make(map[string]queuedTicketCleanup)
	d.queueOrder = nil
	d.queueCompletion = onComplete
	done := d.queueDone
	wake := d.queueWake
	d.queueMu.Unlock()
	go func() {
		d.runQueue(queueCtx, wake)
		d.queueMu.Lock()
		d.queueStarted = false
		d.queueCancel = nil
		d.queueMu.Unlock()
		close(done)
	}()
	return nil
}

// StopQueue cancels the dispatcher and waits for owned workers within ctx.
func (d *Delegates) StopQueue(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	d.queueMu.Lock()
	if !d.queueStarted {
		d.queueMu.Unlock()
		return nil
	}
	cancel := d.queueCancel
	done := d.queueDone
	d.queueMu.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// EnqueueTicket makes a persisted pending ticket eligible for background
// execution. The optional cleanup runs exactly once after it leaves pending.
func (d *Delegates) EnqueueTicket(ticketID string, cleanup func(*Ticket, string, error)) error {
	d.mu.RLock()
	ticket := d.tickets[ticketID]
	valid := ticket != nil && ticket.Status == TicketPending
	d.mu.RUnlock()
	if !valid {
		return fmt.Errorf("工单不存在或已不在待执行状态")
	}
	d.queueMu.Lock()
	if !d.queueStarted {
		d.queueMu.Unlock()
		return fmt.Errorf("工单队列尚未启动")
	}
	if _, exists := d.queueCallbacks[ticketID]; exists {
		d.queueMu.Unlock()
		return fmt.Errorf("工单已在队列中")
	}
	if len(d.queueCallbacks) >= maxQueuedAgentTickets {
		d.queueMu.Unlock()
		return fmt.Errorf("工单等待队列已满（最多 %d 张），请稍后重试", maxQueuedAgentTickets)
	}
	d.queueCallbacks[ticketID] = cleanup
	d.queueOrder = append(d.queueOrder, ticketID)
	wake := d.queueWake
	d.queueMu.Unlock()
	select {
	case wake <- struct{}{}:
	default:
	}
	return nil
}

func (d *Delegates) runQueue(ctx context.Context, wake <-chan struct{}) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var active sync.WaitGroup
	defer func() {
		active.Wait()
		d.cleanupQueuedOnStop(ctx)
	}()

	for {
		dispatched := false
		for _, ticket := range d.pendingQueuedTickets() {
			started := make(chan struct{})
			done := make(chan struct{})
			active.Add(1)
			go func(ticket *Ticket) {
				defer active.Done()
				output, err := d.executeTicket(ctx, ticket, func() { close(started) }, true)
				if !errors.Is(err, errTicketCapacityUnavailable) {
					d.finishQueuedTicket(ctx, ticket.ID, output, err)
				}
				close(done)
			}(ticket)
			select {
			case <-started:
				dispatched = true
			case <-done:
				// A terminal result was finalized, or capacity is temporarily busy.
				if current, ok := d.GetTicket(ticket.ID); ok && current.Status != TicketPending {
					dispatched = true
				}
			case <-ctx.Done():
				return
			}
		}
		if dispatched {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-ticker.C:
		}
	}
}

func (d *Delegates) pendingQueuedTickets() []*Ticket {
	d.queueMu.Lock()
	ids := append([]string(nil), d.queueOrder...)
	d.queueMu.Unlock()

	d.mu.RLock()
	tickets := make([]*Ticket, 0, len(ids))
	for _, id := range ids {
		if ticket := d.tickets[id]; ticket != nil && ticket.Status == TicketPending {
			copy := *ticket
			tickets = append(tickets, &copy)
		}
	}
	d.mu.RUnlock()
	return tickets
}

func (d *Delegates) removeQueuedOrderLocked(ticketID string) {
	for i, id := range d.queueOrder {
		if id == ticketID {
			copy(d.queueOrder[i:], d.queueOrder[i+1:])
			d.queueOrder = d.queueOrder[:len(d.queueOrder)-1]
			return
		}
	}
}

func (d *Delegates) finishQueuedTicket(ctx context.Context, ticketID, output string, err error) {
	d.queueMu.Lock()
	cleanup, queued := d.queueCallbacks[ticketID]
	if queued {
		delete(d.queueCallbacks, ticketID)
		d.removeQueuedOrderLocked(ticketID)
	}
	onComplete := d.queueCompletion
	d.queueMu.Unlock()
	if !queued {
		return
	}
	select {
	case d.queueWake <- struct{}{}:
	default:
	}
	ticket, _ := d.GetTicket(ticketID)
	if cleanup != nil {
		cleanup(ticket, output, err)
	}
	if onComplete != nil {
		onComplete(ctx, ticket, output, err)
	}
}

func (d *Delegates) discardQueuedTicket(ticketID string, err error) {
	d.queueMu.Lock()
	cleanup, queued := d.queueCallbacks[ticketID]
	if queued {
		delete(d.queueCallbacks, ticketID)
		d.removeQueuedOrderLocked(ticketID)
	}
	d.queueMu.Unlock()
	if !queued {
		return
	}
	select {
	case d.queueWake <- struct{}{}:
	default:
	}
	if cleanup != nil {
		ticket, _ := d.GetTicket(ticketID)
		cleanup(ticket, "", err)
	}
}

func (d *Delegates) cleanupQueuedOnStop(ctx context.Context) {
	d.queueMu.Lock()
	pending := d.queueCallbacks
	d.queueCallbacks = make(map[string]queuedTicketCleanup)
	d.queueOrder = nil
	d.queueMu.Unlock()
	for id, cleanup := range pending {
		if cleanup == nil {
			continue
		}
		ticket, _ := d.GetTicket(id)
		cleanup(ticket, "", ctx.Err())
	}
}

// FailPendingTicket closes a ticket that could not be prepared for execution.
// It does not alter tickets already started, stopped, or otherwise finalized.
func (d *Delegates) FailPendingTicket(ticketID, reason string) bool {
	d.mu.Lock()
	ticket := d.tickets[ticketID]
	if ticket == nil || ticket.Status != TicketPending {
		d.mu.Unlock()
		return false
	}
	ticket.Status = TicketFailed
	ticket.Output = strings.TrimSpace(reason)
	touchTicket(ticket)
	snapshot := *ticket
	d.mu.Unlock()
	if d.persistTicket(snapshot) {
		return true
	}
	d.mu.Lock()
	if current := d.tickets[ticketID]; current != nil && current.Status == TicketFailed && current.UpdatedAt.Equal(snapshot.UpdatedAt) {
		current.persistenceWarning = "准备阶段失败状态未能写入 SQLite；当前状态只保存在运行中的进程内。"
	}
	d.mu.Unlock()
	return false
}

func (d *Delegates) persistTicket(snapshot Ticket) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return d.persistTicketContext(ctx, snapshot)
}

func (d *Delegates) persistTicketContext(ctx context.Context, snapshot Ticket) bool {
	d.mu.RLock()
	manager, defaultPlatform := d.ticketDB, d.ticketPlatform
	d.mu.RUnlock()
	platform := strings.TrimSpace(snapshot.Platform)
	if platform == "" {
		platform = defaultPlatform
	}
	if manager == nil || platform == "" {
		return true
	}
	record := db.AgentTicket{ID: snapshot.ID, RequesterID: snapshot.RequesterID, ChannelID: snapshot.ChannelID,
		Goal: snapshot.Goal, Worker: snapshot.Worker, Workspace: snapshot.Workspace, Model: snapshot.Model,
		Status: string(snapshot.Status), Output: snapshot.Output, AllowSensitive: snapshot.AllowSensitive,
		CreatedAt: snapshot.CreatedAt, UpdatedAt: snapshot.UpdatedAt}
	err := manager.SaveAgentTicket(ctx, platform, record)
	d.mu.Lock()
	defer d.mu.Unlock()
	if current := d.tickets[snapshot.ID]; current != nil && current.UpdatedAt.Equal(snapshot.UpdatedAt) {
		if err != nil {
			current.persistenceWarning = "工单状态未能写入 SQLite；当前结果只保存在运行中的进程内。"
		} else {
			current.persistenceWarning = ""
		}
	}
	if err != nil {
		slog.Error("delegate ticket persistence failed", "ticketID", snapshot.ID, "err", err)
		return false
	}
	return true
}

// TicketsFor returns only the caller's history in the current conversation.
func (d *Delegates) TicketsFor(ctx context.Context, platform, requesterID, channelID string, limit int) ([]*Ticket, error) {
	d.mu.RLock()
	manager := d.ticketDB
	d.mu.RUnlock()
	if manager == nil {
		return d.memoryTicketsFor(platform, requesterID, channelID, limit), fmt.Errorf("工单历史存储未配置")
	}
	records, err := manager.ListAgentTickets(ctx, platform, requesterID, channelID, limit)
	if err != nil {
		return d.memoryTicketsFor(platform, requesterID, channelID, limit), err
	}
	byID := make(map[string]*Ticket, len(records))
	for _, record := range records {
		byID[record.ID] = &Ticket{ID: record.ID, Platform: platform, RequesterID: record.RequesterID,
			ChannelID: record.ChannelID, Goal: record.Goal, Worker: record.Worker,
			Workspace: record.Workspace, Model: record.Model, Status: TicketStatus(record.Status),
			Output: record.Output, AllowSensitive: record.AllowSensitive,
			CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}
	}
	d.mu.RLock()
	for id, t := range d.tickets {
		if t.Platform != platform || t.RequesterID != requesterID || t.ChannelID != channelID {
			continue
		}
		cp := *t
		byID[id] = &cp
	}
	d.mu.RUnlock()
	tickets := make([]*Ticket, 0, len(byID))
	for _, ticket := range byID {
		tickets = append(tickets, ticket)
	}
	sort.Slice(tickets, func(i, j int) bool {
		if tickets[i].CreatedAt.Equal(tickets[j].CreatedAt) {
			return tickets[i].ID > tickets[j].ID
		}
		return tickets[i].CreatedAt.After(tickets[j].CreatedAt)
	})
	tickets = prioritizeActiveTickets(tickets, limit)
	var persistenceErr bool
	d.mu.RLock()
	for _, t := range d.tickets {
		if t.Platform == platform && t.RequesterID == requesterID && t.ChannelID == channelID && t.persistenceWarning != "" {
			persistenceErr = true
			break
		}
	}
	d.mu.RUnlock()
	if persistenceErr {
		return tickets, fmt.Errorf("部分工单最新状态尚未持久化")
	}
	return tickets, nil
}

func (d *Delegates) memoryTicketsFor(platform, requesterID, channelID string, limit int) []*Ticket {
	d.mu.RLock()
	tickets := make([]*Ticket, 0)
	for _, t := range d.tickets {
		if t.Platform != platform || t.RequesterID != requesterID || t.ChannelID != channelID {
			continue
		}
		cp := *t
		tickets = append(tickets, &cp)
	}
	d.mu.RUnlock()
	sort.Slice(tickets, func(i, j int) bool {
		if tickets[i].CreatedAt.Equal(tickets[j].CreatedAt) {
			return tickets[i].ID > tickets[j].ID
		}
		return tickets[i].CreatedAt.After(tickets[j].CreatedAt)
	})
	return prioritizeActiveTickets(tickets, limit)
}

func prioritizeActiveTickets(tickets []*Ticket, limit int) []*Ticket {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	selected := make([]*Ticket, 0, limit)
	selectedIDs := make(map[string]struct{}, len(tickets))
	for _, ticket := range tickets {
		if ticket.Status != TicketPending && ticket.Status != TicketRunning && ticket.Status != TicketStopping {
			continue
		}
		if len(selected) >= limit {
			break
		}
		selected = append(selected, ticket)
		selectedIDs[ticket.ID] = struct{}{}
	}
	for _, ticket := range tickets {
		if len(selected) >= limit {
			break
		}
		if _, exists := selectedIDs[ticket.ID]; exists {
			continue
		}
		selected = append(selected, ticket)
		selectedIDs[ticket.ID] = struct{}{}
	}
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].CreatedAt.Equal(selected[j].CreatedAt) {
			return selected[i].ID > selected[j].ID
		}
		return selected[i].CreatedAt.After(selected[j].CreatedAt)
	})
	return selected
}

func (d *Delegates) TicketFor(ctx context.Context, platform, requesterID, channelID, ticketID string) (*Ticket, error) {
	d.mu.RLock()
	manager := d.ticketDB
	if t, ok := d.tickets[ticketID]; ok && t.Platform == platform && t.RequesterID == requesterID && t.ChannelID == channelID {
		cp := *t
		d.mu.RUnlock()
		if cp.persistenceWarning != "" {
			return &cp, fmt.Errorf("%s", cp.persistenceWarning)
		}
		return &cp, nil
	}
	d.mu.RUnlock()
	if manager == nil {
		return nil, fmt.Errorf("工单历史存储未配置")
	}
	record, err := manager.GetAgentTicket(ctx, platform, requesterID, channelID, ticketID)
	if err != nil || record == nil {
		return nil, err
	}
	return &Ticket{ID: record.ID, Platform: platform, RequesterID: record.RequesterID, ChannelID: record.ChannelID,
		Goal: record.Goal, Worker: record.Worker, Workspace: record.Workspace, Model: record.Model,
		Status: TicketStatus(record.Status), Output: record.Output, AllowSensitive: record.AllowSensitive,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}, nil
}

// GetTicket 查询工单状态
func (d *Delegates) GetTicket(id string) (*Ticket, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	t, ok := d.tickets[id]
	if !ok || t == nil {
		return nil, false
	}
	cp := *t
	return &cp, true
}

// ActiveTickets 获取当前正在执行或待执行的所有活跃工单快照
func (d *Delegates) ActiveTickets() []*Ticket {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var list []*Ticket
	for _, t := range d.tickets {
		if t.Status == TicketRunning || t.Status == TicketStopping || t.Status == TicketPending {
			cp := *t
			list = append(list, &cp)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].ID < list[j].ID
		}
		return list[i].CreatedAt.Before(list[j].CreatedAt)
	})
	return list
}

func (d *Delegates) ActiveTicketsFor(platform, requesterID, channelID string) []*Ticket {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var list []*Ticket
	for _, t := range d.tickets {
		if t.Platform != platform || t.RequesterID != requesterID || t.ChannelID != channelID {
			continue
		}
		if t.Status == TicketRunning || t.Status == TicketStopping || t.Status == TicketPending {
			cp := *t
			list = append(list, &cp)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].ID < list[j].ID
		}
		return list[i].CreatedAt.Before(list[j].CreatedAt)
	})
	return list
}

// AbortByChannel 请求中断指定会话通道中的运行工单，并取消尚未启动的工单。
func (d *Delegates) AbortByChannel(platform, channelID string) int {
	return d.abortTickets(func(t *Ticket) bool { return t.Platform == platform && t.ChannelID == channelID })
}

func (d *Delegates) AbortByRequester(platform, channelID, requesterID string) int {
	return d.abortTickets(func(t *Ticket) bool {
		return t.Platform == platform && t.ChannelID == channelID && t.RequesterID == requesterID
	})
}

func (d *Delegates) AbortTicketForRequester(ticketID, platform, channelID, requesterID string) int {
	return d.abortTickets(func(t *Ticket) bool {
		return t.ID == ticketID && t.Platform == platform && t.ChannelID == channelID && t.RequesterID == requesterID
	})
}

func (d *Delegates) abortTickets(matches func(*Ticket) bool) int {
	return d.abortTicketsContext(context.Background(), matches)
}

func (d *Delegates) abortTicketsContext(ctx context.Context, matches func(*Ticket) bool) int {
	if ctx == nil {
		ctx = context.Background()
	}
	d.mu.Lock()

	aborted := 0
	var cancelFuncs []context.CancelFunc
	type ticketState struct {
		id        string
		status    TicketStatus
		updatedAt time.Time
	}
	var changed []ticketState
	for _, t := range d.tickets {
		if !matches(t) || (t.Status != TicketRunning && t.Status != TicketPending) {
			continue
		}
		wasRunning := t.Status == TicketRunning
		if t.CancelFunc != nil {
			cancelFuncs = append(cancelFuncs, t.CancelFunc)
			t.CancelFunc = nil
		}
		if wasRunning {
			t.Status = TicketStopping
		} else {
			t.Status = TicketAborted
		}
		touchTicket(t)
		changed = append(changed, ticketState{id: t.ID, status: t.Status, updatedAt: t.UpdatedAt})
		aborted++
	}
	d.mu.Unlock()
	for _, cancel := range cancelFuncs {
		cancel()
	}
	// Persist after cancellation so a busy SQLite database cannot delay stopping
	// the child process. A newer state may be written first; SQLite rejects stale snapshots.
	var snapshots []Ticket
	d.mu.RLock()
	for _, state := range changed {
		t := d.tickets[state.id]
		if t == nil || t.Status != state.status || !t.UpdatedAt.Equal(state.updatedAt) {
			continue
		}
		snapshots = append(snapshots, *t)
	}
	d.mu.RUnlock()
	persistCtx, cancelPersist := context.WithTimeout(ctx, 5*time.Second)
	defer cancelPersist()
	for _, snapshot := range snapshots {
		d.persistTicketContext(persistCtx, snapshot)
		if snapshot.Status == TicketAborted {
			d.discardQueuedTicket(snapshot.ID, context.Canceled)
		}
	}
	return aborted
}

// AbortAll 请求中断所有运行工单，并取消尚未启动的工单。
func (d *Delegates) AbortAll() int {
	return d.abortTickets(func(*Ticket) bool { return true })
}

// AbortAllContext sends cancellation immediately and persists state within the
// caller's shutdown deadline.
func (d *Delegates) AbortAllContext(ctx context.Context) int {
	return d.abortTicketsContext(ctx, func(*Ticket) bool { return true })
}

// ResetSession 重置指定通道在指定工作空间下的所有 Worker 内部会话状态 (例如 AGY 的 ConversationID)
func (d *Delegates) ResetSession(channelID, workspace string) error {
	d.mu.RLock()
	workers := make([]Worker, 0, len(d.order))
	for _, name := range d.order {
		if worker, ok := d.workers[name]; ok {
			workers = append(workers, worker)
		}
	}
	d.mu.RUnlock()

	for _, w := range workers {
		if r, ok := w.(interface {
			ResetSession(channelID, cwd string) error
		}); ok {
			if err := r.ResetSession(channelID, workspace); err != nil {
				return fmt.Errorf("重置工作者 %s 会话失败: %w", w.Name(), err)
			}
			continue
		}
		if r, ok := w.(interface{ ResetSession(channelID, cwd string) }); ok {
			r.ResetSession(channelID, workspace)
		}
	}
	return nil
}

// CheckCapacity 检查是否具备接纳新工单的并发容量与工作空间互斥条件
func (d *Delegates) CheckCapacity(t *Ticket) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.checkCapacityLocked(t)
}

// runIfWorkspaceIdle holds the scheduler lock while checking and running a
// workspace maintenance action, preventing a ticket from starting between the
// idle check and the action.
func (d *Delegates) runIfWorkspaceIdle(workspace string, action func() error) error {
	if action == nil {
		return fmt.Errorf("工作区维护操作不可为空")
	}
	workspace = strings.TrimSpace(workspace)
	if filepath.Clean(workspace) == "." || workspace == "" {
		return fmt.Errorf("工作区路径不可为空")
	}
	workspace = workspaceLeaseKey(workspace)

	d.mu.Lock()
	defer d.mu.Unlock()
	for leased := range d.workspaceUses {
		if workspacesOverlap(workspace, leased) {
			return fmt.Errorf("工作区 %q 与其他工具正在使用的目录重叠，请等待操作结束后再清理", workspace)
		}
	}
	for _, ticket := range d.tickets {
		if ticket.Status != TicketRunning && ticket.Status != TicketStopping && ticket.Status != TicketPending {
			continue
		}
		ticketWorkspace := workspaceLeaseKey(ticket.Workspace)
		if ticketWorkspace == "." || ticketWorkspace == "" || workspacesOverlap(workspace, ticketWorkspace) {
			return fmt.Errorf("工单 [%s] 仍可能使用工作区 %q（状态：%s），请等待任务结束后再清理", ticket.ID, workspace, ticket.Status)
		}
	}
	return action()
}

// AcquireWorkspaceLease reserves a workspace for a direct tool operation that
// is not represented by a ticket, preventing overlap with tickets and cleanup.
func (d *Delegates) AcquireWorkspaceLease(workspace string) (func(), error) {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" || filepath.Clean(workspace) == "." {
		return nil, fmt.Errorf("工作区路径不可为空")
	}
	workspace = workspaceLeaseKey(workspace)

	d.mu.Lock()
	for _, ticket := range d.tickets {
		if ticket.Status != TicketRunning && ticket.Status != TicketStopping && ticket.Status != TicketPending {
			continue
		}
		ticketWorkspace := workspaceLeaseKey(ticket.Workspace)
		if ticketWorkspace == "" || ticketWorkspace == "." || workspacesOverlap(workspace, ticketWorkspace) {
			d.mu.Unlock()
			return nil, fmt.Errorf("工单 [%s] 仍可能使用工作区 %q（状态：%s）", ticket.ID, workspace, ticket.Status)
		}
	}
	for leased := range d.workspaceUses {
		if workspacesOverlap(workspace, leased) {
			d.mu.Unlock()
			return nil, fmt.Errorf("工作区 %q 与其他工具正在使用的目录重叠", workspace)
		}
	}
	if d.workspaceUses == nil {
		d.workspaceUses = make(map[string]int)
	}
	d.workspaceUses[workspace] = 1
	d.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			delete(d.workspaceUses, workspace)
			d.mu.Unlock()
		})
	}, nil
}

func (d *Delegates) checkCapacityLocked(t *Ticket) error {
	if t == nil {
		return fmt.Errorf("工单对象不可为空")
	}
	workerName := t.Worker
	if workerName == "" {
		workerName = d.defaultWorker
	}
	if _, ok := d.workers[workerName]; !ok {
		available := strings.Join(d.order, ", ")
		return fmt.Errorf("工作者 %q 未挂载 (当前已挂载: [%s])", t.Worker, available)
	}

	var running []*Ticket
	for _, existing := range d.tickets {
		if (existing.Status == TicketRunning || existing.Status == TicketStopping) && existing.ID != t.ID {
			running = append(running, existing)
		}
	}

	maxConc := d.maxConcurrent
	if maxConc <= 0 {
		maxConc = 1
	}

	if len(running) >= maxConc {
		first := running[0]
		return fmt.Errorf("系统负载保护已生效：当前有后台工程工单仍占用执行容量（状态：%s，已运行 %s）。为保护服务器稳定性，全局限制同时运行 %d 个任务。请等待其退出，或发送 /stop 请求中断",
			first.Status, time.Since(first.CreatedAt).Truncate(time.Second), maxConc)
	}

	targetWs := strings.TrimSpace(t.Workspace)
	if targetWs == "" {
		if len(d.workspaceUses) > 0 {
			return fmt.Errorf("有直接工具正在使用工作区，当前工单未指定工作区路径，暂不能安全启动")
		}
		if len(running) > 0 {
			return fmt.Errorf("有工单正在运行，当前工单未指定工作区路径，无法确认其默认目录是否重叠")
		}
	}
	if targetWs != "" {
		workspace := workspaceLeaseKey(targetWs)
		for leased := range d.workspaceUses {
			if workspacesOverlap(workspace, leased) {
				return fmt.Errorf("工作空间 %q 与其他工具正在使用的目录重叠，请等待操作结束后再提交工单", targetWs)
			}
		}
		for _, rt := range running {
			if strings.TrimSpace(rt.Workspace) == "" || workspacesOverlap(workspace, rt.Workspace) {
				return fmt.Errorf("工作空间冲突保护：工单 [%s] 在同一工作空间 %q 中仍占用执行权（状态：%s），请等待其退出以避免并发读写冲突", rt.ID, targetWs, rt.Status)
			}
		}
	}

	return nil
}

// ExecuteTicket 驱动对应的 Worker 执行工单 (支持按 Worker 名字路由并强制执行全局并发控制)
func (d *Delegates) ExecuteTicket(ctx context.Context, t *Ticket) (string, error) {
	return d.executeTicket(ctx, t, nil, false)
}

func (d *Delegates) executeTicket(ctx context.Context, t *Ticket, onStarted func(), waitForCapacity bool) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("工单执行需要有效的 context")
	}
	d.mu.Lock()
	if t == nil {
		d.mu.Unlock()
		return "", fmt.Errorf("工单对象不可为空")
	}
	stored, ok := d.tickets[t.ID]
	if !ok || stored == nil || stored.executionToken == 0 || stored.executionToken != t.executionToken {
		d.mu.Unlock()
		return "", fmt.Errorf("工单不是由当前调度器签发的有效对象")
	}
	// Always execute the scheduler-owned record. The caller only holds a
	// snapshot and cannot alter worker, workspace, requester, or permissions.
	t = stored
	if t.Status != TicketPending {
		status := t.Status
		d.mu.Unlock()
		return "", fmt.Errorf("工单当前状态为 %q，不能重复执行", status)
	}
	if err := ctx.Err(); err != nil {
		t.Status = TicketAborted
		touchTicket(t)
		snapshot := *t
		d.mu.Unlock()
		d.persistTicket(snapshot)
		return "", err
	}
	workerName := t.Worker
	if workerName == "" {
		workerName = d.defaultWorker
	}
	worker := d.workers[workerName]
	if err := d.checkCapacityLocked(t); err != nil {
		if waitForCapacity && worker != nil {
			d.mu.Unlock()
			return "", errTicketCapacityUnavailable
		}
		t.Status = TicketFailed
		reason := err.Error()
		if !t.AllowSensitive {
			reason = core.RedactSensitiveIPs(reason)
		}
		t.Output = reason
		touchTicket(t)
		snapshot := *t
		d.mu.Unlock()
		d.persistTicket(snapshot)
		return "", err
	}

	var execCtx context.Context
	var cancel context.CancelFunc
	if d.executionTimeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, d.executionTimeout)
	} else {
		execCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	t.CancelFunc = cancel
	t.Status = TicketRunning
	touchTicket(t)
	runningSnapshot := *t
	workerTicket := *t
	workerTicket.ExecutionTimeout = d.executionTimeout
	// Worker 只接收该次执行的不可变快照，避免 /stop 并发更新调度状态时
	// 与 Worker 读取共享 Ticket 产生数据竞争。
	workerTicket.CancelFunc = nil
	d.mu.Unlock()
	if onStarted != nil {
		onStarted()
	}
	d.persistTicket(runningSnapshot)

	var output string
	var err error
	d.mu.Lock()
	shouldStartWorker := t.Status == TicketRunning && execCtx.Err() == nil
	d.mu.Unlock()
	if shouldStartWorker {
		output, err = executeWorker(execCtx, worker, &workerTicket)
	} else {
		err = execCtx.Err()
		if err == nil {
			err = context.Canceled
		}
	}
	if !t.AllowSensitive {
		output = core.RedactSensitiveIPs(output)
	}

	d.mu.Lock()
	t.CancelFunc = nil
	t.Output = output
	if t.Status == TicketAborted || t.Status == TicketStopping || execCtx.Err() == context.Canceled {
		t.Status = TicketAborted
		touchTicket(t)
		snapshot := *t
		d.mu.Unlock()
		if !d.persistTicket(snapshot) {
			snapshot.persistenceWarning = "工单状态未能写入 SQLite；当前结果只保存在运行中的进程内。"
		}
		result := d.ticketModelOutput(output, &snapshot)
		return result, context.Canceled
	}
	if execCtx.Err() == context.DeadlineExceeded {
		t.Status = TicketFailed
		t.Output = "工单达到执行时限，尚未完成。"
		if output != "" {
			t.Output += "\n\n" + output
		}
		touchTicket(t)
		snapshot := *t
		d.mu.Unlock()
		if !d.persistTicket(snapshot) {
			snapshot.persistenceWarning = "工单状态未能写入 SQLite；当前结果只保存在运行中的进程内。"
		}
		result := d.ticketModelOutput(snapshot.Output, &snapshot)
		return result, context.DeadlineExceeded
	}

	if err != nil {
		if t.Status != TicketAborted {
			t.Status = TicketFailed
		}
		reason := err.Error()
		if !t.AllowSensitive {
			reason = core.RedactSensitiveIPs(reason)
		}
		reason = core.TruncateRunes(reason, 1000, "…")
		t.Output = "工单未能完成：" + reason
		if output != "" {
			t.Output += "\n\n" + output
		}
		touchTicket(t)
		snapshot := *t
		d.mu.Unlock()
		if !d.persistTicket(snapshot) {
			snapshot.persistenceWarning = "工单状态未能写入 SQLite；当前结果只保存在运行中的进程内。"
		}
		result := d.ticketModelOutput(snapshot.Output, &snapshot)
		return result, err
	}

	t.Status = TicketCompleted
	t.Output = output
	touchTicket(t)
	snapshot := *t
	d.mu.Unlock()
	if !d.persistTicket(snapshot) {
		snapshot.persistenceWarning = "工单状态未能写入 SQLite；当前结果只保存在运行中的进程内。"
	}
	result := d.ticketModelOutput(output, &snapshot)
	return result, nil
}

func (d *Delegates) ticketModelOutput(output string, ticket *Ticket) string {
	limit := 6000
	ticketID := ""
	marker := "\n[结果过长；完整输出已保存，可用 /task %s 分页查看]"
	if ticket != nil {
		ticketID = ticket.ID
	}
	if ticket != nil && ticket.persistenceWarning != "" {
		limit = 5600
		marker = "\n[结果过长；当前进程可用 /task %s 分页查看，重启后可能丢失]"
	}
	result := core.TruncateRunes(output, limit, fmt.Sprintf(marker, ticketID))
	if ticket != nil && ticket.persistenceWarning != "" {
		result += "\n\n【持久化警告】" + ticket.persistenceWarning
	}
	return result
}

func executeWorker(ctx context.Context, worker Worker, ticket *Ticket) (output string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			output = ""
			err = fmt.Errorf("worker panicked: %v", recovered)
		}
	}()
	return worker.ExecuteTicket(ctx, ticket)
}
