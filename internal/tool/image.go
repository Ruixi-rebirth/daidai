package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/provider/llm"
)

// runStatelessImageGen 无状态调用一次 agy 执行生图子任务（不占用工程工单槽位，不累积幽灵会话上下文）
type agyRunResult struct {
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Response       string `json:"response"`
	Error          string `json:"error"`
}

func runStatelessImageGen(ctx context.Context, command, goal, workDir string) (agyRunResult, error) {
	if ctx == nil {
		return agyRunResult{}, fmt.Errorf("image generation requires a caller context")
	}
	if err := llm.CheckAGYAuth(); err != nil {
		return agyRunResult{}, err
	}
	command = strings.TrimSpace(command)
	if command == "" {
		command = "agy"
	}

	args := []string{
		"-p", goal,
		"--output-format", "json",
		"--sandbox",
	}

	cmd := exec.CommandContext(ctx, command, args...)
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

	if err := cmd.Run(); err != nil {
		errDetail := strings.TrimSpace(stderrBuf.String())
		if errDetail == "" {
			errDetail = strings.TrimSpace(stdoutBuf.String())
		}
		if runes := []rune(errDetail); len(runes) > 8000 {
			errDetail = string(runes[:8000]) + "..."
		}
		if stdoutBuf.Truncated() || stderrBuf.Truncated() {
			errDetail += " (子进程输出已截断)"
		}
		return agyRunResult{}, fmt.Errorf("%w (%s)", err, errDetail)
	}
	var result agyRunResult
	if err := json.Unmarshal(stdoutBuf.Bytes(), &result); err != nil {
		return agyRunResult{}, fmt.Errorf("解析 AGY JSON 结果失败: %w", err)
	}
	if result.Status != "SUCCESS" {
		if strings.TrimSpace(result.Error) == "" {
			result.Error = result.Response
		}
		return result, fmt.Errorf("AGY 任务状态为 %q: %s", result.Status, strings.TrimSpace(result.Error))
	}
	if result.ConversationID == "" {
		return result, fmt.Errorf("AGY 结果缺少 conversation_id")
	}
	output := strings.TrimSpace(result.Response)
	if stdoutBuf.Truncated() {
		output += "\n[标准输出已截断，最多保留 2 MiB]"
	}
	if runes := []rune(output); len(runes) > 8000 {
		output = string(runes[:8000]) + "... (输出摘要已截断)"
	}
	result.Response = output
	return result, nil
}

// ImageTool 通用真实图片与插画生成工具 (实现 core.Tool)
type ImageTool struct {
	delegates *entity.Delegates
	immune    *entity.ImmuneSystem
	command   string
	outDir    string
}

// NewImageTool 实例化通用生图工具
func NewImageTool(delegates *entity.Delegates, immune ...*entity.ImmuneSystem) *ImageTool {
	return NewImageToolWithCommand(delegates, "agy", filepath.Join("data", "workspace", "sandbox"), immune...)
}

func NewImageToolWithCommand(delegates *entity.Delegates, command, outDir string, immune ...*entity.ImmuneSystem) *ImageTool {
	command = strings.TrimSpace(command)
	if command == "" {
		command = "agy"
	}
	if strings.TrimSpace(outDir) == "" {
		outDir = filepath.Join("data", "workspace", "sandbox")
	}
	_ = os.MkdirAll(outDir, 0755)
	var immuneSystem *entity.ImmuneSystem
	if len(immune) > 0 {
		immuneSystem = immune[0]
	}
	return &ImageTool{
		delegates: delegates,
		immune:    immuneSystem,
		command:   command,
		outDir:    outDir,
	}
}

func (t *ImageTool) Name() string {
	return "generate_image"
}

func (t *ImageTool) Description() string {
	return "根据画面描述绘制通用图片或插画（用于绘制呆呆本人以外的其他事物、动物或场景，如画一只猫、画风景等）。"
}

