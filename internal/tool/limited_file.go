package tool

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/media"
	"daidai/internal/provider/llm"
	"golang.org/x/sys/unix"
)

const (
	maxGeneratedImageBytes = core.MaxImageBytes
	maxGeneratedVideoBytes = media.MaxRawVideoInputBytes
)

func normalizeImageData(ctx context.Context, data []byte) ([]byte, string, error) {
	return media.NormalizeImageData(ctx, data)
}

func imageContentType(data []byte) string {
	return media.ContentType(data)
}

func readFileLimited(path string, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("maximum file size cannot be negative")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("failed to open file descriptor for %q", path)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing to read non-regular file %q", path)
	}

	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file exceeded %d byte limit", maxBytes)
	}
	return data, nil
}

// readGeneratedImage prefers the requested output path, then recovers a raster
// image the generator may have saved under a different name in its isolated workspace.
func readGeneratedImage(ctx context.Context, outputPath, workspace string, excludedPaths ...string) ([]byte, string, error) {
	return readGeneratedArtifact(ctx, outputPath, workspace, maxGeneratedImageBytes, isImageArtifact, excludedPaths...)
}

func readGeneratedVideo(ctx context.Context, outputPath, workspace string, excludedPaths ...string) ([]byte, string, error) {
	return readGeneratedArtifact(ctx, outputPath, workspace, maxGeneratedVideoBytes, isVideoArtifact, excludedPaths...)
}

func isImageArtifact(_ string, data []byte) bool {
	return strings.HasPrefix(imageContentType(data), "image/")
}

func isVideoArtifact(path string, data []byte) bool {
	if len(data) == 0 {
		return false
	}
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp4", ".m4v", ".mov", ".webm", ".mkv", ".avi", ".gif":
		return true
	}
	detected := http.DetectContentType(data)
	return strings.HasPrefix(detected, "video/") || detected == "image/gif"
}

func readGeneratedArtifact(
	ctx context.Context,
	outputPath, workspace string,
	maxBytes int64,
	accept func(path string, data []byte) bool,
	excludedPaths ...string,
) ([]byte, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if data, err := readFileLimited(outputPath, maxBytes); err == nil {
		return data, outputPath, nil
	} else if !os.IsNotExist(err) {
		return nil, "", err
	}

	excluded := make(map[string]struct{}, len(excludedPaths)+1)
	for _, path := range append(excludedPaths, outputPath) {
		absPath, err := filepath.Abs(path)
		if err == nil {
			excluded[absPath] = struct{}{}
		}
	}

	var candidates []string
	err := filepath.WalkDir(workspace, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != workspace && strings.Count(strings.TrimPrefix(path, workspace), string(filepath.Separator)) >= 2 {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		absPath, err := filepath.Abs(path)
		if err != nil {
			return nil
		}
		if _, skip := excluded[absPath]; skip {
			return nil
		}
		candidates = append(candidates, path)
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	for _, path := range candidates {
		data, err := readFileLimited(path, maxBytes)
		if err != nil || (accept != nil && !accept(path, data)) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		return data, path, nil
	}
	return nil, "", os.ErrNotExist
}

func readAGYConversationArtifact(ctx context.Context, conversationID string, maxBytes int64, accept func(path string, data []byte) bool) ([]byte, string, error) {
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
	dirs := []string{root}
	scratchDir := filepath.Join(root, "scratch")
	if info, err := os.Lstat(scratchDir); err == nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir() {
		dirs = append(dirs, scratchDir)
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if dir == root {
				return nil, "", err
			}
			continue
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, "", err
			}
			if !entry.Type().IsRegular() || entry.Name() == "" {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			data, err := readFileLimited(path, maxBytes)
			if err == nil && (accept == nil || accept(path, data)) {
				return data, path, nil
			}
		}
	}
	return nil, "", os.ErrNotExist
}

func buildStatelessAGYArgs(goal string, allowShellTools bool, modelEffort ...string) []string {
	args := []string{
		"-p", goal,
		"--output-format", "json",
	}
	if len(modelEffort) > 0 && strings.TrimSpace(modelEffort[0]) != "" {
		args = append(args, "--model", strings.TrimSpace(modelEffort[0]))
	}
	if len(modelEffort) > 1 && strings.TrimSpace(modelEffort[1]) != "" {
		args = append(args, "--effort", strings.TrimSpace(modelEffort[1]))
	}
	if allowShellTools {
		args = append(args, "--mode", "auto", "--sandbox", "--dangerously-skip-permissions")
	} else {
		args = append(args, "--sandbox")
	}
	return args
}

func runStatelessAGYMediaGen(ctx context.Context, command, goal, workDir, taskLabel string, allowShellTools bool, router ...*llm.MindRouter) (llm.AGYResult, error) {
	if ctx == nil {
		return llm.AGYResult{}, fmt.Errorf("%s requires a caller context", taskLabel)
	}
	if err := llm.CheckAGYAuth(); err != nil {
		return llm.AGYResult{}, err
	}
	command = strings.TrimSpace(command)
	if command == "" {
		command = "agy"
	}

	var modelEffort []string
	if len(router) > 0 && router[0] != nil {
		if model, effort, err := router[0].ResolveModelAndEffortForChannel("flash", "", "low", "AGY"); err == nil && model != "" && !strings.EqualFold(model, "flash") {
			modelEffort = []string{model, effort}
		}
	}
	args := buildStatelessAGYArgs(goal, allowShellTools, modelEffort...)

	cmd := core.CommandProcessGroup(ctx, command, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	cmd.WaitDelay = 2 * time.Second

	stdoutBuf := core.NewLimitedBuffer(2 << 20)
	stderrBuf := core.NewLimitedBuffer(2 << 20)
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	if err := cmd.Run(); err != nil {
		if len(stdoutBuf.Bytes()) > 0 {
			if p, pErr := llm.ParseAGYResult(stdoutBuf.Bytes()); pErr == nil && p.ConversationID != "" {
				llm.CleanupEphemeralAGYConversation(p.ConversationID)
			}
		}
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
		return llm.AGYResult{}, fmt.Errorf("%w (%s)", err, errDetail)
	}
	if stdoutBuf.Truncated() {
		return llm.AGYResult{}, fmt.Errorf("AGY 标准输出超过 2 MiB，无法确认生成结果")
	}
	result, err := llm.ParseAGYResult(stdoutBuf.Bytes())
	if err != nil {
		return llm.AGYResult{}, fmt.Errorf("解析 AGY JSON 结果失败: %w", err)
	}
	if detail := result.FailureDetail(); detail != "" {
		return result, fmt.Errorf("AGY %s失败: %s", taskLabel, core.TruncateRunes(detail, 1000, "…"))
	}
	if !strings.EqualFold(strings.TrimSpace(result.Status), "SUCCESS") {
		return result, fmt.Errorf("AGY 未确认%s成功", taskLabel)
	}
	if result.ConversationID == "" {
		return result, fmt.Errorf("AGY 结果缺少 conversation_id")
	}
	output := strings.TrimSpace(result.Response)
	if runes := []rune(output); len(runes) > 8000 {
		output = string(runes[:8000]) + "... (输出摘要已截断)"
	}
	result.Response = output
	return result, nil
}
