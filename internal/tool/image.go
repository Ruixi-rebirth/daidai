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
func runStatelessImageGen(ctx context.Context, command, goal, workDir string) (string, error) {
	if err := llm.CheckAGYAuth(); err != nil {
		return "", err
	}
	command = strings.TrimSpace(command)
	if command == "" {
		command = "agy"
	}

	execCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	args := []string{
		"-p", goal,
		"--output-format", "text",
		"--sandbox",
	}

	cmd := exec.CommandContext(execCtx, command, args...)
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
		return "", fmt.Errorf("%w (%s)", err, errDetail)
	}
	output := strings.TrimSpace(stdoutBuf.String())
	if stdoutBuf.Truncated() {
		output += "\n[标准输出已截断，最多保留 2 MiB]"
	}
	if runes := []rune(output); len(runes) > 8000 {
		output = string(runes[:8000]) + "... (输出摘要已截断)"
	}
	return output, nil
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
	outFile, err := os.CreateTemp(t.outDir, "gen_*.jpg")
	if err != nil {
		return nil, fmt.Errorf("创建图片临时文件失败: %w", err)
	}
	outPath := outFile.Name()
	if err := outFile.Close(); err != nil {
		_ = os.Remove(outPath)
		return nil, fmt.Errorf("关闭图片临时文件失败: %w", err)
	}
	defer func() { _ = os.Remove(outPath) }()
	absOutFile, err := filepath.Abs(outPath)
	if err != nil {
		return nil, fmt.Errorf("解析图片临时文件路径失败: %w", err)
	}

	goal := fmt.Sprintf("请调用 image-generator 子智能体生成一张图片，画面要求：%s 。生成完成后，请将生成的图片文件复制到目标路径：%s 。", prompt, absOutFile)

	slog.Info("image_tool: 触发无状态后台生成图片", "promptBytes", len(prompt))
	output, execErr := runStatelessImageGen(ctx, t.command, goal, filepath.Dir(absOutFile))
	if execErr != nil {
		slog.Warn("image_tool: 生成图片失败", "err", execErr)
		return &core.ToolResult{
			Text: fmt.Sprintf("生成图片失败：%v", execErr),
		}, nil
	}

	if data, err := readFileLimited(absOutFile, maxGeneratedImageBytes); err == nil && validImageData(data) {
		return &core.ToolResult{
			Text:      fmt.Sprintf("已生成图片：%s", prompt),
			ImageData: data,
		}, nil
	} else if err != nil && !os.IsNotExist(err) {
		return &core.ToolResult{Text: fmt.Sprintf("图片已生成，但读取失败：%v", err)}, nil
	} else if err == nil {
		return &core.ToolResult{Text: "图片生成器输出的文件不是受支持的图片格式。"}, nil
	}

	return &core.ToolResult{
		Text: fmt.Sprintf("已执行生图任务，但未找到生成的图片文件（输出：%s）", output),
	}, nil
}
