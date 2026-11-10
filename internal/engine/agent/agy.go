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
	runMu          sync.Mutex // 保护同一会话内 CLI 进程串行执行，防止并发工单踩踏同一 ConversationID
	LastCwd        string
	ConversationID string
	LastActive     time.Time
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

// AGYWorkerConfig AGY 隔离沙箱工作者初始化配置
type AGYWorkerConfig struct {
	Command     string
	Description string
	DefaultDir  string
	Provider    *llm.AGYMindProvider
	Router      *llm.MindRouter
}

// AGYWorker 实现 entity.Worker 接口的 Antigravity 隔离沙箱工作者
type AGYWorker struct {
	mu          sync.RWMutex
	sessions    map[string]*AgySessionState
	command     string
	description string
	defaultDir  string
	provider    *llm.AGYMindProvider
	router      *llm.MindRouter
}

// NewAGYWorker 实例化 AGY 隔离沙箱工作者
func NewAGYWorker(defaultDir string, router *llm.MindRouter) *AGYWorker {
	return NewAGYWorkerWithConfig(AGYWorkerConfig{
		DefaultDir: defaultDir,
		Router:     router,
	})
}

// NewAGYWorkerWithConfig 按完整配置实例化 AGY 隔离沙箱工作者
func NewAGYWorkerWithConfig(cfg AGYWorkerConfig) *AGYWorker {
	cmd := strings.TrimSpace(cfg.Command)
	if cmd == "" {
		cmd = "agy"
	}
	desc := strings.TrimSpace(cfg.Description)
	if desc == "" {
		desc = "Google Antigravity 自驱智能体沙箱，具备完整的代码库检索、文件编辑、终端执行与自省反思能力"
	}
	return &AGYWorker{
		sessions:    make(map[string]*AgySessionState),
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

	key := channelID
	if cwd != "" {
		key = channelID + "::" + cwd
	}

	// 清理长期未活跃的过期会话，防止 map 无界增长
	if len(w.sessions) > 200 {
		now := time.Now()
		for id, sess := range w.sessions {
			if now.Sub(sess.LastActive) > 24*time.Hour {
				delete(w.sessions, id)
			}
		}
	}

	s, ok := w.sessions[key]
	if !ok {
		s = &AgySessionState{
			LastCwd:    cwd,
			LastActive: time.Now(),
		}
		w.sessions[key] = s
	} else {
		s.LastActive = time.Now()
	}
	return s
}

// ResetSession 重置指定通道在指定工作空间下的 AGY 会话状态 (清空 ConversationID，下次工单白板启动)
func (w *AGYWorker) ResetSession(channelID, cwd string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	key := channelID
	if cwd != "" {
		key = channelID + "::" + cwd
	}

	if s, ok := w.sessions[key]; ok {
		s.Lock()
		s.ConversationID = ""
		s.LastActive = time.Now()
		s.Unlock()
	}
}

func (w *AGYWorker) resolveWorkDir(rawWorkspace string) (string, error) {
	if dir := strings.TrimSpace(rawWorkspace); dir != "" {
		if strings.HasPrefix(dir, "~") {
			home, _ := os.UserHomeDir()
			dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
		}
		dir = filepath.Clean(dir)
		stat, err := os.Stat(dir)
		if err != nil || !stat.IsDir() {
			return "", fmt.Errorf("指定的工作空间目录不存在或不可访问: %s", dir)
		}
		return dir, nil
	}

	cwd := w.defaultDir
	if strings.HasPrefix(cwd, "~") {
		home, _ := os.UserHomeDir()
		cwd = filepath.Join(home, strings.TrimPrefix(cwd, "~"))
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

func (w *AGYWorker) resolveModelAndEffort(ticketModel string) (string, string) {
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
		return baseModel, baseEffort
	}

	if w.router != nil {
		return w.router.ResolveModelAndEffort(ticketModel, baseModel, baseEffort)
	}
	m, eff := llm.SplitModelAndEffort(ticketModel)
	if m == "" {
		m = baseModel
	}
	if eff == "" {
		eff = baseEffort
	}
	return m, eff
}

// ExecuteTicket 执行指定的独立沙箱工单 (实现 entity.Worker)
func (w *AGYWorker) ExecuteTicket(ctx context.Context, t *entity.Ticket) (string, error) {
	if strings.Contains(w.command, "agy") {
		if err := llm.CheckAGYAuth(); err != nil {
			return "", err
		}
	}

	cwd, err := w.resolveWorkDir(t.Workspace)
	if err != nil {
		return "", err
	}
	model, effort := w.resolveModelAndEffort(t.Model)

	session := w.GetSession(t.ChannelID, cwd)

	session.runMu.Lock()
	defer session.runMu.Unlock()

	session.Lock()
	convID := session.ConversationID
	session.Unlock()

	safeGoal := t.Goal
	if !t.AllowSensitive {
		safeGoal += "\n\n【安全红线：当前非伙伴私聊会话，严禁执行任何探测宿主机本机公网 IP 的命令（如 curl ifconfig.me / ip.sb / cip.cc 等），严禁在回复中输出本机公网 IP 地址。】"
	}
	args := []string{
		"-p", safeGoal,
		"--output-format", "json",
		"--dangerously-skip-permissions",
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

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

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
		return "", fmt.Errorf("AGY 执行异常 (%v): %s", err, errText)
	}

	var parsed agyJSONOutput
	rawOut := stdoutBuf.Bytes()
	if jErr := json.Unmarshal(rawOut, &parsed); jErr != nil {
		start := bytes.IndexByte(rawOut, '{')
		end := bytes.LastIndexByte(rawOut, '}')
		if start >= 0 && end > start {
			_ = json.Unmarshal(rawOut[start:end+1], &parsed)
		}
	}

	if parsed.ConversationID != "" {
		session.Lock()
		session.ConversationID = parsed.ConversationID
		session.Unlock()
	}

	response := strings.TrimSpace(parsed.Response)
	if response == "" {
		if parsed.Status != "" && parsed.Status != "SUCCESS" {
			return "", fmt.Errorf("AGY 执行状态异常: %s", parsed.Status)
		}
		response = "工单已由 AGY 工作者执行完毕。"
	}

	if !t.AllowSensitive {
		response = core.RedactSensitiveIPs(response)
	}
	return response, nil
}
