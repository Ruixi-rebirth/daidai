package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"daidai/internal/provider/llm"
)

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
