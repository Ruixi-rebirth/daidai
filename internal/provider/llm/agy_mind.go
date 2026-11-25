package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"daidai/internal/core"
)

// AGYMindConfig 外部命令行自主智能体心智配置
type AGYMindConfig struct {
	Command     string // 二进制命令名或路径，默认 "agy"
	ChannelName string // 通道名，默认 "AGY"
	Model       string
	Effort      string
	WorkingDir  string
	Timeout     time.Duration
}

// AGYMindProvider 将命令行自主智能体驱动统一封装为 core.LLMProvider
type AGYMindProvider struct {
	mu          sync.RWMutex
	command     string
	channelName string
	model       string
	effort      string
	workingDir  string
	timeout     time.Duration
}

// NewAGYMindProvider 实例化 AGY 驱动
func NewAGYMindProvider(cfg AGYMindConfig) *AGYMindProvider {
	cmd := strings.TrimSpace(cfg.Command)
	if cmd == "" {
		cmd = "agy"
	}
	ch := strings.TrimSpace(cfg.ChannelName)
	if ch == "" {
		ch = "AGY"
	}
	model := strings.TrimSpace(cfg.Model)
	effort := strings.TrimSpace(cfg.Effort)
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 4 * time.Minute
	}
	cwd := strings.TrimSpace(cfg.WorkingDir)
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	return &AGYMindProvider{
		command:     cmd,
		channelName: ch,
		model:       model,
		effort:      effort,
		workingDir:  cwd,
		timeout:     timeout,
	}
}

// DiscoverModels 通过执行 `<command> models` 子命令实时探测当前 CLI 支持的模型池与思考强度
func (p *AGYMindProvider) SetModel(model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if model != "" {
		p.model = strings.TrimSpace(model)
	}
}

// SetEffort 动态设置当前思考强度（允许传空字符串以清除不支持思考强度模型的残留配置）
func (p *AGYMindProvider) SetEffort(effort string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.effort = strings.TrimSpace(effort)
}

// GetModelAndEffort 获取当前模型与思考强度
func (p *AGYMindProvider) GetModelAndEffort() (string, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.model, p.effort
}

func (p *AGYMindProvider) Name() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	ch := p.channelName
	if ch == "" {
		ch = p.command
	}
	if p.effort != "" {
		return fmt.Sprintf("%s (%s, effort: %s)", ch, p.model, p.effort)
	}
	return fmt.Sprintf("%s (%s)", ch, p.model)
}

