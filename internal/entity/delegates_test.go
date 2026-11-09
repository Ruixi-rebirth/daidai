package entity

import (
	"context"
	"strings"
	"testing"
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
