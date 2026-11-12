package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"daidai/internal/core"
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

// CLIWorker 通用命令行子进程工作者 (直接实现 entity.Worker；不提供操作系统级隔离)
type CLIWorker struct {
	cfg CLIAgentConfig
}

// NewCLIWorker 实例化通用命令行工作者
func NewCLIWorker(cfg CLIAgentConfig) *CLIWorker {
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
	if ctx == nil {
		return "", fmt.Errorf("[%s] ticket execution context cannot be nil", w.cfg.Name)
	}
	if t == nil {
		return "", fmt.Errorf("[%s] ticket cannot be nil", w.cfg.Name)
	}
	if strings.TrimSpace(w.cfg.Command) == "" {
		return "", fmt.Errorf("[%s] command cannot be empty", w.cfg.Name)
	}

	var finalArgs []string
	passesGoal := false
	for _, arg := range w.cfg.Args {
		if strings.Contains(arg, "{{GOAL}}") {
			passesGoal = true
		}
		if arg == "{{GOAL}}" {
			finalArgs = append(finalArgs, t.Goal)
		} else {
			finalArgs = append(finalArgs, strings.ReplaceAll(arg, "{{GOAL}}", t.Goal))
		}
	}
	if !passesGoal {
		return "", fmt.Errorf("[%s] args must include {{GOAL}} so the task reaches the command", w.cfg.Name)
	}

	workDir := strings.TrimSpace(t.Workspace)
	if workDir == "" {
		workDir = strings.TrimSpace(w.cfg.DefaultDir)
	}
	if workDir == "~" {
		workDir = strings.TrimSpace(w.cfg.DefaultDir)
	}
	if strings.HasPrefix(workDir, "~/") || strings.HasPrefix(workDir, "~"+string(filepath.Separator)) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			workDir = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(workDir, "~"), string(filepath.Separator)))
		}
	}
	if workDir == "" {
		workDir = "."
	}
	if workDir != "" {
		absWorkDir, err := filepath.Abs(filepath.Clean(workDir))
		if err != nil {
			return "", fmt.Errorf("[%s] resolve working directory: %w", w.cfg.Name, err)
		}
		workDir, err = filepath.EvalSymlinks(absWorkDir)
		if err != nil {
			return "", fmt.Errorf("[%s] resolve working directory %q: %w", w.cfg.Name, absWorkDir, err)
		}
		workDir = filepath.Clean(workDir)
		info, err := os.Stat(workDir)
		if err != nil {
			return "", fmt.Errorf("[%s] inspect working directory %q: %w", w.cfg.Name, workDir, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("[%s] working directory %q is not a directory", w.cfg.Name, workDir)
		}
		if workDir == string(filepath.Separator) {
			return "", fmt.Errorf("[%s] working directory cannot be the filesystem root", w.cfg.Name)
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if resolvedHome, err := filepath.EvalSymlinks(home); err == nil {
				home = resolvedHome
			}
			home, err = filepath.Abs(filepath.Clean(home))
			if err != nil {
				return "", fmt.Errorf("[%s] resolve user home directory: %w", w.cfg.Name, err)
			}
			if workDir == home {
				return "", fmt.Errorf("[%s] working directory cannot be the entire user home", w.cfg.Name)
			}
		}
	}

	taskCtx := ctx
	cancel := func() {}
	if w.cfg.Timeout > 0 {
		taskCtx, cancel = context.WithTimeout(ctx, w.cfg.Timeout)
	}
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
	cmd.WaitDelay = 2 * time.Second

	stdoutBuf := core.NewLimitedBuffer(2 << 20)
	stderrBuf := core.NewLimitedBuffer(2 << 20)
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	err := cmd.Run()
	stdoutStr := strings.TrimSpace(stdoutBuf.String())
	stderrStr := strings.TrimSpace(stderrBuf.String())
	if stdoutBuf.Truncated() {
		stdoutStr += "\n[标准输出已截断，最多保留 2 MiB]"
	}
	if stderrBuf.Truncated() {
		stderrStr += "\n[标准错误已截断，最多保留 2 MiB]"
	}

	if err != nil {
		detail := stderrStr
		if detail == "" {
			detail = stdoutStr
		}
		var partial string
		if stdoutStr != "" {
			partial = "标准输出：\n" + stdoutStr
		}
		if stderrStr != "" {
			if partial != "" {
				partial += "\n\n"
			}
			partial += "标准错误：\n" + stderrStr
		}
		if partial != "" {
			if stdoutBuf.Truncated() || stderrBuf.Truncated() {
				partial += "\n[子进程输出已截断]"
			}
			partial = "【本阶段未完成，以下是子进程部分输出】\n" + core.TruncateRunes(partial, 6000, "\n[阶段输出超过长度限制，后续内容已截断]")
		}
		if detail != "" {
			return partial, fmt.Errorf("[%s] 执行失败 (%v): %s", w.cfg.Name, err, detail)
		}
		return partial, fmt.Errorf("[%s] 执行失败: %w", w.cfg.Name, err)
	}
	if stdoutBuf.Truncated() {
		partial := stdoutStr
		if stderrStr != "" {
			if partial != "" {
				partial += "\n\n"
			}
			partial += "标准错误：\n" + stderrStr
		}
		return partial, fmt.Errorf("[%s] 标准输出超过 2 MiB 上限，工单结果不完整", w.cfg.Name)
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
