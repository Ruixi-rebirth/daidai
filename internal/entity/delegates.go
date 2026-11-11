package entity

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"daidai/internal/core"
)

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

// Delegates 抽象工单与多工作者调度系统 (保持确定性注册顺序与全局并发资源保护)
type Delegates struct {
	mu            sync.RWMutex
	tickets       map[string]*Ticket
	workers       map[string]Worker
	order         []string
	defaultWorker string
	maxConcurrent int
	nextTicketID  uint64
	workspaceUses map[string]int
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
		tickets:       make(map[string]*Ticket),
		workers:       make(map[string]Worker),
		maxConcurrent: maxConcurrent,
		workspaceUses: make(map[string]int),
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
	d.mu.Lock()
	defer d.mu.Unlock()

	// 清理已完结的历史工单，避免长期运行内存无界增长
	if len(d.tickets) > 200 {
		for id, old := range d.tickets {
			if old.Status != TicketRunning && old.Status != TicketStopping && old.Status != TicketPending {
				delete(d.tickets, id)
			}
		}
	}

	d.nextTicketID++
	ticketID := fmt.Sprintf("tkt_%d", d.nextTicketID)

	canSeeSensitive := len(allowSensitive) > 0 && allowSensitive[0]
	t := &Ticket{
		ID:             ticketID,
		Goal:           goal,
		RequesterID:    requesterID,
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
	// Do not expose the scheduler-owned mutable record. Callers may keep or
	// modify this snapshot without racing with cancellation or state updates.
	copy := *t
	return &copy
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
	return list
}

// AbortByChannel 请求中断指定会话通道中的运行工单，并取消尚未启动的工单。
func (d *Delegates) AbortByChannel(channelID string) int {
	d.mu.Lock()

	aborted := 0
	var cancelFuncs []context.CancelFunc
	for _, t := range d.tickets {
		if t.ChannelID != channelID || (t.Status != TicketRunning && t.Status != TicketPending) {
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
		t.UpdatedAt = time.Now()
		aborted++
	}
	d.mu.Unlock()
	for _, cancel := range cancelFuncs {
		cancel()
	}
	return aborted
}

// AbortAll 请求中断所有运行工单，并取消尚未启动的工单。
func (d *Delegates) AbortAll() int {
	d.mu.Lock()

	aborted := 0
	var cancelFuncs []context.CancelFunc
	for _, t := range d.tickets {
		if t.Status != TicketRunning && t.Status != TicketPending {
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
		t.UpdatedAt = time.Now()
		aborted++
	}
	d.mu.Unlock()
	for _, cancel := range cancelFuncs {
		cancel()
	}
	return aborted
}

// ResetSession 重置指定通道在指定工作空间下的所有 Worker 内部会话状态 (例如 AGY 的 ConversationID)
func (d *Delegates) ResetSession(channelID, workspace string) {
	d.mu.RLock()
	workers := make([]Worker, 0, len(d.order))
	for _, name := range d.order {
		if worker, ok := d.workers[name]; ok {
			workers = append(workers, worker)
		}
	}
	d.mu.RUnlock()

	for _, w := range workers {
		if r, ok := w.(interface{ ResetSession(channelID, cwd string) }); ok {
			r.ResetSession(channelID, workspace)
		}
	}
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
	if targetWs == "" && len(d.workspaceUses) > 0 {
		return fmt.Errorf("有直接工具正在使用工作区，当前工单未指定工作区路径，暂不能安全启动")
	}
	if targetWs != "" {
		workspace := workspaceLeaseKey(targetWs)
		for leased := range d.workspaceUses {
			if workspacesOverlap(workspace, leased) {
				return fmt.Errorf("工作空间 %q 与其他工具正在使用的目录重叠，请等待操作结束后再提交工单", targetWs)
			}
		}
		for _, rt := range running {
			if workspacesOverlap(workspace, rt.Workspace) {
				return fmt.Errorf("工作空间冲突保护：工单 [%s] 在同一工作空间 %q 中仍占用执行权（状态：%s），请等待其退出以避免并发读写冲突", rt.ID, targetWs, rt.Status)
			}
		}
	}

	return nil
}

// ExecuteTicket 驱动对应的 Worker 执行工单 (支持按 Worker 名字路由并强制执行全局并发控制)
func (d *Delegates) ExecuteTicket(ctx context.Context, t *Ticket) (string, error) {
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
		t.UpdatedAt = time.Now()
		d.mu.Unlock()
		return "", err
	}
	if err := d.checkCapacityLocked(t); err != nil {
		t.Status = TicketFailed
		t.UpdatedAt = time.Now()
		d.mu.Unlock()
		return "", err
	}

	workerName := t.Worker
	if workerName == "" {
		workerName = d.defaultWorker
	}
	worker := d.workers[workerName]

	execCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	t.CancelFunc = cancel
	t.Status = TicketRunning
	t.UpdatedAt = time.Now()
	workerTicket := *t
	// Worker 只接收该次执行的不可变快照，避免 /stop 并发更新调度状态时
	// 与 Worker 读取共享 Ticket 产生数据竞争。
	workerTicket.CancelFunc = nil
	d.mu.Unlock()

	output, err := executeWorker(execCtx, worker, &workerTicket)
	if !t.AllowSensitive {
		output = core.RedactSensitiveIPs(output)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	t.CancelFunc = nil
	if t.Status == TicketAborted || t.Status == TicketStopping || execCtx.Err() == context.Canceled {
		t.Status = TicketAborted
		t.UpdatedAt = time.Now()
		return "", context.Canceled
	}
	if execCtx.Err() == context.DeadlineExceeded {
		t.Status = TicketFailed
		t.UpdatedAt = time.Now()
		return "", context.DeadlineExceeded
	}

	if err != nil {
		if t.Status != TicketAborted {
			t.Status = TicketFailed
		}
		t.UpdatedAt = time.Now()
		return "", err
	}

	t.Status = TicketCompleted
	t.Output = output
	t.UpdatedAt = time.Now()
	return output, nil
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
