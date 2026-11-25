package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"daidai/internal/entity"
)

func buildAGYCommandArgs(ticket *entity.Ticket, model, effort, conversationID string) ([]string, error) {
	args := []string{
		"-p", buildAGYExecutionGoal(ticket),
		"--output-format", "stream-json",
		"--mode", "accept-edits",
		"--sandbox",
		"--dangerously-skip-permissions",
	}
	for _, rawDir := range ticket.AdditionalDirs {
		absDir, err := filepath.Abs(filepath.Clean(strings.TrimSpace(rawDir)))
		if err != nil {
			return nil, fmt.Errorf("解析工单附件目录失败: %w", err)
		}
		linkInfo, err := os.Lstat(absDir)
		if err != nil {
			return nil, fmt.Errorf("检查工单附件目录 %q 失败: %w", absDir, err)
		}
		if linkInfo.Mode()&os.ModeSymlink != 0 || !linkInfo.IsDir() {
			return nil, fmt.Errorf("工单附件路径 %q 必须是非符号链接目录", absDir)
		}
		resolvedDir, err := filepath.EvalSymlinks(absDir)
		if err != nil {
			return nil, fmt.Errorf("解析工单附件目录 %q 失败: %w", absDir, err)
		}
		info, err := os.Stat(resolvedDir)
		if err != nil {
			return nil, fmt.Errorf("检查工单附件目录 %q 失败: %w", resolvedDir, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("工单附件路径 %q 不是目录", resolvedDir)
		}
		args = append(args, "--add-dir", resolvedDir)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	if conversationID != "" {
		args = append(args, "--conversation", conversationID)
	}
	return args, nil
}
