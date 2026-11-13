package entity

import (
	"context"
	"strings"
	"testing"
	"time"
)

type mockWorker struct {
	name string
	desc string
}

func (m *mockWorker) Name() string        { return m.name }
func (m *mockWorker) Description() string { return m.desc }
func (m *mockWorker) ExecuteTicket(ctx context.Context, t *Ticket) (string, error) {
	return "result from " + m.name + ": " + t.Goal, nil
}

func TestDelegates_RoutingAndWorkspacePreservation(t *testing.T) {
	wAgy := &mockWorker{name: "agy", desc: "Antigravity runner"}
	wDocker := &mockWorker{name: "docker", desc: "Docker sandbox"}

	delegates := NewDelegates(wAgy, wDocker)

	if delegates.DefaultName() != "agy" {
		t.Fatalf("expected default worker to be agy, got %s", delegates.DefaultName())
	}

	// 默认未指定工作者时自动路由到主力工作者，且空 workspace 不被强行覆盖为 os.Getwd()
	tktDefault := delegates.CreateTicket("user1", "chan1", "重构核心模块", "", "", "")
	if tktDefault.Workspace != "" {
		t.Fatalf("expected empty workspace to be preserved for worker DefaultDir fallback, got %q", tktDefault.Workspace)
	}
	resDefault, err := delegates.ExecuteTicket(context.Background(), tktDefault)
	if err != nil {
		t.Fatalf("execute default ticket failed: %v", err)
	}
	if !strings.Contains(resDefault, "result from agy") {
		t.Errorf("expected routed to agy, got: %s", resDefault)
	}

	// 显式指定工作者名称时精确路由
	tktDocker := delegates.CreateTicket("user1", "chan1", "运行未知脚本", "", "", "docker")
	resDocker, err := delegates.ExecuteTicket(context.Background(), tktDocker)
	if err != nil {
		t.Fatalf("execute docker ticket failed: %v", err)
	}
	if !strings.Contains(resDocker, "result from docker") {
		t.Errorf("expected routed to docker, got: %s", resDocker)
	}

	// 调度未挂载的工作者时返回清晰错误
	tktGhost := delegates.CreateTicket("user1", "chan1", "不存在的任务", "", "", "ghost_worker")
	_, errGhost := delegates.ExecuteTicket(context.Background(), tktGhost)
	if errGhost == nil {
		t.Fatalf("expected error when routing to nonexistent worker")
	}
	if !strings.Contains(errGhost.Error(), "未挂载") {
		t.Errorf("expected error message to explain missing worker, got: %v", errGhost)
	}
}

type blockingWorker struct {
	startedCh chan struct{}
	releaseCh chan struct{}
}

