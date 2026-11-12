package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/provider/llm"
)

// AgySessionState 保存单个会话的 AGY 上下文续接状态
type AgySessionState struct {
	mu             sync.Mutex
	runGate        chan struct{} // 同一会话内串行执行，并允许等待方响应取消
	LastCwd        string
	ConversationID string
	LastActive     time.Time
}

type agySessionKey struct {
	channelID string
	cwd       string
}

func (s *AgySessionState) Lock()   { s.mu.Lock() }
func (s *AgySessionState) Unlock() { s.mu.Unlock() }

type agyJSONOutput struct {
	Response       string `json:"response"`
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Usage          struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// AGYWorkerConfig AGY 工作区执行器配置
type AGYWorkerConfig struct {
	Command     string
	Description string
	DefaultDir  string
	Provider    *llm.AGYMindProvider
	Router      *llm.MindRouter
}

// AGYWorker 实现 entity.Worker 接口，在所选工作目录中启动 AGY CLI。
type AGYWorker struct {
	mu          sync.RWMutex
	sessions    map[agySessionKey]*AgySessionState
	command     string
	description string
	defaultDir  string
	provider    *llm.AGYMindProvider
	router      *llm.MindRouter
}

// NewAGYWorker 实例化 AGY 工作区执行器
func NewAGYWorker(defaultDir string, router *llm.MindRouter) *AGYWorker {
	return NewAGYWorkerWithConfig(AGYWorkerConfig{
		DefaultDir: defaultDir,
		Router:     router,
	})
}

// NewAGYWorkerWithConfig 按完整配置实例化 AGY 工作区执行者
func NewAGYWorkerWithConfig(cfg AGYWorkerConfig) *AGYWorker {
	cmd := strings.TrimSpace(cfg.Command)
	if cmd == "" {
		cmd = "agy"
	}
	desc := strings.TrimSpace(cfg.Description)
	if desc == "" {
		desc = "Google Antigravity 工程执行器，可在所选工作目录中检索代码、编辑文件并运行终端命令；不提供操作系统级隔离"
	}
	return &AGYWorker{
		sessions:    make(map[agySessionKey]*AgySessionState),
		command:     cmd,
		description: desc,
		defaultDir:  strings.TrimSpace(cfg.DefaultDir),
		provider:    cfg.Provider,
		router:      cfg.Router,
	}
}

func (w *AGYWorker) Name() string {
	return "agy"
}

func (w *AGYWorker) Description() string {
	return w.description
}

// GetSession 获取指定通道在特定工作空间下的续接状态 (以 ChannelID + CWD 复合键实现多项目会话隔离与并行保留)
func (w *AGYWorker) GetSession(channelID, cwd string) *AgySessionState {
	w.mu.Lock()
	defer w.mu.Unlock()

	key := agySessionKey{channelID: channelID, cwd: cwd}

	// 清理长期未活跃的过期会话，防止 map 无界增长
	if len(w.sessions) > 200 {
		now := time.Now()
		for id, sess := range w.sessions {
			sess.Lock()
			stale := now.Sub(sess.LastActive) > 24*time.Hour
			sess.Unlock()
			if stale {
				delete(w.sessions, id)
			}
		}
	}

	s, ok := w.sessions[key]
	if !ok {
		s = &AgySessionState{
			LastCwd:    cwd,
			LastActive: time.Now(),
			runGate:    make(chan struct{}, 1),
		}
		s.runGate <- struct{}{}
		w.sessions[key] = s
	} else {
		s.Lock()
		s.LastActive = time.Now()
		s.Unlock()
	}
	return s
}

// ResetSession 重置指定通道在指定工作空间下的 AGY 会话状态 (清空 ConversationID，下次工单白板启动)
func (w *AGYWorker) ResetSession(channelID, cwd string) {
	w.mu.Lock()
	key := agySessionKey{channelID: channelID, cwd: cwd}
	s := w.sessions[key]
	if s != nil {
		s.Lock()
		s.LastActive = time.Now()
	}
	w.mu.Unlock()
	if s == nil {
		return
	}
	s.Unlock()

	// Wait for an in-flight CLI process to finish before clearing its session;
	// otherwise a successful process could write its old ConversationID back.
	<-s.runGate
	defer func() { s.runGate <- struct{}{} }()
	s.Lock()
	s.ConversationID = ""
	s.LastActive = time.Now()
	s.Unlock()
}

func (w *AGYWorker) resolveWorkDir(rawWorkspace string) (string, error) {
	if dir := strings.TrimSpace(rawWorkspace); dir != "" {
		if dir == "~" {
			dir = strings.TrimSpace(w.defaultDir)
		}
		if strings.HasPrefix(dir, "~/") || strings.HasPrefix(dir, "~"+string(filepath.Separator)) {
			home, _ := os.UserHomeDir()
			dir = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(dir, "~"), string(filepath.Separator)))
		}
		if dir == "" {
			return "", fmt.Errorf("未配置默认工作空间，不能将用户主目录用作执行目录")
		}
		dir = filepath.Clean(dir)
		stat, err := os.Stat(dir)
		if err != nil || !stat.IsDir() {
			return "", fmt.Errorf("指定的工作空间目录不存在或不可访问: %s", dir)
		}
		return dir, nil
	}

	cwd := w.defaultDir
	if cwd == "~" {
		return "", fmt.Errorf("配置的默认工作空间不能是整个用户主目录")
	} else if strings.HasPrefix(cwd, "~/") || strings.HasPrefix(cwd, "~"+string(filepath.Separator)) {
		home, _ := os.UserHomeDir()
		cwd = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(cwd, "~"), string(filepath.Separator)))
	} else if !filepath.IsAbs(cwd) && cwd != "" {
		if abs, err := filepath.Abs(cwd); err == nil {
			cwd = abs
		}
	}
	if cwd != "" {
		if stat, err := os.Stat(cwd); err == nil && stat.IsDir() {
			return filepath.Clean(cwd), nil
		}
	}
	if wd, err := os.Getwd(); err == nil && wd != "" {
		return wd, nil
	}
	home, _ := os.UserHomeDir()
	return home, nil
}

