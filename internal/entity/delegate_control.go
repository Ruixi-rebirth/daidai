package entity

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func (d *Delegates) AbortByChannel(platform, channelID string) int {
	return d.abortTickets(func(t *Ticket) bool { return t.Platform == platform && t.ChannelID == channelID })
}

func (d *Delegates) AbortByRequester(platform, channelID, requesterID string) int {
	return d.abortTickets(func(t *Ticket) bool {
		return t.Platform == platform && t.ChannelID == channelID && t.RequesterID == requesterID
	})
}

func (d *Delegates) AbortTicketForRequester(ticketID, platform, channelID, requesterID string) int {
	resolvedID := strings.TrimSpace(ticketID)
	d.mu.RLock()
	if _, exact := d.tickets[resolvedID]; !exact {
		matched := ""
		for _, t := range d.tickets {
			if t.Platform == platform && t.ChannelID == channelID && t.RequesterID == requesterID &&
				(t.Status == TicketRunning || t.Status == TicketPending) && matchTicketIdentifier(t.ID, resolvedID) {
				if matched != "" {
					matched = ""
					break
				}
				matched = t.ID
			}
		}
		if matched != "" {
			resolvedID = matched
		}
	}
	d.mu.RUnlock()
	return d.abortTickets(func(t *Ticket) bool {
		return t.ID == resolvedID && t.Platform == platform && t.ChannelID == channelID && t.RequesterID == requesterID
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
func (d *Delegates) ResetSession(platform, channelID, workspace string) error {
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
			ResetSession(platform, channelID, cwd string) error
		}); ok {
			if err := r.ResetSession(platform, channelID, workspace); err != nil {
				return fmt.Errorf("重置工作者 %s 会话失败: %w", w.Name(), err)
			}
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
