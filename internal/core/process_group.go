package core

import (
	"context"
	"os/exec"
	"syscall"
)

// CommandProcessGroup configures a command with its own process group so context
// cancellation also terminates children that remain in that group.
func CommandProcessGroup(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	return cmd
}
