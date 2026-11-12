package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
	"daidai/internal/provider/llm"
)

// AgySessionState 保存单个会话的 AGY 上下文续接状态
type AgySessionState struct {
	mu             sync.Mutex
	runGate        chan struct{} // 同一会话内串行执行，并允许等待方响应取消
	users          int           // 受 AGYWorker.mu 保护，包含等待 runGate 的工单
	LastCwd        string
	ConversationID string
	loaded         bool
	LastActive     time.Time
}

type agySessionKey struct {
	requesterID string
	channelID   string
	cwd         string
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
	SessionDB   *db.Manager
	Platform    string
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
	sessionDB   *db.Manager
	platform    string
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
		platform:    strings.TrimSpace(cfg.Platform),
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
	session := w.getSession("", channelID, cwd)
	w.releaseSession(session)
	return session
}

func (w *AGYWorker) getSession(requesterID, channelID, cwd string) *AgySessionState {
	w.mu.Lock()
	defer w.mu.Unlock()

	key := agySessionKey{requesterID: requesterID, channelID: channelID, cwd: cwd}

	// 清理长期未活跃的过期会话，防止 map 无界增长
	if len(w.sessions) > 200 {
		now := time.Now()
		for id, sess := range w.sessions {
			if sess.users > 0 {
				continue
			}
			sess.Lock()
			stale := now.Sub(sess.LastActive) > 24*time.Hour && len(sess.runGate) > 0
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
	s.users++
	return s
}

func (w *AGYWorker) releaseSession(session *AgySessionState) {
	if session == nil {
		return
	}
	w.mu.Lock()
	if session.users > 0 {
		session.users--
	}
	w.mu.Unlock()
}

// ResetSession 重置指定通道在指定工作空间下的 AGY 会话状态 (清空 ConversationID，下次工单白板启动)
func (w *AGYWorker) ResetSession(channelID, cwd string) error {
	resolvedCWD, resolveErr := w.resolveWorkDir(cwd)
	if resolveErr != nil {
		return fmt.Errorf("无法解析 AGY 会话工作区 %q: %w", cwd, resolveErr)
	}
	cwd = resolvedCWD
	w.mu.Lock()
	defer w.mu.Unlock()
	var sessions []*AgySessionState
	for key, s := range w.sessions {
		if key.channelID != channelID || key.cwd != cwd {
			continue
		}
		sessions = append(sessions, s)
	}
	for _, s := range sessions {
		<-s.runGate
	}
	if w.sessionDB != nil && w.platform != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := w.sessionDB.DeleteAGYSessions(ctx, w.platform, channelID, cwd)
		cancel()
		if err != nil {
			for _, s := range sessions {
				s.runGate <- struct{}{}
			}
			return fmt.Errorf("删除持久化 AGY 会话失败: %w", err)
		}
	}
	for _, s := range sessions {
		s.Lock()
		s.ConversationID = ""
		s.loaded = true
		s.LastActive = time.Now()
		s.Unlock()
		s.runGate <- struct{}{}
	}
	return nil
}

func (w *AGYWorker) resolveWorkDir(rawWorkspace string) (string, error) {
	dir := strings.TrimSpace(rawWorkspace)
	if dir == "" {
		dir = strings.TrimSpace(w.defaultDir)
	}
	if dir == "~" {
		dir = strings.TrimSpace(w.defaultDir)
	}
	if dir == "" {
		return "", fmt.Errorf("AGY 未配置工作空间，拒绝退回进程当前目录")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("无法解析当前用户主目录: %w", err)
	}
	if strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("当前用户主目录为空")
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") || strings.HasPrefix(dir, "~"+string(filepath.Separator)) {
		suffix := strings.TrimPrefix(dir, "~")
		suffix = strings.TrimPrefix(suffix, string(filepath.Separator))
		suffix = strings.TrimPrefix(suffix, "/")
		dir = filepath.Join(home, suffix)
	}
	absDir, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return "", fmt.Errorf("无法解析 AGY 工作空间 %q: %w", dir, err)
	}
	resolvedDir, err := filepath.EvalSymlinks(absDir)
	if err != nil {
		return "", fmt.Errorf("AGY 工作空间目录不存在或不可访问 %q: %w", absDir, err)
	}
	resolvedDir = filepath.Clean(resolvedDir)
	info, err := os.Stat(resolvedDir)
	if err != nil {
		return "", fmt.Errorf("无法检查 AGY 工作空间 %q: %w", resolvedDir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("AGY 工作空间不是目录: %s", resolvedDir)
	}
	if resolvedDir == string(filepath.Separator) {
		return "", fmt.Errorf("AGY 工作空间不能是文件系统根目录")
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", fmt.Errorf("无法解析用户主目录 %q: %w", home, err)
	}
	realHome, err = filepath.Abs(filepath.Clean(realHome))
	if err != nil {
		return "", fmt.Errorf("无法规范化用户主目录: %w", err)
	}
	if resolvedDir == filepath.Clean(realHome) {
		return "", fmt.Errorf("AGY 工作空间不能是整个用户主目录")
	}
	return resolvedDir, nil
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

	session := w.getSession(t.RequesterID, t.ChannelID, cwd)
	defer w.releaseSession(session)

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
	if !session.loaded && w.sessionDB != nil && w.platform != "" {
		conversationID, loadErr := w.sessionDB.LoadAGYSession(ctx, w.platform, t.RequesterID, t.ChannelID, cwd)
		if loadErr != nil {
			session.Unlock()
			return "", fmt.Errorf("读取 AGY 会话续接状态失败: %w", loadErr)
		}
		session.ConversationID = conversationID
	}
	session.loaded = true
	convID := session.ConversationID
	session.Unlock()

	executionGoal := t.Goal
	timeLimit := "调度器未设置固定时限"
	if t.ExecutionTimeout > 0 {
		timeLimit = fmt.Sprintf("调度器硬时限为 %s", t.ExecutionTimeout)
	}
	executionGoal += fmt.Sprintf("\n\n【工单阶段边界】当前%s。若完整目标无法在本次时限内可靠完成，不要一次性铺开全部工作；先完成一个可独立验证、能在本阶段收敛的部分。阶段结束时明确写出：本阶段完成项、整体目标是否仍未完成、剩余阶段、下一阶段可直接执行的具体指令，以及涉及的文件或状态。不要把阶段完成表述成整体目标完成。若目标可以在本阶段完整完成，则正常完成并说明验证结果。", timeLimit)
	executionGoal += "\n\n【执行边界】仅在当前工作区内读取和修改文件；如本工单另行授权了输入目录，只能从中读取本工单所需输入，不得在其中写入。使用 AGY Auto 模式与终端沙箱。不要尝试访问其他工作区外文件、关闭沙箱或绕过权限策略。若操作需要沙箱外权限或被拒绝，应说明受限步骤和所需权限，不要把它表述为已完成。"
	if !t.AllowSensitive {
		executionGoal += "\n\n【行为要求：当前不是核心搭档私聊。不要主动探测宿主机公网 IP，也不要在回复中输出本机公网 IP。该要求是行为指引，不代表操作系统权限隔离。】"
	}
	args := []string{
		"-p", executionGoal,
		"--output-format", "json",
		"--mode", "auto",
		"--sandbox",
	}
	for _, rawDir := range t.AdditionalDirs {
		absDir, err := filepath.Abs(filepath.Clean(strings.TrimSpace(rawDir)))
		if err != nil {
			return "", fmt.Errorf("解析工单附件目录失败: %w", err)
		}
		linkInfo, err := os.Lstat(absDir)
		if err != nil {
			return "", fmt.Errorf("检查工单附件目录 %q 失败: %w", absDir, err)
		}
		if linkInfo.Mode()&os.ModeSymlink != 0 || !linkInfo.IsDir() {
			return "", fmt.Errorf("工单附件路径 %q 必须是非符号链接目录", absDir)
		}
		resolvedDir, err := filepath.EvalSymlinks(absDir)
		if err != nil {
			return "", fmt.Errorf("解析工单附件目录 %q 失败: %w", absDir, err)
		}
		info, err := os.Stat(resolvedDir)
		if err != nil {
			return "", fmt.Errorf("检查工单附件目录 %q 失败: %w", resolvedDir, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("工单附件路径 %q 不是目录", resolvedDir)
		}
		args = append(args, "--add-dir", resolvedDir)
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

	parsed, err := parseAGYWorkerOutput(stdoutBuf.Bytes())
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
	status := strings.ToUpper(strings.TrimSpace(parsed.Status))
	if status != "SUCCESS" {
		response := strings.TrimSpace(parsed.Response)
		if response == "" {
			response = "AGY 未返回可确认完成的阶段结果。"
		}
		response = "【本阶段未完成】\n" + response
		response = appendSessionSaveWarning(response, sessionSaveErr)
		if !t.AllowSensitive {
			response = core.RedactSensitiveIPs(response)
		}
		return response, fmt.Errorf("AGY 执行状态未确认成功: %q", status)
	}

	response := strings.TrimSpace(parsed.Response)
	if response == "" {
		response = "工单已由 AGY 工作者执行完毕。"
	}
	response = appendSessionSaveWarning(response, sessionSaveErr)

	if !t.AllowSensitive {
		response = core.RedactSensitiveIPs(response)
	}
	return response, nil
}

func (w *AGYWorker) retainConversationID(session *AgySessionState, ticket *entity.Ticket, cwd string, output []byte) error {
	parsed, err := parseAGYWorkerOutput(output)
	if err != nil || strings.TrimSpace(parsed.ConversationID) == "" {
		return nil
	}
	session.Lock()
	session.ConversationID = strings.TrimSpace(parsed.ConversationID)
	session.loaded = true
	conversationID := session.ConversationID
	session.Unlock()
	return w.saveConversationID(ticket, cwd, conversationID)
}

func (w *AGYWorker) saveConversationID(ticket *entity.Ticket, cwd, conversationID string) error {
	if w.sessionDB == nil || w.platform == "" || ticket == nil || conversationID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.sessionDB.SaveAGYSession(ctx, w.platform, ticket.RequesterID, ticket.ChannelID, cwd, conversationID); err != nil {
		slog.Error("agy: save conversation ID failed", "requesterID", ticket.RequesterID, "channelID", ticket.ChannelID, "workspace", cwd, "err", err)
		return err
	}
	return nil
}

func appendSessionSaveWarning(output string, err error) string {
	if err == nil {
		return output
	}
	warning := "\n\n【续接状态未保存】当前进程内仍可继续，但服务重启后可能无法续接此 AGY 会话。"
	return strings.TrimSpace(output) + warning
}

func partialAGYOutput(stdout, stderr string, stdoutTruncated, stderrTruncated bool) string {
	var parts []string
	if stdout = strings.TrimSpace(stdout); stdout != "" {
		parts = append(parts, "标准输出：\n"+stdout)
	}
	if stderr = strings.TrimSpace(stderr); stderr != "" {
		parts = append(parts, "标准错误：\n"+stderr)
	}
	if len(parts) == 0 {
		return ""
	}
	result := strings.Join(parts, "\n\n")
	if stdoutTruncated || stderrTruncated {
		result += "\n[子进程输出已截断]"
	}
	return "【本阶段未完成，以下是子进程部分输出】\n" + core.TruncateRunes(result, 6000, "\n[阶段输出超过长度限制，后续内容已截断]")
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