func (w *AGYWorker) resolveModelAndEffort(ticketModel string) (string, string, error) {
	var baseModel, baseEffort string
	if w.provider != nil {
		baseModel, baseEffort = w.provider.GetModelAndEffort()
	}
	if w.router != nil {
		if curr := w.router.CurrentMind(); strings.EqualFold(curr.Channel, "AGY") && curr.Model != "" {
			baseModel = curr.Model
			baseEffort = curr.Effort
		}
	}

	if strings.TrimSpace(ticketModel) == "" {
		return baseModel, baseEffort, nil
	}

	if w.router != nil {
		return w.router.ResolveModelAndEffortForChannel(ticketModel, baseModel, baseEffort, "AGY")
	}
	m, eff := llm.SplitModelAndEffort(ticketModel)
	if m == "" {
		m = baseModel
	}
	if eff == "" {
		eff = baseEffort
	}
	return m, eff, nil
}

// ExecuteTicket 在指定工作目录中执行工程任务 (实现 entity.Worker)
func (w *AGYWorker) ExecuteTicket(ctx context.Context, t *entity.Ticket) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("AGY 工单执行上下文不可为空")
	}
	if t == nil {
		return "", fmt.Errorf("AGY 工单不可为空")
	}
	if err := llm.CheckAGYAuth(); err != nil {
		return "", err
	}

	cwd, err := w.resolveWorkDir(t.Workspace)
	if err != nil {
		return "", err
	}
	model, effort, err := w.resolveModelAndEffort(t.Model)
	if err != nil {
		return "", err
	}

	session := w.GetSession(t.ChannelID, cwd)

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-session.runGate:
	}
	if err := ctx.Err(); err != nil {
		session.runGate <- struct{}{}
		return "", err
	}
	defer func() { session.runGate <- struct{}{} }()

	session.Lock()
	convID := session.ConversationID
	session.Unlock()

	executionGoal := t.Goal
	executionGoal += "\n\n【工单阶段边界】单次工单最多运行 5 分钟。若完整目标无法在本次时限内可靠完成，不要一次性铺开全部工作；先完成一个可独立验证、能在本阶段收敛的部分。阶段结束时明确写出：本阶段完成项、整体目标是否仍未完成、剩余阶段、下一阶段可直接执行的具体指令，以及涉及的文件或状态。不要把阶段完成表述成整体目标完成。若目标可以在本阶段完整完成，则正常完成并说明验证结果。"
	executionGoal += "\n\n【执行边界】仅在当前工作区内读取和修改文件，使用 AGY Auto 模式与终端沙箱。不要尝试访问工作区外的文件、关闭沙箱或绕过权限策略。若操作需要沙箱外权限或被拒绝，应说明受限步骤和所需权限，不要把它表述为已完成。"
	if !t.AllowSensitive {
		executionGoal += "\n\n【行为要求：当前不是核心搭档私聊。不要主动探测宿主机公网 IP，也不要在回复中输出本机公网 IP。该要求是行为指引，不代表操作系统权限隔离。】"
	}
	args := []string{
		"-p", executionGoal,
		"--output-format", "json",
		"--mode", "auto",
		"--sandbox",
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	if convID != "" {
		args = append(args, "--conversation", convID)
	}

	cmd := exec.CommandContext(ctx, w.command, args...)
	cmd.Dir = cwd
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

	startTime := time.Now()
	err = cmd.Run()
	duration := time.Since(startTime)

	if err != nil {
		if ctx.Err() == context.Canceled {
			return "", fmt.Errorf("任务已被中断 (已耗时 %s)", duration.Truncate(time.Second))
		}
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("任务执行超时 (已耗时 %s)", duration.Truncate(time.Second))
		}
		errText := strings.TrimSpace(stderrBuf.String())
		if errText == "" {
			errText = strings.TrimSpace(stdoutBuf.String())
		}
		if errRunes := []rune(errText); len(errRunes) > 800 {
			errText = string(errRunes[len(errRunes)-800:])
		}
		if stdoutBuf.Truncated() || stderrBuf.Truncated() {
			errText += " (子进程输出已截断)"
		}
		return "", fmt.Errorf("AGY 执行异常 (%v): %s", err, errText)
	}
	if stdoutBuf.Truncated() {
		return "", fmt.Errorf("AGY 输出超过 2 MiB 上限，结果不完整")
	}

	parsed, err := parseAGYWorkerOutput(stdoutBuf.Bytes())
	if err != nil {
		return "", fmt.Errorf("AGY 输出不是有效的 JSON 结果: %w", err)
	}
	status := strings.ToUpper(strings.TrimSpace(parsed.Status))
	if status != "SUCCESS" {
		return "", fmt.Errorf("AGY 执行状态未确认成功: %q", status)
	}

	if parsed.ConversationID != "" {
		session.Lock()
		session.ConversationID = parsed.ConversationID
		session.Unlock()
	}

	response := strings.TrimSpace(parsed.Response)
	if response == "" {
		response = "工单已由 AGY 工作者执行完毕。"
	}

	if !t.AllowSensitive {
		response = core.RedactSensitiveIPs(response)
	}
	return response, nil
}

func parseAGYWorkerOutput(raw []byte) (agyJSONOutput, error) {
	var parsed agyJSONOutput
	if err := json.Unmarshal(raw, &parsed); err == nil {
		return parsed, nil
	}

	start := bytes.IndexByte(raw, '{')
	end := bytes.LastIndexByte(raw, '}')
	if start < 0 || end <= start {
		return agyJSONOutput{}, fmt.Errorf("JSON 对象缺失")
	}
	if err := json.Unmarshal(raw[start:end+1], &parsed); err != nil {
		return agyJSONOutput{}, err
	}
	return parsed, nil
}
