package vision

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/media"
	"daidai/internal/provider/llm"
)

// describeWithCLI 利用本地多模态 CLI 引擎进行图像感知与文字提取
func (e *Engine) describeWithCLI(ctx context.Context, imageURLs []string, userText string) (string, []int, error) {

	cliCmd := e.cliCommand()
	if cliCmd == "" {
		return "", nil, fmt.Errorf("no available multimodal CLI found in system")
	}

	tmpWorkDir, err := os.MkdirTemp("", "daidai-vision-*")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create vision sandbox dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpWorkDir) }()

	var tmpFiles []string
	var processedIndexes []int
	for idx, rawURL := range imageURLs {
		imgBytes, err := e.fetchImage(ctx, rawURL)
		if err != nil || len(imgBytes) == 0 {
			if err != nil {
				slog.Warn("vision: 下载输入图片失败，已标记为未分析")
			}
			continue
		}
		imgBytes, _, err = media.NormalizeImageData(ctx, imgBytes)
		if err != nil {
			slog.Warn("vision: 无法解码输入图片", "err", err)
			continue
		}

		tmpFile, err := os.CreateTemp(tmpWorkDir, fmt.Sprintf("img_%d_*.png", idx))
		if err != nil {
			slog.Warn("vision: 创建图片临时文件失败", "err", err)
			continue
		}
		if _, err := tmpFile.Write(imgBytes); err != nil {
			_ = tmpFile.Close()
			slog.Warn("vision: 写入图片临时文件失败", "err", err)
			continue
		}
		if err := tmpFile.Close(); err != nil {
			slog.Warn("vision: 关闭图片临时文件失败", "err", err)
			continue
		}
		tmpFiles = append(tmpFiles, tmpFile.Name())
		processedIndexes = append(processedIndexes, idx)
	}

	if len(tmpFiles) == 0 {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		return "", nil, fmt.Errorf("no images successfully fetched for vision cli")
	}

	prompt := systemPrompt + "\n" + visionUserContext(userText, 150) + "\n请仅调用 read_file 工具读取并客观描述以下图片文件中的画面与文字，严禁读取其他任何文件或执行任何命令：\n"
	for _, p := range tmpFiles {
		prompt += p + "\n"
	}
	if cliCmd == "agy" || e.cfg.RequireAGYAuth {
		if err := llm.CheckAGYAuth(); err != nil {
			return "", nil, err
		}
	}

	args := []string{"-p", prompt, "--output-format", "text", "--mode", "plan", "--sandbox", "--dangerously-skip-permissions"}
	if e.cfg.Router != nil {
		if model, effort, err := e.cfg.Router.ResolveModelAndEffortForChannel("flash", "", "low", "AGY"); err == nil && model != "" && !strings.EqualFold(model, "flash") {
			args = append(args, "--model", model)
			if effort != "" {
				args = append(args, "--effort", effort)
			}
		}
	}

	var cmd *exec.Cmd
	var credGuard llm.AGYCredentialGuard
	if llm.ShouldIsolateAGYCommand(cliCmd) {
		var cleanup func()
		var isoErr error
		cmd, cleanup, credGuard, isoErr = llm.IsolatedAGYCommand(ctx, cliCmd, args, tmpWorkDir)
		if isoErr != nil {
			return "", nil, fmt.Errorf("vision cli sandbox unavailable: %w", isoErr)
		}
		defer cleanup()
	} else {
		cmd = core.CommandProcessGroup(ctx, cliCmd, args...)
		cmd.Dir = tmpWorkDir
		cmd.Env = os.Environ()
	}
	cmd.WaitDelay = 2 * time.Second
	stdout := core.NewLimitedBuffer(2 << 20)
	stderr := core.NewLimitedBuffer(256 << 10)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		errText := strings.TrimSpace(stderr.String())
		if runes := []rune(errText); len(runes) > 4000 {
			errText = string(runes[:4000]) + "..."
		}
		if stderr.Truncated() {
			errText += " [stderr 已截断]"
		}
		return "", nil, fmt.Errorf("vision cli execution failed (%s): %w, stderr: %s", cliCmd, err, errText)
	}
	if stdout.Truncated() {
		return "", nil, fmt.Errorf("vision cli output exceeded 2 MiB limit")
	}

	output := strings.TrimSpace(stdout.String())
	if credGuard.ContainsSecret(output) {
		return "", nil, fmt.Errorf("vision cli output blocked by credential guard")
	}
	if output == "" {
		return "", nil, fmt.Errorf("empty output from vision cli")
	}

	return output, processedIndexes, nil
}
