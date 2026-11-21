package entity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

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
	d.queueContext = queueCtx
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
	d.queueMu.Lock()
	// Register under the status lock so cancellation cannot strand a ticket
	// and its attachment cleanup between validation and enqueue.
	d.mu.RLock()
	ticket := d.tickets[ticketID]
	valid := ticket != nil && ticket.Status == TicketPending
	defer d.mu.RUnlock()
	if !valid {
		d.queueMu.Unlock()
		return fmt.Errorf("工单不存在或已不在待执行状态")
	}
	if !d.queueStarted || d.queueContext.Err() != nil {
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
			if d.availableExecutionSlots() == 0 {
				break
			}
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

func (d *Delegates) availableExecutionSlots() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	limit := d.maxConcurrent
	if limit <= 0 {
		limit = 1
	}
	running := 0
	for _, ticket := range d.tickets {
		if ticket.Status == TicketRunning || ticket.Status == TicketStopping {
			running++
		}
	}
	if running >= limit {
		return 0
	}
	return limit - running
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
	// StopQueue must also terminate waiting tickets when called on its own.
	d.abortTickets(func(t *Ticket) bool {
		_, queued := pending[t.ID]
		return queued
	})
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
