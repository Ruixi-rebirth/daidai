package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
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
		timeout = 90 * time.Second
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
func (p *AGYMindProvider) DiscoverModels(ctx context.Context) []ModelInfo {
	models, err := p.DiscoverModelsWithError(ctx)
	if err != nil {
		slog.Warn("agy_mind: 动态模型探测失败", "err", err)
	}
	return models
}

func (p *AGYMindProvider) DiscoverModelsWithError(ctx context.Context) ([]ModelInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.RLock()
	command := p.command
	channel := p.channelName
	p.mu.RUnlock()

	discCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(discCtx, command, "models")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second

	outBuf := core.NewLimitedBuffer(1 << 20)
	errBuf := core.NewLimitedBuffer(256 << 10)
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("run %s models: %w", command, err)
	}
	if outBuf.Truncated() {
		return nil, fmt.Errorf("%s models output exceeds 1 MiB limit", command)
	}

	models := ParseCLIModelsOutput(outBuf.String(), channel)
	if len(models) == 0 {
		return nil, fmt.Errorf("%s models returned no recognizable models", command)
	}
	p.mu.Lock()
	if p.model == "" {
		p.model = models[0].ID
	}
	if p.effort == "" && models[0].DefaultEffort != "" {
		p.effort = models[0].DefaultEffort
	}
	p.mu.Unlock()
	slog.Info("agy_mind: 成功从 CLI 动态探测可用模型列表", "command", command, "count", len(models))
	return models, nil
}

type agyUsageBucket struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	Description       string  `json:"description"`
	Window            string  `json:"window"`
	RemainingFraction float64 `json:"remaining_fraction"`
	ResetTime         string  `json:"reset_time"`
}

type agyUsageGroup struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Buckets     []agyUsageBucket `json:"buckets"`
}

type agyUsageEnvelope struct {
	Status   string `json:"status"`
	Response string `json:"response"`
	Command  struct {
		Name string `json:"name"`
		Data struct {
			Description string          `json:"description"`
			Groups      []agyUsageGroup `json:"groups"`
		} `json:"data"`
	} `json:"command"`
}

// FormatAGYUsageOutput 将 `agy -p "/usage" --output-format json` 响应解析并格式化为结构化 Markdown
func FormatAGYUsageOutput(raw []byte) (string, error) {
	var env agyUsageEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("解析 AGY 配额 JSON 失败: %w", err)
	}
	status := strings.ToUpper(strings.TrimSpace(env.Status))
	if status != "" && status != "SUCCESS" {
		detail := strings.TrimSpace(env.Response)
		if detail == "" {
			detail = status
		}
		if runes := []rune(detail); len(runes) > 4000 {
			detail = string(runes[:4000]) + "…"
		}
		return "", fmt.Errorf("AGY 配额查询返回非成功状态 %q: %s", status, detail)
	}

	groups := env.Command.Data.Groups
	if len(groups) == 0 {
		if strings.TrimSpace(env.Response) != "" {
			return strings.TrimSpace(env.Response), nil
		}
		return "", fmt.Errorf("未从 AGY 响应中提取到有效配额组数据")
	}

	var sb strings.Builder
	sb.WriteString("### AGY 实时配额与剩余使用量 (Quota Pools)\n\n")

	for i, g := range groups {
		fmt.Fprintf(&sb, "#### %d. %s\n", i+1, g.Name)
		if g.Description != "" {
			fmt.Fprintf(&sb, "- 说明：%s\n", g.Description)
		}
		for _, b := range g.Buckets {
			remPct := b.RemainingFraction * 100
			resetDesc := ""
			if b.ResetTime != "" {
				if t, err := time.Parse(time.RFC3339, b.ResetTime); err == nil {
					resetDesc = fmt.Sprintf("，刷新时间: %s", t.Local().Format("2006-01-02 15:04:05"))
				} else {
					resetDesc = fmt.Sprintf("，刷新时间: %s", b.ResetTime)
				}
			}
			hint := ""
			if b.Description != "" {
				hint = fmt.Sprintf(" (%s)", b.Description)
			}
			fmt.Fprintf(&sb, "- **%s**：剩余 **%.1f%%**%s%s\n", b.Name, remPct, resetDesc, hint)
		}
		sb.WriteString("\n")
	}

	return strings.TrimSpace(sb.String()), nil
}

