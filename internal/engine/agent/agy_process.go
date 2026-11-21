package agent

import (
	"context"
	"time"

	"daidai/internal/core"
)

const agyOutputLimit = 2 << 20

type agyProcessResult struct {
	stdout   *core.LimitedBuffer
	stderr   *core.LimitedBuffer
	duration time.Duration
}

func runAGYProcess(ctx context.Context, command, cwd string, args []string) (agyProcessResult, error) {
	cmd := core.CommandProcessGroup(ctx, command, args...)
	cmd.Dir = cwd
	cmd.WaitDelay = 2 * time.Second

	result := agyProcessResult{
		stdout: core.NewLimitedBuffer(agyOutputLimit),
		stderr: core.NewLimitedBuffer(agyOutputLimit),
	}
	cmd.Stdout = result.stdout
	cmd.Stderr = result.stderr

	startedAt := time.Now()
	err := cmd.Run()
	result.duration = time.Since(startedAt)
	return result, err
}
