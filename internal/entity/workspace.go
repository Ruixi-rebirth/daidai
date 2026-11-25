package entity

import (
	"log/slog"
	"os"
	"sync"

	"daidai/internal/db"
)

// WorkspaceBookmark 通道已纳管的工作空间书签
type WorkspaceBookmark struct {
	Path        string `json:"path"`
	ProjectName string `json:"project_name"`
	IsCurrent   bool   `json:"is_current"`
}

// WorkspaceManager 通道级动态工作空间中枢 (SQLite 状态持久化 + 显式激活纳管 + 默认工作区与工程目录双态调度)
type WorkspaceManager struct {
	mu         sync.Mutex
	db         *db.Manager
	defaultDir string                  // 默认工作区目录；目录本身不提供操作系统级隔离
	cache      map[workspaceKey]string // (platform, channelID) -> currentWorkspacePath
}

type workspaceKey struct {
	platform  string
	channelID string
}

// ResolveSandboxDir 解析默认工作区路径；未配置或直接指向主目录根/系统根时，收敛为 ~/sandbox。
func NewWorkspaceManager(defaultDir string, dbMgr *db.Manager) *WorkspaceManager {
	def := ResolveSandboxDir(defaultDir)
	if err := os.MkdirAll(def, 0700); err != nil {
		slog.Error("workspace: 创建默认工作区失败", "path", def, "err", err)
	} else if err := os.Chmod(def, 0700); err != nil {
		slog.Error("workspace: 收紧默认工作区权限失败", "path", def, "err", err)
	}

	return &WorkspaceManager{
		db:         dbMgr,
		defaultDir: def,
		cache:      make(map[workspaceKey]string),
	}
}

// ExecuteAction 统一执行工作空间查询与变更指令 (供控制面 Slash 指令与 switch_workspace 工具共同复用，消除重复逻辑与鉴权遗漏)
