package tool

import (
	"fmt"
	"path/filepath"
	"strings"

	"daidai/internal/entity"
)

func (t *DelegateTool) resolveTicketWorkspace(platform, channelID, requested string) (string, error) {
	workspace := strings.TrimSpace(requested)
	if workspace != "" && t.workspaces == nil {
		return "", fmt.Errorf("无法校验目标工作区，已拒绝执行显式路径工单。请挂载工作区管理器，或省略 workspace 使用 Worker 默认目录。")
	}
	if workspace == "" && t.workspaces != nil && channelID != "" {
		current, _, err := t.workspaces.GetCurrent(platform, channelID)
		if err != nil {
			return "", fmt.Errorf("无法确认当前工作区，已拒绝签发工单：%w", err)
		}
		return current, nil
	}
	if workspace == "" || t.workspaces == nil {
		return workspace, nil
	}

	current, _, err := t.workspaces.GetCurrent(platform, channelID)
	if err != nil {
		return "", fmt.Errorf("无法确认当前工作区，已拒绝校验目标：%w", err)
	}
	var bookmarks []entity.WorkspaceBookmark
	if channelID != "" {
		_, bookmarks, err = t.workspaces.ListBookmarked(platform, channelID)
		if err != nil {
			return "", fmt.Errorf("无法校验目标工作空间权限：%w", err)
		}
	}
	workspace = resolveWorkspaceAlias(workspace, current, bookmarks, t.workspaces.SandboxDir())
	if !allowedTicketWorkspace(t.workspaces, workspace, current, bookmarks) {
		return "", fmt.Errorf("【工单排期失败】权限拒绝：工作空间 %q 未纳管。仅允许在当前工作空间、默认工作区或已书签的工程中执行工单。", workspace)
	}
	return workspace, nil
}

func resolveWorkspaceAlias(workspace, current string, bookmarks []entity.WorkspaceBookmark, sandbox string) string {
	switch {
	case strings.EqualFold(workspace, "sandbox"), strings.EqualFold(workspace, "default"),
		strings.EqualFold(workspace, "exit"), strings.EqualFold(workspace, "~"):
		return sandbox
	case strings.EqualFold(workspace, "current"), workspace == ".":
		return current
	default:
		for _, bookmark := range bookmarks {
			if strings.EqualFold(bookmark.ProjectName, workspace) ||
				filepath.Clean(bookmark.Path) == filepath.Clean(workspace) {
				return bookmark.Path
			}
		}
		return workspace
	}
}

func allowedTicketWorkspace(workspaces *entity.WorkspaceManager, workspace, current string, bookmarks []entity.WorkspaceBookmark) bool {
	if current != "" && filepath.Clean(current) == filepath.Clean(workspace) || workspaces.IsSandbox(workspace) {
		return true
	}
	for _, bookmark := range bookmarks {
		if filepath.Clean(bookmark.Path) == filepath.Clean(workspace) {
			return true
		}
	}
	return false
}
