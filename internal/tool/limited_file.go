package tool

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/media"
	"daidai/internal/provider/llm"
	"golang.org/x/sys/unix"
)

const maxGeneratedImageBytes = core.MaxImageBytes

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

func isImageArtifact(_ string, data []byte) bool {
	return strings.HasPrefix(imageContentType(data), "image/")
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

func readAGYConversationArtifact(ctx context.Context, conversationID, homeOverride string, maxBytes int64, accept func(path string, data []byte) bool) ([]byte, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	conversationID = strings.TrimSpace(conversationID)
	if !isAGYConversationID(conversationID) {
		return nil, "", fmt.Errorf("无效的 AGY conversation_id")
	}
	home := strings.TrimSpace(homeOverride)
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, "", err
		}
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

func buildStatelessAGYArgs(goal string, modelEffort ...string) []string {
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
	return append(args, "--sandbox")
}

func runStatelessAGYMediaGen(ctx context.Context, command, goal, workDir, taskLabel string, router ...*llm.MindRouter) (llm.AGYResult, string, func(), error) {
	if ctx == nil {
		return llm.AGYResult{}, "", nil, fmt.Errorf("%s requires a caller context", taskLabel)
	}
	if err := llm.CheckAGYAuth(); err != nil {
		return llm.AGYResult{}, "", nil, err
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
	args := buildStatelessAGYArgs(goal, modelEffort...)

	var (
		cmd          *exec.Cmd
		artifactHome string
		cleanup      func()
		credGuard    llm.AGYCredentialGuard
		isolated     = llm.ShouldIsolateAGYCommand(command)
	)
	if isolated {
		var isoErr error
		cmd, artifactHome, cleanup, credGuard, isoErr = llm.IsolatedAGYCommandWithHome(ctx, command, args, workDir)
		if isoErr != nil {
			return llm.AGYResult{}, "", nil, isoErr
		}
	} else {
		cmd = core.CommandProcessGroup(ctx, command, args...)
		if workDir != "" {
			cmd.Dir = workDir
		}
	}
	cmd.WaitDelay = 2 * time.Second

	stdoutBuf := core.NewLimitedBuffer(2 << 20)
	stderrBuf := core.NewLimitedBuffer(2 << 20)
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	if err := cmd.Run(); err != nil {
		if !isolated && len(stdoutBuf.Bytes()) > 0 {
			if p, pErr := llm.ParseAGYResult(stdoutBuf.Bytes()); pErr == nil && p.ConversationID != "" {
				llm.CleanupEphemeralAGYConversation(p.ConversationID)
			}
		}
		if cleanup != nil {
			cleanup()
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
		if credGuard.ContainsSecret(errDetail) {
			errDetail = "输出包含敏感凭据，已安全拦截"
		}
		return llm.AGYResult{}, "", nil, fmt.Errorf("%w (%s)", err, errDetail)
	}
	cleanOnErr := func() {
		if cleanup != nil {
			cleanup()
		}
	}
	if stdoutBuf.Truncated() {
		cleanOnErr()
		return llm.AGYResult{}, "", nil, fmt.Errorf("AGY 标准输出超过 2 MiB，无法确认生成结果")
	}
	result, err := llm.ParseAGYResult(stdoutBuf.Bytes())
	if err != nil {
		cleanOnErr()
		return llm.AGYResult{}, "", nil, fmt.Errorf("解析 AGY JSON 结果失败: %w", err)
	}
	if credGuard.ContainsSecret(result.Response) {
		result.Response = ""
	}
	if detail := result.FailureDetail(); detail != "" {
		if !isolated && result.ConversationID != "" {
			llm.CleanupEphemeralAGYConversation(result.ConversationID)
		}
		cleanOnErr()
		if credGuard.ContainsSecret(detail) {
			detail = "上游返回包含敏感凭据，已安全拦截"
		}
		return llm.AGYResult{}, "", nil, fmt.Errorf("AGY %s失败: %s", taskLabel, core.TruncateRunes(detail, 1000, "…"))
	}
	if !strings.EqualFold(strings.TrimSpace(result.Status), "SUCCESS") {
		if !isolated && result.ConversationID != "" {
			llm.CleanupEphemeralAGYConversation(result.ConversationID)
		}
		cleanOnErr()
		return llm.AGYResult{}, "", nil, fmt.Errorf("AGY 未确认%s成功", taskLabel)
	}
	if result.ConversationID == "" {
		cleanOnErr()
		return llm.AGYResult{}, "", nil, fmt.Errorf("AGY 结果缺少 conversation_id")
	}
	if summary := result.DeniedActionSummary(); summary != "" {
		slog.Warn("media_gen: AGY 无头生图触发工具权限拒绝", "task", taskLabel, "conversationID", result.ConversationID, "deniedActions", summary)
	}
	if !isolated {
		convID := result.ConversationID
		cleanup = func() { llm.CleanupEphemeralAGYConversation(convID) }
	}
	output := strings.TrimSpace(result.Response)
	if runes := []rune(output); len(runes) > 8000 {
		output = string(runes[:8000]) + "... (输出摘要已截断)"
	}
	result.Response = output
	return result, artifactHome, cleanup, nil
}
