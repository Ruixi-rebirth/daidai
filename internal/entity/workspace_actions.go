package entity

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (wm *WorkspaceManager) ExecuteAction(platform, channelID string, role UserRole, immune *ImmuneSystem, action, target string, delegates ...*Delegates) string {
	if immune == nil {
		return "权限校验未配置，拒绝执行工作空间操作"
	}
	if err := immune.VerifyDelegationPerm(role); err != nil {
		return err.Error()
	}

	action = strings.ToLower(strings.TrimSpace(action))
	target = strings.TrimSpace(target)
	if action == "" {
		if target != "" {
			action = "switch"
		} else {
			action = "current"
		}
	}

	switch action {
	case "current":
		return wm.FormatCurrent(platform, channelID)

	case "list":
		return wm.FormatList(platform, channelID)

	case "exit":
		newPath, err := wm.Switch(platform, channelID, "sandbox")
		if err != nil {
			return fmt.Sprintf("退出工程失败：%v", err)
		}
		return fmt.Sprintf("### 已退出生产工程\n\n已切回默认工作区：`sandbox` (`%s`)\n\n> 后续工程任务默认以此目录为工作目录。它不是操作系统级隔离环境，工作者仍可能访问目录外资源。\n> 随时可通过 `/cd <项目名>` 重新切入生产工程。", newPath)

	case "clean":
		var err error
		if len(delegates) > 0 && delegates[0] != nil {
			err = delegates[0].runIfWorkspaceIdle(wm.defaultDir, wm.CleanSandbox)
		} else {
			err = wm.CleanSandbox()
		}
		if err != nil {
			return fmt.Sprintf("清理默认工作区失败：%v", err)
		}
		return fmt.Sprintf("### 默认工作区已清理\n\n默认工作区 (`%s`) 中的文件已清理。", wm.SandboxDir())

	case "remove":
		if target == "" {
			return "请指定要注销的目标工作空间路径或工程名"
		}
		if err := wm.Remove(platform, channelID, target); err != nil {
			return fmt.Sprintf("注销工作空间失败：%v", err)
		}
		return fmt.Sprintf("### 工作空间书签已注销\n\n已成功注销书签：`%s`\n\n> 提示：仅从书签索引中解绑，磁盘上的物理代码文件完好保留。", target)

	case "switch":
		if target == "" {
			return "请指定要切换的目标工作空间路径或工程名"
		}
		newPath, err := wm.Switch(platform, channelID, target)
		if err != nil {
			return fmt.Sprintf("切换工作空间失败：%v", err)
		}
		if wm.IsSandbox(newPath) {
			return fmt.Sprintf("### 已切回默认工作区\n\n- **运行模式**：`默认工作区` (临时草稿区)\n- **物理路径**：`%s`\n\n> 工程工作者默认以此目录为工作目录；该目录不构成操作系统级隔离，工作者仍可能访问目录外资源。", newPath)
		}
		projectName := filepath.Base(newPath)
		return fmt.Sprintf("### 已切入生产工程\n\n- **工程名称**：`%s`\n- **物理路径**：`%s`\n- **运行模式**：`生产工程态`\n\n> 工程任务会以该目录为工作目录；工作者仍可能访问目录外资源。\n> 随时可通过 `/ws exit` 退出并切回默认工作区。", projectName, newPath)

	default:
		return fmt.Sprintf("未知操作 %q，支持的操作为 current, list, switch, remove, clean, exit", action)
	}
}

// IsSandbox 判断指定路径是否为当前配置的默认工作区
func (wm *WorkspaceManager) IsSandbox(path string) bool {
	if path == "" {
		return false
	}
	clean := filepath.Clean(path)
	defaultDir := filepath.Clean(wm.defaultDir)
	if real, err := filepath.EvalSymlinks(clean); err == nil {
		clean = real
	}
	if real, err := filepath.EvalSymlinks(defaultDir); err == nil {
		defaultDir = real
	}
	return clean == defaultDir
}

// SandboxDir 获取沙盒目录绝对路径
func (wm *WorkspaceManager) SandboxDir() string {
	return wm.defaultDir
}

// CleanSandbox 清空专用默认工作区目录中的所有临时文件 (保留 .gitignore)
func (wm *WorkspaceManager) CleanSandbox() error {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	sandbox := wm.defaultDir
	if sandbox == "" {
		return fmt.Errorf("默认工作区未初始化")
	}

	cleanSandbox, err := canonicalWorkspacePath(sandbox)
	if err != nil {
		return fmt.Errorf("安全拒绝：禁止清空主目录或根目录 (%s)", sandbox)
	}
	if cleanSandbox != filepath.Clean(sandbox) {
		return fmt.Errorf("安全拒绝：默认工作区路径已被符号链接重定向 (%s -> %s)", sandbox, cleanSandbox)
	}
	info, err := os.Lstat(sandbox)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("安全拒绝：默认工作区不能是符号链接 (%s)", sandbox)
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("检查默认工作区路径失败: %w", err)
	}
	if filepath.Base(cleanSandbox) != "sandbox" {
		return fmt.Errorf("安全拒绝：仅允许清理专用 sandbox 目录 (%s)", sandbox)
	}
	sandbox = cleanSandbox
	if err := os.MkdirAll(sandbox, 0700); err != nil {
		return fmt.Errorf("创建默认工作区失败: %w", err)
	}
	if err := os.Chmod(sandbox, 0700); err != nil {
		return fmt.Errorf("收紧默认工作区权限失败: %w", err)
	}

	entries, err := os.ReadDir(sandbox)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.Name() == ".gitignore" {
			continue
		}
		target := filepath.Join(sandbox, entry.Name())
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("移除临时文件失败 (%s): %w", entry.Name(), err)
		}
	}
	return nil
}

// GetCurrent 获取当前工作区；无法读取持久状态时返回错误，不静默改用默认目录。