// Chat 纯文本会话推理
func (p *AGYMindProvider) Chat(ctx context.Context, messages []core.ChatMessage, opts *core.ChatOptions) (string, error) {
	resp, err := p.ChatWithTools(ctx, messages, nil, opts)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

type cliToolCallPayload struct {
	ToolCalls []struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"tool_calls"`
}

// ChatWithTools 带工具定义的综合推理执行
func (p *AGYMindProvider) ChatWithTools(
	ctx context.Context,
	messages []core.ChatMessage,
	tools []core.ToolDefinition,
	opts *core.ChatOptions,
) (*core.ChatResponse, error) {
	p.mu.RLock()
	command := p.command
	channelName := p.channelName
	currentModel := p.model
	currentEffort := p.effort
	cwd := p.workingDir
	timeout := p.timeout
	p.mu.RUnlock()

	if opts != nil && strings.TrimSpace(opts.Model) != "" {
		currentModel = strings.TrimSpace(opts.Model)
		currentEffort = strings.TrimSpace(opts.Effort)
	}
	readOnly := opts != nil && (opts.ReadOnly || opts.ReasoningOnly)
	if opts != nil && opts.ReadOnly {
		switch strings.ToLower(currentEffort) {
		case "high", "xhigh", "max":
			currentEffort = "medium"
		}
	}
	if err := CheckAGYAuth(); err != nil {
		return nil, err
	}

	uncertainFailure := func(err error) error {
		if readOnly {
			return err
		}
		return &core.UncertainExecutionError{Err: err}
	}
	hostTools := agyHostTools(tools)
	prompt := p.assemblePrompt(messages, hostTools, readOnly)

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"-p", prompt,
		"--output-format", "json",
		"--dangerously-skip-permissions",
	}
	if currentModel != "" {
		args = append(args, "--model", currentModel)
	}
	if currentEffort != "" {
		args = append(args, "--effort", currentEffort)
	}
	if readOnly {
		args = append(args, "--mode", "plan", "--sandbox")
	} else {
		args = append(args, "--mode", "accept-edits", "--sandbox")
	}

	var cmd *exec.Cmd
	var credGuard AGYCredentialGuard
	if readOnly {
		var cleanup func()
		var isoErr error
		cmd, cleanup, credGuard, isoErr = IsolatedAGYCommand(execCtx, command, args, "")
		if isoErr != nil {
			return nil, isoErr
		}
		defer cleanup()
	} else {
		cmd = core.CommandProcessGroup(execCtx, command, args...)
		if cwd != "" {
			cmd.Dir = cwd
		}
	}
	cmd.WaitDelay = 2 * time.Second

	stdoutBuf := core.NewLimitedBuffer(2 << 20)
	stderrBuf := core.NewLimitedBuffer(2 << 20)
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	if err := cmd.Run(); err != nil {
		if isCommandStartError(err) {
			return nil, fmt.Errorf("%s 进程未能启动: %w", channelName, err)
		}
		if execCtx.Err() == context.Canceled {
			return nil, uncertainFailure(fmt.Errorf("%s 任务已被中断", channelName))
		}
		if execCtx.Err() == context.DeadlineExceeded {
			return nil, uncertainFailure(fmt.Errorf("%s 执行超时 (%v)", channelName, timeout))
		}

		errDetail := strings.TrimSpace(stderrBuf.String())
		if errDetail == "" {
			errDetail = strings.TrimSpace(stdoutBuf.String())
		}
		if errDetail != "" {
			if runes := []rune(errDetail); len(runes) > 8000 {
				errDetail = string(runes[:8000]) + "..."
			}
			if stdoutBuf.Truncated() || stderrBuf.Truncated() {
				errDetail += " (子进程输出已截断)"
			}
			return nil, uncertainFailure(fmt.Errorf("%s 执行异常: %s", channelName, errDetail))
		}
		return nil, uncertainFailure(fmt.Errorf("%s 进程执行失败: %w", channelName, err))
	}

	if stdoutBuf.Truncated() {
		return nil, uncertainFailure(fmt.Errorf("%s 输出超过 2 MiB 上限，结果不完整", channelName))
	}
	if readOnly && credGuard.ContainsSecret(stdoutBuf.String()) {
		return nil, fmt.Errorf("%s 响应触发凭据保护熔断", channelName)
	}
	parsed, err := ParseAGYResult(stdoutBuf.Bytes())
	if err != nil {
		return nil, uncertainFailure(fmt.Errorf("%s: %w", channelName, err))
	}
	response, err := p.chatResponse(parsed, channelName, messages, hostTools)
	if err != nil {
		return nil, uncertainFailure(err)
	}
	if readOnly && credGuard.ContainsSecret(response.Content) {
		return nil, fmt.Errorf("%s 响应触发凭据保护熔断", channelName)
	}
	if isEmptyChatResponse(response) {
		return nil, uncertainFailure(fmt.Errorf("%s 返回空响应", channelName))
	}
	return response, nil
}

func isCommandStartError(err error) bool {
	var lookupErr *exec.Error
	var startErr *os.PathError
	return errors.As(err, &lookupErr) || errors.As(err, &startErr)
}

func (p *AGYMindProvider) chatResponse(parsed AGYResult, channelName string, messages []core.ChatMessage, hostTools []core.ToolDefinition) (*core.ChatResponse, error) {
	if detail := parsed.FailureDetail(); detail != "" {
		if runes := []rune(detail); len(runes) > 4000 {
			detail = string(runes[:4000]) + "…"
		}
		return nil, fmt.Errorf("%s 返回执行错误: %s", channelName, detail)
	}
	if !strings.EqualFold(strings.TrimSpace(parsed.Status), "SUCCESS") {
		status := strings.TrimSpace(parsed.Status)
		if status == "" {
			status = "缺少 status"
		}
		return nil, fmt.Errorf("%s 未返回明确的成功状态（%s）", channelName, status)
	}

	response := &core.ChatResponse{
		Content:      stripAGYToolEcho(strings.TrimSpace(parsed.Response), messages),
		FinishReason: strings.TrimSpace(parsed.FinishedReason),
	}

	// 若提供了工具列表，尝试从响应中解析工具调用指令
	if len(hostTools) > 0 && response.Content != "" {
		toolCalls := p.extractToolCalls(response.Content)
		if len(toolCalls) > 0 {
			allowed := make(map[string]struct{}, len(hostTools))
			for _, tool := range hostTools {
				allowed[tool.Function.Name] = struct{}{}
			}
			for _, call := range toolCalls {
				if _, ok := allowed[call.Function.Name]; !ok {
					return nil, fmt.Errorf("%s 返回了当前未开放的应用工具 %q", channelName, core.TruncateRunes(call.Function.Name, 80, "…"))
				}
			}
			response.Content = ""
			response.ToolCalls = toolCalls
		}
	}

	return response, nil
}