// GetUsage 查询 AGY 底层当前各大配额池的实时剩余使用量与重置倒计时 (实现 UsageDiscoverer)
func (p *AGYMindProvider) GetUsage(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.RLock()
	command := p.command
	cwd := p.workingDir
	p.mu.RUnlock()

	if err := CheckAGYAuth(); err != nil {
		return "", err
	}

	usageCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(usageCtx, command, "-p", "/usage", "--output-format", "json")
	if cwd != "" {
		cmd.Dir = cwd
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cmd.Env = append(os.Environ(), "HOME="+home)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second

	outBuf := core.NewLimitedBuffer(256 << 10)
	errBuf := core.NewLimitedBuffer(256 << 10)
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("执行 %s -p /usage 失败: %w", command, err)
	}
	if outBuf.Truncated() {
		return "", fmt.Errorf("执行 %s -p /usage 失败：输出超过 256 KiB 上限", command)
	}

	return FormatAGYUsageOutput(outBuf.Bytes())
}

// ParseCLIModelsOutput 解析 `agy models` 标准输出，自动聚合模型 ID、展示名称与支持的思考强度列表
func ParseCLIModelsOutput(rawOutput, channel string) []ModelInfo {
	if channel == "" {
		channel = "AGY"
	}

	var order []string
	byID := make(map[string]*ModelInfo)

	lines := strings.Split(rawOutput, "\n")
	for _, line := range lines {
		// 清理终端 spinner 控制字符 (\r) 并截取最后一段有效文本
		if idx := strings.LastIndex(line, "\r"); idx != -1 {
			line = line[idx+1:]
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Fetching") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		rawID := fields[0]
		displayName := strings.TrimSpace(line[len(rawID):])

		baseID := rawID
		effort := ""
		openParen := strings.LastIndex(displayName, "(")
		if openParen > 0 && strings.HasSuffix(displayName, ")") {
			effortLabel := strings.TrimSpace(displayName[openParen+1 : len(displayName)-1])
			if isSupportedEffortLabel(effortLabel) {
				candidateID, candidateEffort := SplitModelAndEffort(rawID)
				if candidateEffort != "" && strings.EqualFold(candidateEffort, effortLabel) {
					baseID = candidateID
					effort = candidateEffort
					displayName = strings.TrimSpace(displayName[:openParen])
				}
			}
		}
		if baseID == "" {
			continue
		}

		if openParen := strings.LastIndex(displayName, "("); openParen > 0 && strings.HasSuffix(displayName, ")") {
			parentheticalID := strings.TrimSpace(displayName[openParen+1 : len(displayName)-1])
			if strings.EqualFold(parentheticalID, baseID) {
				displayName = strings.TrimSpace(displayName[:openParen])
			}
		}
		if displayName == "" {
			displayName = baseID
		}

		info, exists := byID[baseID]
		if !exists {
			info = &ModelInfo{
				ID:      baseID,
				Channel: channel,
				Name:    displayName,
			}
			byID[baseID] = info
			order = append(order, baseID)
		}

		if effort != "" {
			info.SupportsEffort = true
			hasEffort := false
			for _, e := range info.SupportedEfforts {
				if e == effort {
					hasEffort = true
					break
				}
			}
			if !hasEffort {
				info.SupportedEfforts = append(info.SupportedEfforts, effort)
			}
			if info.DefaultEffort == "" || effort == "medium" {
				info.DefaultEffort = effort
			}
		}
	}

	result := make([]ModelInfo, 0, len(order))
	for _, id := range order {
		result = append(result, *byID[id])
	}
	return result
}

func isSupportedEffortLabel(label string) bool {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

// SetModel 动态设置当前模型
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

type cliRawResult struct {
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Response       string `json:"response"`
	Error          string `json:"error"`
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

	if err := CheckAGYAuth(); err != nil {
		return nil, err
	}

	readOnly := opts != nil && opts.ReadOnly
	prompt := p.assemblePrompt(messages, tools, readOnly)

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"-p", prompt,
		"--output-format", "json",
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
		args = append(args, "--mode", "auto", "--sandbox")
	}

	cmd := exec.CommandContext(execCtx, command, args...)
	if readOnly {
		cmd.Dir = os.TempDir()
	} else if cwd != "" {
		cmd.Dir = cwd
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

	if err := cmd.Run(); err != nil {
		if execCtx.Err() == context.Canceled {
			return nil, fmt.Errorf("%s 任务已被中断", channelName)
		}
		if execCtx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("%s 执行超时 (%v)", channelName, timeout)
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
			return nil, fmt.Errorf("%s 执行异常: %s", channelName, errDetail)
		}
		return nil, fmt.Errorf("%s 进程执行失败: %w", channelName, err)
	}

	if stdoutBuf.Truncated() {
		return nil, fmt.Errorf("%s 输出超过 2 MiB 上限，结果不完整", channelName)
	}
	rawOut := stdoutBuf.Bytes()
	var parsed cliRawResult
	if jErr := json.Unmarshal(rawOut, &parsed); jErr != nil {
		start := bytes.IndexByte(rawOut, '{')
		end := bytes.LastIndexByte(rawOut, '}')
		if start >= 0 && end > start {
			_ = json.Unmarshal(rawOut[start:end+1], &parsed)
		}
	}

	status := strings.ToUpper(strings.TrimSpace(parsed.Status))
	if status != "" && status != "SUCCESS" {
		detail := strings.TrimSpace(parsed.Error)
		if detail == "" {
			detail = status
		}
		if runes := []rune(detail); len(runes) > 4000 {
			detail = string(runes[:4000]) + "…"
		}
		return nil, fmt.Errorf("%s 返回非成功状态 %q: %s", channelName, status, detail)
	}

	respText := strings.TrimSpace(parsed.Response)
	if respText == "" {
		return &core.ChatResponse{Content: ""}, nil
	}

	// 若提供了工具列表，尝试从响应中解析工具调用指令
	if len(tools) > 0 {
		toolCalls := p.extractToolCalls(respText)
		if len(toolCalls) > 0 {
			return &core.ChatResponse{
				ToolCalls: toolCalls,
			}, nil
		}
	}

	return &core.ChatResponse{
		Content: respText,
	}, nil
}

// assemblePrompt 将消息流与工具定义转换为纯净上下文 Prompt
func (p *AGYMindProvider) assemblePrompt(messages []core.ChatMessage, tools []core.ToolDefinition, readOnly bool) string {
	var sb strings.Builder

	var systemInstructions []string
	if readOnly {
		systemInstructions = append(systemInstructions, "【访客行为限制：当前发言人为外部访客 (RoleGuest)。仅进行日常交流，不主动读取、探测或透露宿主机代码仓库、配置文件与敏感环境信息，也不执行文件修改或命令操作。这是行为指引；具体工具权限由应用执行层控制。】")
	}
	var historyLines []string
	var toolResults []string
	var currentUserMessage string

	lastUserIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == core.RoleUser {
			lastUserIdx = i
			break
		}
	}

	for i, msg := range messages {
		switch msg.Role {
		case core.RoleSystem:
			if msg.Content != "" {
				systemInstructions = append(systemInstructions, msg.Content)
			}
		case core.RoleTool:
			toolResults = append(toolResults, fmt.Sprintf("[工具 %s 执行结果]: %s", msg.Name, msg.Content))
		case core.RoleAssistant:
			if msg.Content != "" {
				historyLines = append(historyLines, fmt.Sprintf("呆呆: %s", msg.Content))
			}
			for _, tc := range msg.ToolCalls {
				toolResults = append(toolResults, fmt.Sprintf("[已发起工具调用]: %s(%s)", tc.Function.Name, tc.Function.Arguments))
			}
		case core.RoleUser:
			if i == lastUserIdx {
				currentUserMessage = msg.Content
			} else if msg.Content != "" {
				historyLines = append(historyLines, fmt.Sprintf("用户: %s", msg.Content))
			}
		}
	}

	if len(systemInstructions) > 0 {
		sb.WriteString("=== 系统角色与运行指令 ===\n")
		sb.WriteString(strings.Join(systemInstructions, "\n\n"))
		sb.WriteString("\n\n")
	}

	if len(historyLines) > 0 {
		sb.WriteString("=== 近期会话记录 ===\n")
		sb.WriteString(strings.Join(historyLines, "\n"))
		sb.WriteString("\n\n")
	}

	var nativeTools []core.ToolDefinition
	for _, t := range tools {
		// AGY 自身已具备完整的文件、终端与联网检索能力，无需再通过 delegate_ticket 或 search_web 套娃启动第二个 AGY 进程
		if t.Function.Name == "delegate_ticket" || t.Function.Name == "search_web" {
			continue
		}
		nativeTools = append(nativeTools, t)
	}

	if len(nativeTools) > 0 {
		sb.WriteString("=== 可用宿主工具列表 ===\n")
		sb.WriteString("你可以直接完成对话、代码分析或联网检索；若根据用户意图需要使用下列宿主工具，请按协议输出调用指令：\n")
		toolBytes, _ := json.MarshalIndent(nativeTools, "", "  ")
		sb.WriteString(string(toolBytes))
		sb.WriteString("\n\n")

		sb.WriteString("=== 工具调用协议 ===\n")
		sb.WriteString("仅当需要调用上述宿主专属工具时，输出以下 JSON 代码块：\n")
		sb.WriteString("```json\n{\n  \"tool_calls\": [\n    {\n      \"name\": \"工具名称\",\n      \"arguments\": { \"参数键\": \"参数值\" }\n    }\n  ]\n}\n```\n")
		sb.WriteString("若无需调用上述宿主专属工具，请直接用你自身能力处理并输出自然亲切的中文回复，不要输出 JSON 代码块。\n\n")
	}

	sb.WriteString("=== 用户当前发言 ===\n")
	sb.WriteString(currentUserMessage)

	if len(toolResults) > 0 {
		sb.WriteString("\n\n=== 本轮已执行的工具与反馈 ===\n")
		sb.WriteString(strings.Join(toolResults, "\n\n"))
		sb.WriteString("\n\n以上是刚才调用工具得到的真实结果。若这些结果已足以回应用户，请直接输出自然中文回复，不要重复调用相同的工具。")
	}

	return sb.String()
}

