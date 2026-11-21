package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
	"daidai/internal/provider/llm"
)

// AGYWorkerConfig AGY 工作区执行器配置
type AGYWorkerConfig struct {
	Command     string
	Description string
	DefaultDir  string
	Provider    *llm.AGYMindProvider
	Router      *llm.MindRouter
	SessionDB   *db.Manager
}

// AGYWorker 实现 entity.Worker 接口，在所选工作目录中启动 AGY CLI。
type AGYWorker struct {
	mu          sync.RWMutex
	resetMu     sync.Mutex
	sessions    map[agySessionKey]*AgySessionState
	resetting   map[agyResetScope]chan struct{}
	command     string
	description string
	defaultDir  string
	provider    *llm.AGYMindProvider
	router      *llm.MindRouter
	sessionDB   *db.Manager
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
		sessionDB:   cfg.SessionDB,
	}
}

func (w *AGYWorker) Name() string {
	return "agy"
}

func (w *AGYWorker) Description() string {
	return w.description
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

	session := w.getSession(t.Platform, t.RequesterID, t.ChannelID, cwd)
	defer w.releaseSession(session)

	if err := w.acquireSessionRun(ctx, session); err != nil {
		return "", err
	}
	defer func() { session.runGate <- struct{}{} }()

	session.Lock()
	if !session.loaded && w.sessionDB != nil {
		conversationID, loadErr := w.sessionDB.LoadAGYSession(ctx, t.Platform, t.RequesterID, t.ChannelID, cwd)
		if loadErr != nil {
			session.Unlock()
			return "", fmt.Errorf("读取 AGY 会话续接状态失败: %w", loadErr)
		}
		session.ConversationID = conversationID
	}
	session.loaded = true
	convID := session.ConversationID
	session.Unlock()

	args, err := buildAGYCommandArgs(t, model, effort, convID)
	if err != nil {
		return "", err
	}

	process, err := runAGYProcess(ctx, w.command, cwd, args)
	stdoutBuf, stderrBuf, duration := process.stdout, process.stderr, process.duration

	if err != nil {
		saveErr := w.retainConversationID(session, t, cwd, stdoutBuf.Bytes())
		partial := partialAGYOutput(stdoutBuf.String(), stderrBuf.String(), stdoutBuf.Truncated(), stderrBuf.Truncated())
		partial = appendSessionSaveWarning(partial, saveErr)
		if ctx.Err() == context.Canceled {
			return partial, fmt.Errorf("任务已被中断 (已耗时 %s)", duration.Truncate(time.Second))
		}
		if ctx.Err() == context.DeadlineExceeded {
			return partial, fmt.Errorf("任务执行超时 (已耗时 %s)", duration.Truncate(time.Second))
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
		return partial, fmt.Errorf("AGY 执行异常 (%v): %s", err, errText)
	}
	if stdoutBuf.Truncated() {
		saveErr := w.retainConversationID(session, t, cwd, stdoutBuf.Bytes())
		partial := partialAGYOutput(stdoutBuf.String(), stderrBuf.String(), true, stderrBuf.Truncated())
		partial = appendSessionSaveWarning(partial, saveErr)
		return partial, fmt.Errorf("AGY 输出超过 2 MiB 上限，结果不完整")
	}

	parsed, err := llm.ParseAGYResult(stdoutBuf.Bytes())
	if err != nil {
		partial := partialAGYOutput(stdoutBuf.String(), stderrBuf.String(), false, stderrBuf.Truncated())
		return partial, fmt.Errorf("AGY 输出不是有效的 JSON 结果: %w", err)
	}
	// A non-success response can still carry a valid conversation ID and useful
	// stage output. Preserve both so a later, explicit stage can continue safely.
	var sessionSaveErr error
	if parsed.ConversationID != "" {
		session.Lock()
		session.ConversationID = strings.TrimSpace(parsed.ConversationID)
		session.loaded = true
		conversationID := session.ConversationID
		session.Unlock()
		sessionSaveErr = w.saveConversationID(t, cwd, conversationID)
	}
	response, resultErr := agyTicketResult(parsed)
	response = appendSessionSaveWarning(response, sessionSaveErr)
	if !t.AllowSensitive {
		response = core.RedactSensitiveIPs(response)
	}
	return response, resultErr
}

func agyTicketResult(result llm.AGYResult) (string, error) {
	status := strings.ToUpper(strings.TrimSpace(result.Status))
	reason := strings.TrimSpace(result.FinishedReason)
	response := strings.TrimSpace(result.Response)

	if detail := result.FailureDetail(); detail != "" {
		return response, fmt.Errorf("AGY 执行终止: %s", detail)
	}
	if core.OutputLimitReached(reason) {
		return response, fmt.Errorf("AGY 达到输出上限，本阶段结果不完整")
	}
	if status != "SUCCESS" {
		if status == "" {
			status = "未知"
		}
		return response, fmt.Errorf("AGY 未返回成功状态（%s）", status)
	}
	if response == "" {
		response = "工单已由 AGY 工作者执行完毕。"
	}
	return response, nil
}
