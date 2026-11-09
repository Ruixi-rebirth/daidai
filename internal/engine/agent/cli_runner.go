package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"daidai/internal/entity"
)

// CLIAgentConfig 命令行工作者通用配置
type CLIAgentConfig struct {
	Name        string   // 引擎标识，如 "local" 或 "claude"
	Description string   // 引擎说明
	Command     string   // 二进制路径或命令名，如 "bash"
	Args        []string // 参数模板，如 ["-c", "{{GOAL}}"]
	DefaultDir  string   // 默认工作空间
	Timeout     time.Duration
}

// CLIWorker 通用命令行子进程沙箱工作者 (直接实现 entity.Worker)
type CLIWorker struct {
	cfg CLIAgentConfig
}

// NewCLIWorker 实例化通用命令行工作者
func NewCLIWorker(cfg CLIAgentConfig) *CLIWorker {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Minute
	}
	return &CLIWorker{cfg: cfg}
}

func (w *CLIWorker) Name() string {
	return w.cfg.Name
}

func (w *CLIWorker) Description() string {
	return w.cfg.Description
}

// ExecuteTicket 唤起子进程执行工单并返回真实标准输出与错误状态 (实现 entity.Worker)
func (w *CLIWorker) ExecuteTicket(ctx context.Context, t *entity.Ticket) (string, error) {
	var finalArgs []string
	for _, arg := range w.cfg.Args {
		if arg == "{{GOAL}}" {
			finalArgs = append(finalArgs, t.Goal)
		} else {
			finalArgs = append(finalArgs, strings.ReplaceAll(arg, "{{GOAL}}", t.Goal))
		}
	}

	workDir := strings.TrimSpace(t.Workspace)
	if workDir == "" {
		workDir = strings.TrimSpace(w.cfg.DefaultDir)
	}
	if strings.HasPrefix(workDir, "~") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			workDir = filepath.Join(home, strings.TrimPrefix(workDir, "~"))
		}
	}
	if workDir != "" {
		workDir = filepath.Clean(workDir)
	}

	taskCtx, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
	defer cancel()

	cmd := exec.CommandContext(taskCtx, w.cfg.Command, finalArgs...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	err := cmd.Run()
	stdoutStr := strings.TrimSpace(stdoutBuf.String())
	stderrStr := strings.TrimSpace(stderrBuf.String())

	if err != nil {
		detail := stderrStr
		if detail == "" {
			detail = stdoutStr
		}
		if detail != "" {
			return "", fmt.Errorf("[%s] 执行失败 (%v): %s", w.cfg.Name, err, detail)
		}
		return "", fmt.Errorf("[%s] 执行失败: %w", w.cfg.Name, err)
	}

	switch {
	case stdoutStr != "" && stderrStr != "":
		return stdoutStr + "\n" + stderrStr, nil
	case stdoutStr != "":
		return stdoutStr, nil
	case stderrStr != "":
		return stderrStr, nil
	default:
		return fmt.Sprintf("工单已由 %s 执行完毕（无标准输出）。", w.cfg.Name), nil
	}
}