func (b *blockingWorker) Name() string        { return "blocker" }
func (b *blockingWorker) Description() string { return "blocking mock worker" }
func (b *blockingWorker) ExecuteTicket(ctx context.Context, t *Ticket) (string, error) {
	close(b.startedCh)
	select {
	case <-b.releaseCh:
		return "done", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestDelegates_ConcurrencyLimit(t *testing.T) {
	bWorker := &blockingWorker{
		startedCh: make(chan struct{}),
		releaseCh: make(chan struct{}),
	}
	delegates := NewDelegatesWithLimit(1, bWorker)

	tkt1 := delegates.CreateTicket("user1", "chan1", "长任务1", "", "", "blocker")
	tkt2 := delegates.CreateTicket("user2", "chan2", "长任务2", "", "", "blocker")

	errCh := make(chan error, 1)
	go func() {
		_, err := delegates.ExecuteTicket(context.Background(), tkt1)
		errCh <- err
	}()

	// 等待任务1正式进入运行状态
	<-bWorker.startedCh

	// 此时任务2应当被全局并发容量拦截
	err := delegates.CheckCapacity(tkt2)
	if err == nil {
		t.Fatalf("expected error due to concurrency limit, got nil")
	}
	if !strings.Contains(err.Error(), "系统负载保护已生效") {
		t.Errorf("expected error message mentioning load protection, got: %v", err)
	}

	_, execErr := delegates.ExecuteTicket(context.Background(), tkt2)
	if execErr == nil || !strings.Contains(execErr.Error(), "系统负载保护已生效") {
		t.Errorf("expected ExecuteTicket to reject concurrent ticket, got: %v", execErr)
	}

	// 释放任务1
	close(bWorker.releaseCh)
	if err1 := <-errCh; err1 != nil {
		t.Fatalf("ticket1 execution failed: %v", err1)
	}

	// 任务1完成后，检查容量恢复正常
	if errRecover := delegates.CheckCapacity(tkt2); errRecover != nil {
		t.Errorf("expected capacity to be recovered after ticket1 finished, got: %v", errRecover)
	}
}

func TestDelegates_WorkspaceConflict(t *testing.T) {
	bWorker := &blockingWorker{
		startedCh: make(chan struct{}),
		releaseCh: make(chan struct{}),
	}
	// 允许全局并发 2 个，但同一工作空间冲突仍应被拦截
	delegates := NewDelegatesWithLimit(2, bWorker)

	tkt1 := delegates.CreateTicket("user1", "chan1", "任务1", "", "/home/ruixi/daidai", "blocker")
	tktConflict := delegates.CreateTicket("user2", "chan2", "冲突任务", "", "/home/ruixi/daidai", "blocker")
	tktOther := delegates.CreateTicket("user3", "chan3", "不同目录任务", "", "/home/ruixi/other", "blocker")

	go func() {
		_, _ = delegates.ExecuteTicket(context.Background(), tkt1)
	}()

	<-bWorker.startedCh

	// 相同工作空间冲突检测
	errConflict := delegates.CheckCapacity(tktConflict)
	if errConflict == nil {
		t.Fatalf("expected workspace conflict error, got nil")
	}
	if !strings.Contains(errConflict.Error(), "工作空间冲突保护") {
		t.Errorf("expected workspace conflict message, got: %v", errConflict)
	}

	// 不同工作空间应当允许
	if errOther := delegates.CheckCapacity(tktOther); errOther != nil {
		t.Errorf("expected non-conflicting workspace to pass capacity check, got: %v", errOther)
	}

	close(bWorker.releaseCh)
}

type queueTestWorker struct {
	started chan string
	release chan struct{}
}

func (w *queueTestWorker) Name() string        { return "queue-test" }
func (w *queueTestWorker) Description() string { return "controlled worker" }
func (w *queueTestWorker) ExecuteTicket(ctx context.Context, ticket *Ticket) (string, error) {
	w.started <- ticket.ID
	select {
	case <-w.release:
		return "complete", nil
	case <-ctx.Done():
		return "partial result", ctx.Err()
	}
}

func awaitQueueID(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(3 * time.Second):
		t.Fatal("queue did not make progress")
		return ""
	}
}

func TestQueueCancellationAndCompletion(t *testing.T) {
	w := &queueTestWorker{started: make(chan string, 10), release: make(chan struct{}, 10)}
	d := NewDelegates(w)
	completed := make(chan string, 10)
	cleaned := make(chan string, 10)
	if err := d.StartQueue(context.Background(), func(_ context.Context, ticket *Ticket, _ string, _ error) { completed <- ticket.ID }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := d.StopQueue(ctx); err != nil {
			t.Error(err)
		}
	})
	create := func() *Ticket {
		ticket := d.CreateTicketForPlatform("qq", "owner", "room", "goal", "", "", "")
		if err := d.EnqueueTicket(ticket.ID, func(tk *Ticket, _ string, _ error) { cleaned <- tk.ID }); err != nil {
			t.Fatal(err)
		}
		return ticket
	}
	first := create()
	if got := awaitQueueID(t, w.started); got != first.ID {
		t.Fatal(got)
	}
	second, third := create(), create()
	if d.AbortTicketForRequester(second.ID, "qq", "room", "other") != 0 {
		t.Fatal("unauthorized cancellation")
	}
	if d.AbortTicketForRequester(second.ID, "qq", "room", "owner") != 1 {
		t.Fatal("pending cancellation failed")
	}
	if got := awaitQueueID(t, cleaned); got != second.ID {
		t.Fatal(got)
	}
	if d.AbortTicketForRequester(first.ID, "qq", "room", "owner") != 1 {
		t.Fatal("running cancellation failed")
	}
	if got := awaitQueueID(t, completed); got != first.ID {
		t.Fatal(got)
	}
	if got := awaitQueueID(t, cleaned); got != first.ID {
		t.Fatal(got)
	}
	if got := awaitQueueID(t, w.started); got != third.ID {
		t.Fatal("cancelled pending task ran", got)
	}
	w.release <- struct{}{}
	if got := awaitQueueID(t, completed); got != third.ID {
		t.Fatal(got)
	}
	if got := awaitQueueID(t, cleaned); got != third.ID {
		t.Fatal(got)
	}
	for id, status := range map[string]TicketStatus{first.ID: TicketAborted, second.ID: TicketAborted, third.ID: TicketCompleted} {
		ticket, _ := d.GetTicket(id)
		if ticket.Status != status {
			t.Fatalf("%s: %s", id, ticket.Status)
		}
	}
}

