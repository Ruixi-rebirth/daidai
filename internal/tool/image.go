package tool

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
	"syscall"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
)

// runStatelessImageGen 无状态调用一次 agy 执行生图子任务（不占用工程工单槽位，不累积幽灵会话上下文）
func runStatelessImageGen(ctx context.Context, goal, workDir string) (string, error) {
	execCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	args := []string{
		"-p", goal,
		"--output-format", "text",
		"--dangerously-skip-permissions",
	}

	cmd := exec.CommandContext(execCtx, "agy", args...)
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

	if err := cmd.Run(); err != nil {
		errDetail := strings.TrimSpace(stderrBuf.String())
		if errDetail == "" {
			errDetail = strings.TrimSpace(stdoutBuf.String())
		}
		return "", fmt.Errorf("%w (%s)", err, errDetail)
	}
	return strings.TrimSpace(stdoutBuf.String()), nil
}

// ImageTool 通用真实图片与插画生成工具 (实现 core.Tool)
type ImageTool struct {
	delegates *entity.Delegates
	outDir    string
}

// NewImageTool 实例化通用生图工具
func NewImageTool(delegates *entity.Delegates) *ImageTool {
	outDir := filepath.Join("data", "workspace", "sandbox")
	_ = os.MkdirAll(outDir, 0755)
	return &ImageTool{
		delegates: delegates,
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
	if event != nil && event.Role == entity.RoleGuest {
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

	_ = os.MkdirAll(t.outDir, 0755)
	outFile := filepath.Join(t.outDir, fmt.Sprintf("gen_%d.jpg", time.Now().UnixNano()))
	absOutFile, err := filepath.Abs(outFile)
	if err != nil {
		absOutFile = outFile
	}
	defer os.Remove(absOutFile)

	goal := fmt.Sprintf("请调用 image-generator 子智能体生成一张图片，画面要求：%s 。生成完成后，请将生成的图片文件复制到目标路径：%s 。", prompt, absOutFile)

	slog.Info("image_tool: 触发无状态后台生成图片", "prompt", prompt)
	output, execErr := runStatelessImageGen(ctx, goal, filepath.Dir(absOutFile))
	if execErr != nil {
		slog.Warn("image_tool: 生成图片失败", "err", execErr)
		return &core.ToolResult{
			Text: fmt.Sprintf("生成图片失败：%v", execErr),
		}, nil
	}

	if data, err := os.ReadFile(absOutFile); err == nil && len(data) > 0 {
		return &core.ToolResult{
			Text:      fmt.Sprintf("已生成图片：%s", prompt),
			ImageData: data,
		}, nil
	}

	return &core.ToolResult{
		Text: fmt.Sprintf("已执行生图任务，但未找到生成的图片文件（输出：%s）", output),
	}, nil
}
