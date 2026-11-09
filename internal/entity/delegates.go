package entity

import (
	"context"
	"fmt"
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

// Delegates 抽象工单与多沙箱工作者调度系统 (保持确定性注册顺序与全局并发资源保护)
type Delegates struct {
	mu            sync.RWMutex
	tickets       map[string]*Ticket
	workers       map[string]Worker
	order         []string
	defaultWorker string
	maxConcurrent int
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

// Register 注册一个沙箱工作者实体（首个注册的工作者自动成为默认主力）
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
func (d *Delegates) CreateTicket(requesterID, channelID, goal, model, workspace, workerName string) *Ticket {
	d.mu.Lock()
	defer d.mu.Unlock()

	// 清理已完结的历史工单，避免长期运行内存无界增长
	if len(d.tickets) > 200 {
		for id, old := range d.tickets {
			if old.Status != TicketRunning && old.Status != TicketPending {
				delete(d.tickets, id)
			}
		}
	}

	ticketID := fmt.Sprintf("tkt_%d", time.Now().UnixNano())

	t := &Ticket{
		ID:          ticketID,
		Goal:        goal,
		RequesterID: requesterID,
		ChannelID:   channelID,
		Worker:      strings.TrimSpace(workerName),
		Workspace:   strings.TrimSpace(workspace),
		Model:       strings.TrimSpace(model),
		Status:      TicketPending,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	d.tickets[ticketID] = t
	return t
}

// GetTicket 查询工单状态
func (d *Delegates) GetTicket(id string) (*Ticket, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	t, ok := d.tickets[id]
	return t, ok
}

// ActiveTickets 获取当前正在执行或待执行的所有活跃工单快照
func (d *Delegates) ActiveTickets() []*Ticket {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var list []*Ticket
	for _, t := range d.tickets {
		if t.Status == TicketRunning || t.Status == TicketPending {
			cp := *t
			list = append(list, &cp)
		}
	}
	return list
}

// AbortByChannel 紧急中断指定会话通道中正在执行的工单
func (d *Delegates) AbortByChannel(channelID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()

	aborted := 0
	for _, t := range d.tickets {
		if t.Status == TicketRunning && t.ChannelID == channelID && t.CancelFunc != nil {
			t.CancelFunc()
			t.CancelFunc = nil
			t.Status = TicketAborted
			t.UpdatedAt = time.Now()
			aborted++
		}
	}
	return aborted
}

// AbortAll 紧急熔断打断所有正在执行的工单
func (d *Delegates) AbortAll() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	aborted := 0
	for _, t := range d.tickets {
		if t.Status == TicketRunning && t.CancelFunc != nil {
			t.CancelFunc()
			t.CancelFunc = nil
			t.Status = TicketAborted
			t.UpdatedAt = time.Now()
			aborted++
		}
	}
	return aborted
}

// ResetSession 重置指定通道在指定工作空间下的所有 Worker 内部会话状态 (例如 AGY 的 ConversationID)
func (d *Delegates) ResetSession(channelID, workspace string) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, w := range d.workers {
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
		return fmt.Errorf("沙箱工作者 %q 未挂载 (当前已挂载: [%s])", t.Worker, available)
	}

	var running []*Ticket
	for _, existing := range d.tickets {
		if existing.Status == TicketRunning && existing.ID != t.ID {
			running = append(running, existing)
		}
	}

	maxConc := d.maxConcurrent
	if maxConc <= 0 {
		maxConc = 1
	}

	if len(running) >= maxConc {
		first := running[0]
		return fmt.Errorf("系统负载保护已生效：当前已有后台工程工单 [%s] 正在执行（目标：%s，已运行 %s）。为保护服务器稳定性，全局限制同时运行 %d 个沙箱任务。请等待其执行完毕，或发送 /stop 中断现有工单",
			first.ID, first.Goal, time.Since(first.CreatedAt).Truncate(time.Second), maxConc)
	}

	targetWs := strings.TrimSpace(t.Workspace)
	if targetWs != "" {
		for _, rt := range running {
			if strings.TrimSpace(rt.Workspace) == targetWs {
				return fmt.Errorf("工作空间冲突保护：工单 [%s] 正在同一工作空间 %q 下运行，请等待其执行完毕以避免并发读写冲突", rt.ID, targetWs)
			}
		}
	}

	return nil
}

// ExecuteTicket 驱动对应的 Worker 执行工单 (支持按 Worker 名字路由并强制执行全局并发控制)
func (d *Delegates) ExecuteTicket(ctx context.Context, t *Ticket) (string, error) {
	d.mu.Lock()
	if err := d.checkCapacityLocked(t); err != nil {
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
	d.mu.Unlock()

	output, err := worker.ExecuteTicket(execCtx, t)
	if !t.AllowSensitive {
		output = core.RedactSensitiveIPs(output)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	t.CancelFunc = nil

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