func (t *ImageTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt": map[string]any{
				"type":        "string",
				"description": "需要生成的画面内容与风格描述",
			},
		},
		"required": []string{"prompt"},
	}
}

type imageArgs struct {
	Prompt string `json:"prompt"`
}

func (t *ImageTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	if t.delegates == nil {
		return nil, fmt.Errorf("delegates subsystem not configured")
	}

	event := entity.SensoryEventFromContext(ctx)
	if event == nil {
		if err := llm.CheckAGYAuth(); err != nil {
			return &core.ToolResult{Text: fmt.Sprintf("生成图片失败：%v", err)}, nil
		}
		return &core.ToolResult{Text: "无法校验当前会话权限，拒绝调用生图引擎。"}, nil
	}
	if t.immune == nil {
		return &core.ToolResult{Text: "权限校验未配置，拒绝调用生图引擎。"}, nil
	}
	role := t.immune.ResolveRole(event.SenderID)
	if err := t.immune.VerifyDelegationPerm(role); err != nil {
		return &core.ToolResult{
			Text: "当前为访客权限，不支持调用后台生图引擎。",
		}, nil
	}

	var args imageArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, fmt.Errorf("invalid generate_image args: %w", err)
	}

	prompt := strings.TrimSpace(args.Prompt)
	if prompt == "" {
		return nil, fmt.Errorf("image prompt cannot be empty")
	}

	if event != nil && event.OnProgress != nil {
		event.OnProgress("稍等，正在画这张图~")
	}

	releaseWorkspace, err := t.delegates.AcquireWorkspaceLease(t.outDir)
	if err != nil {
		return &core.ToolResult{Text: fmt.Sprintf("生成图片失败：%v", err)}, nil
	}
	defer releaseWorkspace()

	if err := os.MkdirAll(t.outDir, 0755); err != nil {
		return nil, fmt.Errorf("创建图片输出目录失败: %w", err)
	}
	workDir, err := os.MkdirTemp(t.outDir, ".imagegen-*")
	if err != nil {
		return nil, fmt.Errorf("创建图片临时工作目录失败: %w", err)
	}
	if err := os.Chmod(workDir, 0700); err != nil {
		_ = os.RemoveAll(workDir)
		return nil, fmt.Errorf("保护图片临时工作目录失败: %w", err)
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	outFile, err := os.CreateTemp(workDir, "gen_*.png")
	if err != nil {
		return nil, fmt.Errorf("创建图片临时文件失败: %w", err)
	}
	outPath := outFile.Name()
	if err := outFile.Close(); err != nil {
		_ = os.Remove(outPath)
		return nil, fmt.Errorf("关闭图片临时文件失败: %w", err)
	}
	defer func() { _ = os.Remove(outPath) }()
	// Keep the randomized target name, but remove the empty placeholder. If the
	// generator fails to create the image, the later read must report a missing
	// file instead of misclassifying this empty file as an unsupported format.
	if err := os.Remove(outPath); err != nil {
		return nil, fmt.Errorf("准备图片输出路径失败: %w", err)
	}
	absOutFile, err := filepath.Abs(outPath)
	if err != nil {
		return nil, fmt.Errorf("解析图片临时文件路径失败: %w", err)
	}

	goal := fmt.Sprintf("请调用 image-generator 子智能体生成一张图片，画面要求：%s。将最终文件保存到目标路径：%s；必须确认该文件实际存在且非空后才能结束任务，文件内容必须是实际 PNG 格式（不能只修改扩展名）。", prompt, absOutFile)

	slog.Info("image_tool: 触发无状态后台生成图片", "promptBytes", len(prompt))
	agyResult, execErr := runStatelessImageGen(ctx, t.command, goal, workDir)
	if execErr != nil {
		slog.Warn("image_tool: 生成图片失败", "err", execErr)
		return &core.ToolResult{
			Text: fmt.Sprintf("生成图片失败：%v", execErr),
		}, nil
	}

	if data, generatedPath, err := readGeneratedImage(ctx, absOutFile, workDir); err == nil {
		if generatedPath != absOutFile {
			slog.Info("image_tool: 从生成器实际输出文件中恢复图片", "file", filepath.Base(generatedPath))
		}
		normalized, format, normalizeErr := normalizeImageData(ctx, data)
		if normalizeErr != nil {
			slog.Warn("image_tool: 生成结果无法解码或转换", "contentType", imageContentType(data), "format", format, "bytes", len(data), "err", normalizeErr)
			return &core.ToolResult{Text: fmt.Sprintf("生图任务写出了文件，但无法识别或转换为可发送图片（检测类型：%s）：%v", imageContentType(data), normalizeErr)}, nil
		}
		return &core.ToolResult{
			Text:      fmt.Sprintf("已生成图片：%s", prompt),
			ImageData: normalized,
		}, nil
	} else if err != nil && !os.IsNotExist(err) {
		slog.Warn("image_tool: 生成结果读取失败", "err", err)
		return &core.ToolResult{Text: "图片生成任务已执行，但读取生成结果失败；请检查服务日志。"}, nil
	}

	// AGY's image-generator agent may save the finished image into its own
	// conversation artifact directory. The JSON result identifies this exact
	// AGY conversation, so recovery never scans artifacts from another run.
	if data, generatedPath, err := readAGYImageArtifact(ctx, agyResult.ConversationID); err == nil {
		slog.Info("image_tool: 从本次 AGY 会话产物中恢复图片", "conversationID", agyResult.ConversationID, "file", filepath.Base(generatedPath))
		normalized, format, normalizeErr := normalizeImageData(ctx, data)
		if normalizeErr != nil {
			slog.Warn("image_tool: AGY 会话产物无法解码或转换", "contentType", imageContentType(data), "format", format, "bytes", len(data), "err", normalizeErr)
			return &core.ToolResult{Text: fmt.Sprintf("生图任务写出了文件，但无法识别或转换为可发送图片（检测类型：%s）：%v", imageContentType(data), normalizeErr)}, nil
		}
		return &core.ToolResult{Text: fmt.Sprintf("已生成图片：%s", prompt), ImageData: normalized}, nil
	} else if err != nil && !os.IsNotExist(err) {
		slog.Warn("image_tool: AGY 会话产物扫描失败", "err", err)
	}

	slog.Warn("image_tool: 生图任务未找到图片", "conversationID", agyResult.ConversationID, "outputBytes", len(agyResult.Response))
	return &core.ToolResult{
		Text: "生成任务已结束，但没有找到图片文件；未确认失败原因，不要推测为超时或上游服务故障。",
	}, nil
}

func readAGYImageArtifact(ctx context.Context, conversationID string) ([]byte, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	conversationID = strings.TrimSpace(conversationID)
	if !isAGYConversationID(conversationID) {
		return nil, "", fmt.Errorf("无效的 AGY conversation_id")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, "", err
	}
	root := filepath.Join(home, ".gemini", "antigravity-cli", "brain", conversationID)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, "", err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		if !entry.Type().IsRegular() || entry.Name() == "" {
			continue
		}
		path := filepath.Join(root, entry.Name())
		data, err := readFileLimited(path, maxGeneratedImageBytes)
		if err == nil && strings.HasPrefix(imageContentType(data), "image/") {
			return data, path, nil
		}
	}
	return nil, "", os.ErrNotExist
}

func isAGYConversationID(value string) bool {
	parts := strings.Split(value, "-")
	lengths := [...]int{8, 4, 4, 4, 12}
	if len(parts) != len(lengths) {
		return false
	}
	for i, part := range parts {
		if len(part) != lengths[i] {
			return false
		}
		for _, char := range part {
			if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
				return false
			}
		}
	}
	return true
}
