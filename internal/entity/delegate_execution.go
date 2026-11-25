package entity

import (
	"context"
	"fmt"

	"daidai/internal/core"
)

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

	t.Worker = workerName
	t.CancelFunc = cancel
	t.Status = TicketRunning
	touchTicket(t)
	runningSnapshot := *t
	workerTicket := *t
	workerTicket.ExecutionTimeout = d.executionTimeout
	// Worker 只接收该次执行的不可变快照，避免 /stop 并发更新调度状态时
	// 与 Worker 读取共享 Ticket 产生数据竞争；实时进度通过回调线程安全写回。
	workerTicket.CancelFunc = nil
	ticketID := t.ID
	allowSensitive := t.AllowSensitive
	execToken := t.executionToken
	workerTicket.OnLiveUpdate = func(resolvedModel, liveOut, summary string) {
		if !allowSensitive {
			liveOut = core.RedactSensitiveIPs(liveOut)
			summary = core.RedactSensitiveIPs(summary)
		}
		d.mu.Lock()
		if cur := d.tickets[ticketID]; cur != nil && cur.executionToken == execToken && cur.Status == TicketRunning {
			if resolvedModel != "" {
				cur.Model = resolvedModel
			}
			if liveOut != "" {
				cur.Output = liveOut
			}
			if summary != "" {
				cur.Summary = summary
			}
		}
		d.mu.Unlock()
	}
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
