package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"daidai/internal/entity"
)

func TestCLIWorker_ExecuteTicketAndCaptureOutput(t *testing.T) {
	worker := NewCLIWorker(CLIAgentConfig{
		Name:        "local",
		Description: "shell test",
		Command:     "sh",
		Args:        []string{"-c", "{{GOAL}}"},
		Timeout:     5 * time.Second,
	})

	out, err := worker.ExecuteTicket(context.Background(), &entity.Ticket{
		ID:   "tkt-1",
		Goal: "echo 'executing test step'; echo 'done'",
	})
	if err != nil {
		t.Fatalf("ExecuteTicket failed: %v", err)
	}

	if !strings.Contains(out, "executing test step") || !strings.Contains(out, "done") {
		t.Fatalf("expected stdout captured in ticket result, got: %q", out)
	}

	// 验证非零退出码返回真实 error
	_, errFail := worker.ExecuteTicket(context.Background(), &entity.Ticket{
		ID:   "tkt-2",
		Goal: "echo 'fatal failure' >&2; exit 1",
	})
	if errFail == nil {
		t.Fatalf("expected error on non-zero exit code")
	}
	if !strings.Contains(errFail.Error(), "fatal failure") {
		t.Fatalf("expected stderr detail in error, got: %v", errFail)
	}
}