func TestQueueTimeoutAndStop(t *testing.T) {
	w := &queueTestWorker{started: make(chan string, 10), release: make(chan struct{})}
	d := NewDelegates(w)
	if err := d.SetExecutionTimeout(50 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	completed := make(chan string, 10)
	if err := d.StartQueue(context.Background(), func(_ context.Context, ticket *Ticket, _ string, _ error) { completed <- ticket.ID }); err != nil {
		t.Fatal(err)
	}
	defer d.StopQueue(context.Background())
	first := d.CreateTicket("owner", "room", "goal", "", "", "")
	if err := d.EnqueueTicket(first.ID, nil); err != nil {
		t.Fatal(err)
	}
	awaitQueueID(t, w.started)
	awaitQueueID(t, completed)
	ticket, _ := d.GetTicket(first.ID)
	if ticket.Status != TicketFailed || !strings.Contains(ticket.Output, "partial result") || !strings.Contains(ticket.Output, "执行时限") {
		t.Fatalf("timeout lost status/output: %+v", ticket)
	}
	if err := d.SetExecutionTimeout(0); err != nil {
		t.Fatal(err)
	}
	running := d.CreateTicket("owner", "room", "goal", "", "", "")
	if err := d.EnqueueTicket(running.ID, nil); err != nil {
		t.Fatal(err)
	}
	awaitQueueID(t, w.started)
	pending := d.CreateTicket("owner", "room", "goal", "", "", "")
	if err := d.EnqueueTicket(pending.ID, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := d.StopQueue(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{running.ID, pending.ID} {
		ticket, _ := d.GetTicket(id)
		if ticket.Status != TicketAborted {
			t.Fatalf("stopped queue left %s in %s", id, ticket.Status)
		}
	}
}

func TestQueueRejectsEnqueueDuringShutdown(t *testing.T) {
	d := NewDelegates(&mockWorker{name: "mock"})
	ctx, cancel := context.WithCancel(context.Background())
	if err := d.StartQueue(ctx, nil); err != nil {
		t.Fatal(err)
	}
	cancel()
	ticket := d.CreateTicket("owner", "room", "goal", "", "", "")
	if err := d.EnqueueTicket(ticket.ID, nil); err == nil {
		t.Fatal("accepted work after shutdown")
	}
	if err := d.StopQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestQueueWaitDoesNotConsumeExecutionTimeout(t *testing.T) {
	w := &queueTestWorker{started: make(chan string, 10), release: make(chan struct{}, 1)}
	d := NewDelegates(w)
	if err := d.SetExecutionTimeout(0); err != nil {
		t.Fatal(err)
	}
	completed := make(chan string, 10)
	if err := d.StartQueue(context.Background(), func(_ context.Context, ticket *Ticket, _ string, _ error) { completed <- ticket.ID }); err != nil {
		t.Fatal(err)
	}
	defer d.StopQueue(context.Background())
	first := d.CreateTicket("owner", "room", "first", "", "", "")
	if err := d.EnqueueTicket(first.ID, nil); err != nil {
		t.Fatal(err)
	}
	awaitQueueID(t, w.started)
	if err := d.SetExecutionTimeout(50 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	second := d.CreateTicket("owner", "room", "second", "", "", "")
	if err := d.EnqueueTicket(second.ID, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-w.started:
		t.Fatal("concurrency limit violated", id)
	case <-time.After(100 * time.Millisecond):
	}
	ticket, _ := d.GetTicket(second.ID)
	if ticket.Status != TicketPending {
		t.Fatalf("waiting ticket timed out: %s", ticket.Status)
	}
	w.release <- struct{}{}
	awaitQueueID(t, completed)
	if got := awaitQueueID(t, w.started); got != second.ID {
		t.Fatal(got)
	}
	awaitQueueID(t, completed)
	ticket, _ = d.GetTicket(second.ID)
	if ticket.Status != TicketFailed || !strings.Contains(ticket.Output, "partial result") {
		t.Fatalf("second worker never ran: %+v", ticket)
	}
}