// extractToolCalls 从模型文本中解析结构化工具调用
func (p *AGYMindProvider) extractToolCalls(text string) []core.ToolCall {
	var jsonBlocks []string

	lines := strings.Split(text, "\n")
	inCodeBlock := false
	var block strings.Builder

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inCodeBlock {
				inCodeBlock = false
				jsonBlocks = append(jsonBlocks, block.String())
				block.Reset()
			} else {
				if strings.Contains(trimmed, "json") || trimmed == "```" {
					inCodeBlock = true
				}
			}
			continue
		}
		if inCodeBlock {
			block.WriteString(line)
			block.WriteString("\n")
		}
	}

	// 若未在代码块中，尝试扫描裸 JSON 对象（兼容换行与缩进格式）
	if len(jsonBlocks) == 0 {
		if strings.Contains(text, `"tool_calls"`) {
			// 解析器允许 JSON 前后带说明文字。逐个尝试对象起点，避免嵌套字段
			// 恰好出现在 tool_calls 前时，从内层左花括号开始截取而解析失败。
			for start := strings.IndexByte(text, '{'); start >= 0; {
				var payload cliToolCallPayload
				decoder := json.NewDecoder(strings.NewReader(text[start:]))
				if err := decoder.Decode(&payload); err == nil && len(payload.ToolCalls) > 0 {
					jsonBlocks = append(jsonBlocks, text[start:start+int(decoder.InputOffset())])
					break
				}
				next := strings.IndexByte(text[start+1:], '{')
				if next < 0 {
					break
				}
				start += next + 1
			}
		}
	}

	nowNano := time.Now().UnixNano()
	var result []core.ToolCall

	for _, jStr := range jsonBlocks {
		var payload cliToolCallPayload
		if err := json.Unmarshal([]byte(strings.TrimSpace(jStr)), &payload); err == nil && len(payload.ToolCalls) > 0 {
			for i, tc := range payload.ToolCalls {
				argStr := string(tc.Arguments)
				if argStr == "" || argStr == "null" {
					argStr = "{}"
				}
				result = append(result, core.ToolCall{
					ID:   fmt.Sprintf("call_cli_%d_%d", nowNano, i),
					Type: "function",
					Function: core.ToolCallFunction{
						Name:      tc.Name,
						Arguments: argStr,
					},
				})
			}
			break
		}
	}

	return result
}
