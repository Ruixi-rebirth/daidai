package entity

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	stateMu    sync.Mutex
	mu         sync.RWMutex
	db         *db.Manager
	platform   string
	defaultDir string            // 默认工作区目录；目录本身不提供操作系统级隔离
	cache      map[string]string // channelID -> currentWorkspacePath
}

// ResolveSandboxDir 解析默认工作区路径；未配置或直接指向主目录根/系统根时，收敛为 ~/sandbox。
func ResolveSandboxDir(defaultDir string) string {
	def := strings.TrimSpace(defaultDir)
	home, _ := os.UserHomeDir()
	if home != "" {
		home = filepath.Clean(home)
	}

	if def == "" {
		if home != "" {
			def = filepath.Join(home, "sandbox")
		} else {
			def = "data/workspace/sandbox"
		}
	} else if def == "~" || strings.HasPrefix(def, "~/") || strings.HasPrefix(def, "~"+string(filepath.Separator)) {
		sub := strings.TrimPrefix(def, "~")
		sub = strings.TrimPrefix(sub, "/")
		sub = strings.TrimPrefix(sub, string(filepath.Separator))
		if sub == "" {
			if home != "" {
				def = filepath.Join(home, "sandbox")
			} else {
				def = "data/workspace/sandbox"
			}
		} else if home != "" {
			def = filepath.Join(home, sub)
		} else {
			def = filepath.Clean(sub)
		}
	}

	if !filepath.IsAbs(def) {
		if abs, err := filepath.Abs(def); err == nil {
			def = abs
		}
	}
	def = filepath.Clean(def)
	if home != "" {
		home = resolvePathThroughExistingParent(home)
	}

	if def == "/" || (home != "" && def == home) {
		if home != "" {
			def = filepath.Join(home, "sandbox")
		} else {
			def = "/tmp/daidai/sandbox"
		}
	}

	resolved := resolvePathThroughExistingParent(def)
	if resolved == string(filepath.Separator) || (home != "" && resolved == home) {
		fallback := filepath.Join(home, "sandbox")
		if home == "" {
			fallback = "data/workspace/sandbox"
		}
		resolved = resolvePathThroughExistingParent(fallback)
		if resolved == string(filepath.Separator) || (home != "" && resolved == home) {
			resolved = resolvePathThroughExistingParent("data/workspace/sandbox")
		}
	}
	return resolved
}

func resolvePathThroughExistingParent(path string) string {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return filepath.Clean(path)
	}
	current := absolute
	var suffix []string
	for {
		if realPath, err := filepath.EvalSymlinks(current); err == nil {
			resolved := realPath
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return absolute
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func canonicalWorkspacePath(path string) (string, error) {
	resolved := resolvePathThroughExistingParent(path)
	home, _ := os.UserHomeDir()
	if realHome, err := filepath.EvalSymlinks(home); err == nil {
		home = realHome
	}
	if resolved == string(filepath.Separator) || (home != "" && resolved == filepath.Clean(home)) {
		return "", fmt.Errorf("不能将根目录或整个用户主目录设为工作空间")
	}
	return resolved, nil
}

// NewWorkspaceManager 实例化工作空间中枢
func NewWorkspaceManager(defaultDir string, dbMgr *db.Manager) *WorkspaceManager {
	return NewWorkspaceManagerForPlatform("qq", defaultDir, dbMgr)
}

// NewWorkspaceManagerForPlatform 按指定平台实例化工作空间中枢
func NewWorkspaceManagerForPlatform(platform, defaultDir string, dbMgr *db.Manager) *WorkspaceManager {
	def := ResolveSandboxDir(defaultDir)
	_ = os.MkdirAll(def, 0755)

	if dbMgr == nil {
		dbMgr = db.DefaultManager()
	}
	plat := db.NormPlatform(platform)
	if plat == "" {
		plat = "qq"
	}

	return &WorkspaceManager{
		db:         dbMgr,
		platform:   plat,
		defaultDir: def,
		cache:      make(map[string]string),
	}
}

// ExecuteAction 统一执行工作空间查询与变更指令 (供控制面 Slash 指令与 switch_workspace 工具共同复用，消除重复逻辑与鉴权遗漏)
func (wm *WorkspaceManager) ExecuteAction(channelID string, role UserRole, immune *ImmuneSystem, action, target string, delegates ...*Delegates) string {
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
		return wm.FormatCurrent(channelID)

	case "list":
		return wm.FormatList(channelID)

	case "exit":
		newPath, err := wm.Switch(channelID, "sandbox")
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
		if err := wm.Remove(channelID, target); err != nil {
			return fmt.Sprintf("注销工作空间失败：%v", err)
		}
		return fmt.Sprintf("### 工作空间书签已注销\n\n已成功注销书签：`%s`\n\n> 提示：仅从书签索引中解绑，磁盘上的物理代码文件完好保留。", target)

	case "switch":
		if target == "" {
			return "请指定要切换的目标工作空间路径或工程名"
		}
		newPath, err := wm.Switch(channelID, target)
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

// CurrentMode 获取指定通道当前的运行模式名称 ("默认工作区" 或 "生产工程态")
func (wm *WorkspaceManager) CurrentMode(channelID string) string {
	curr, _ := wm.GetCurrent(channelID)
	if wm.IsSandbox(curr) {
		return "默认工作区"
	}
	return "生产工程态"
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

	entries, err := os.ReadDir(sandbox)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(sandbox, 0755)
		}
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

// GetCurrent 获取指定通道当前激活的工作空间路径与项目名
func (wm *WorkspaceManager) GetCurrent(channelID string) (string, string) {
	path, name, err := wm.GetCurrentChecked(channelID)
	if err != nil {
		slog.Warn("workspace: 无法确认活动工作区，回退默认目录", "channelID", channelID, "err", err)
		return wm.defaultDir, filepath.Base(wm.defaultDir)
	}
	return path, name
}

// GetCurrentChecked 获取当前工作区；无法读取持久状态时返回错误，不静默改用默认目录。
func (wm *WorkspaceManager) GetCurrentChecked(channelID string) (string, string, error) {
	wm.stateMu.Lock()
	defer wm.stateMu.Unlock()
	return wm.getCurrent(channelID)
}

func (wm *WorkspaceManager) getCurrent(channelID string) (string, string, error) {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return wm.defaultDir, filepath.Base(wm.defaultDir), nil
	}

	wm.mu.RLock()
	cached, ok := wm.cache[channelID]
	wm.mu.RUnlock()

	if ok && cached != "" {
		if resolved, err := canonicalWorkspacePath(cached); err == nil {
			if st, statErr := os.Stat(resolved); statErr == nil && st.IsDir() {
				wm.mu.Lock()
				wm.cache[channelID] = resolved
				wm.mu.Unlock()
				return resolved, filepath.Base(resolved), nil
			}
		}
		wm.mu.Lock()
		delete(wm.cache, channelID)
		wm.mu.Unlock()
	}

	// 从 SQLite 状态表中拉取
	if wm.db != nil {
		active, err := wm.db.GetActiveWorkspace(wm.platform, channelID)
		if err != nil {
			return "", "", fmt.Errorf("读取活动工作区失败: %w", err)
		}
		if active != nil && active.Path != "" {
			cleanActive, pathErr := canonicalWorkspacePath(active.Path)
			if pathErr != nil || (strings.EqualFold(active.ProjectName, "sandbox") && cleanActive != wm.defaultDir) {
				_ = wm.db.RemoveChannelWorkspace(wm.platform, channelID, active.Path)
			} else if st, sErr := os.Stat(cleanActive); sErr == nil && st.IsDir() {
				wm.mu.Lock()
				wm.cache[channelID] = cleanActive
				wm.mu.Unlock()
				return cleanActive, filepath.Base(cleanActive), nil
			} else {
				// 物理目录已不存在，自动自愈剔除失效记录
				_ = wm.db.RemoveChannelWorkspace(wm.platform, channelID, active.Path)
			}
		}
	}

	// 兜底并自动激活默认目录
	def := wm.defaultDir
	wm.mu.Lock()
	wm.cache[channelID] = def
	wm.mu.Unlock()
	if wm.db != nil {
		if err := wm.db.SetActiveWorkspace(wm.platform, channelID, def, filepath.Base(def)); err != nil {
			slog.Warn("workspace: 保存默认工作区状态失败", "channelID", channelID, "err", err)
		}
	}
	return def, filepath.Base(def), nil
}

// Switch 显式切换并激活指定通道的工作空间目录 (支持绝对路径、相对路径、家目录波浪号及已纳管项目名)
func (wm *WorkspaceManager) Switch(channelID, target string) (string, error) {
	wm.stateMu.Lock()
	defer wm.stateMu.Unlock()

	channelID = strings.TrimSpace(channelID)
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("目标工作空间路径不可为空")
	}

	currPath, _, err := wm.getCurrent(channelID)
	if err != nil {
		return "", fmt.Errorf("无法确认当前工作区: %w", err)
	}
	resolved := target

	if strings.EqualFold(resolved, "default") || strings.EqualFold(resolved, "exit") || strings.EqualFold(resolved, "sandbox") {
		resolved = wm.defaultDir
		if resolved == "" {
			if wd, err := os.Getwd(); err == nil && wd != "" {
				resolved = wd
			}
		}
	} else if resolved == "~" {
		// A bare home marker means the safe default workspace, never the whole home directory.
		resolved = wm.defaultDir
	} else if strings.HasPrefix(resolved, "~/") || strings.HasPrefix(resolved, "~"+string(filepath.Separator)) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			resolved = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(resolved, "~"), string(filepath.Separator)))
		}
	} else if strings.HasPrefix(resolved, "./") || strings.HasPrefix(resolved, "../") {
		resolved = filepath.Join(currPath, resolved)
	} else if !filepath.IsAbs(resolved) {
		relCandidate := filepath.Join(currPath, resolved)
		if st, err := os.Stat(relCandidate); err == nil && st.IsDir() {
			resolved = relCandidate
		} else {
			var matched []WorkspaceBookmark
			if wm.db != nil {
				list, lErr := wm.db.ListChannelWorkspaces(wm.platform, channelID)
				if lErr != nil {
					return "", fmt.Errorf("读取工作空间书签失败: %w", lErr)
				}
				for _, entry := range list {
					if !strings.EqualFold(entry.ProjectName, resolved) {
						continue
					}
					if st, err := os.Stat(entry.Path); err == nil && st.IsDir() {
						matched = append(matched, WorkspaceBookmark{Path: entry.Path, ProjectName: entry.ProjectName})
					} else {
						_ = wm.db.RemoveChannelWorkspace(wm.platform, channelID, entry.Path)
					}
				}
			}
			if len(matched) > 1 {
				paths := make([]string, len(matched))
				for i, entry := range matched {
					paths[i] = entry.Path
				}
				return "", fmt.Errorf("工程名 %q 对应多个工作空间，请使用完整路径：%s", target, strings.Join(paths, "；"))
			}
			if len(matched) == 1 {
				resolved = matched[0].Path
			} else {
				if home, hErr := os.UserHomeDir(); hErr == nil && home != "" {
					homeCand := filepath.Join(home, resolved)
					if st, err := os.Stat(homeCand); err == nil && st.IsDir() {
						resolved = homeCand
					}
				}
			}
		}
	}

	resolved = filepath.Clean(resolved)
	stat, err := os.Stat(resolved)
	if err != nil || !stat.IsDir() {
		return "", fmt.Errorf("目标目录不存在或不是合法文件夹: %s", resolved)
	}
	resolved, err = canonicalWorkspacePath(resolved)
	if err != nil {
		return "", fmt.Errorf("安全拒绝：%w (%s)", err, resolved)
	}

	projectName := filepath.Base(resolved)

	if wm.db != nil {
		if err := wm.db.SetActiveWorkspace(wm.platform, channelID, resolved, projectName); err != nil {
			return "", fmt.Errorf("保存工作空间状态失败: %w", err)
		}
	}

	wm.mu.Lock()
	wm.cache[channelID] = resolved
	wm.mu.Unlock()

	return resolved, nil
}

// Remove 从通道书签列表中注销指定工作空间 (物理文件完好保留)
func (wm *WorkspaceManager) Remove(channelID, target string) error {
	wm.stateMu.Lock()
	defer wm.stateMu.Unlock()

	channelID = strings.TrimSpace(channelID)
	target = strings.TrimSpace(target)
	if target == "" {
		return fmt.Errorf("目标工作空间不可为空")
	}

	currPath, _, err := wm.getCurrent(channelID)
	if err != nil {
		return fmt.Errorf("无法确认当前工作区: %w", err)
	}
	if wm.db == nil {
		return fmt.Errorf("工作空间书签存储未配置")
	}
	entries, err := wm.db.ListChannelWorkspaces(wm.platform, channelID)
	if err != nil {
		return fmt.Errorf("读取工作空间书签失败: %w", err)
	}

	pathCandidate := target
	switch {
	case target == "~":
		pathCandidate = wm.defaultDir
	case strings.HasPrefix(target, "~/") || strings.HasPrefix(target, "~"+string(filepath.Separator)):
		if home, homeErr := os.UserHomeDir(); homeErr == nil && home != "" {
			pathCandidate = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(target, "~"), string(filepath.Separator)))
		}
	case strings.HasPrefix(target, "./") || strings.HasPrefix(target, "../"):
		pathCandidate = filepath.Join(currPath, target)
	case !filepath.IsAbs(target):
		pathCandidate = filepath.Join(currPath, target)
	}
	pathCandidate = filepath.Clean(pathCandidate)
	canonicalCandidate, canonicalErr := canonicalWorkspacePath(pathCandidate)
	if canonicalErr == nil {
		pathCandidate = canonicalCandidate
	}

	targetPath := ""
	var namedMatches []db.WorkspaceEntry
	for _, entry := range entries {
		if filepath.Clean(entry.Path) == pathCandidate {
			targetPath = entry.Path
		}
		if strings.EqualFold(entry.ProjectName, target) {
			namedMatches = append(namedMatches, entry)
		}
	}
	targetIsPath := filepath.IsAbs(target) || target == "~" || strings.ContainsRune(target, filepath.Separator)
	if len(namedMatches) > 1 && !targetIsPath {
		paths := make([]string, len(namedMatches))
		for i, entry := range namedMatches {
			paths[i] = entry.Path
		}
		return fmt.Errorf("工程名 %q 对应多个工作空间，请使用完整路径：%s", target, strings.Join(paths, "；"))
	}
	if targetPath == "" {
		if len(namedMatches) > 1 {
			paths := make([]string, len(namedMatches))
			for i, entry := range namedMatches {
				paths[i] = entry.Path
			}
			return fmt.Errorf("工程名 %q 对应多个工作空间，请使用完整路径：%s", target, strings.Join(paths, "；"))
		}
		if len(namedMatches) == 1 {
			targetPath = namedMatches[0].Path
		}
	}
	if targetPath == "" {
		return fmt.Errorf("未找到工作空间书签 %q", target)
	}

	targetPath = filepath.Clean(targetPath)
	if wm.IsSandbox(targetPath) {
		return fmt.Errorf("无法注销默认工作区")
	}
	if filepath.Clean(targetPath) == filepath.Clean(currPath) {
		return fmt.Errorf("无法注销当前正在激活中的工作空间 (%s)，请先切入其他项目或回退至默认工作区后再注销", targetPath)
	}

	return wm.db.RemoveChannelWorkspace(wm.platform, channelID, targetPath)
}

// ListBookmarked 获取当前通道已激活纳管的工作空间书签列表。
// 数据库读取失败时保留旧接口行为，同时记录错误；需要区分错误的调用方应使用 ListBookmarkedChecked。
func (wm *WorkspaceManager) ListBookmarked(channelID string) (currentPath string, bookmarks []WorkspaceBookmark) {
	currentPath, bookmarks, err := wm.ListBookmarkedChecked(channelID)
	if err != nil {
		slog.Warn("workspace: 读取书签列表失败", "channelID", channelID, "err", err)
	}
	return currentPath, bookmarks
}

// ListBookmarkedChecked 获取当前通道书签列表，并将数据库读取错误返回给调用方。
func (wm *WorkspaceManager) ListBookmarkedChecked(channelID string) (currentPath string, bookmarks []WorkspaceBookmark, err error) {
	wm.stateMu.Lock()
	defer wm.stateMu.Unlock()
	currentPath, _, err = wm.getCurrent(channelID)
	if err != nil {
		return "", nil, fmt.Errorf("无法确认当前工作区: %w", err)
	}

	if wm.db != nil {
		entries, listErr := wm.db.ListChannelWorkspaces(wm.platform, channelID)
		if listErr != nil {
			return currentPath, nil, fmt.Errorf("读取工作空间书签失败: %w", listErr)
		}
		hasCurrent := false
		home, _ := os.UserHomeDir()
		cleanHome := ""
		if home != "" {
			cleanHome = filepath.Clean(home)
		}
		for _, e := range entries {
			cleanPath := filepath.Clean(e.Path)
			if cleanPath == "/" || (cleanHome != "" && cleanPath == cleanHome) {
				_ = wm.db.RemoveChannelWorkspace(wm.platform, channelID, e.Path)
				continue
			}
			if !wm.IsSandbox(cleanPath) {
				if st, err := os.Stat(cleanPath); err != nil || !st.IsDir() {
					_ = wm.db.RemoveChannelWorkspace(wm.platform, channelID, e.Path)
					continue
				}
			}

			isCurr := (e.Path == currentPath)
			if isCurr {
				hasCurrent = true
			}
			bookmarks = append(bookmarks, WorkspaceBookmark{
				Path:        e.Path,
				ProjectName: e.ProjectName,
				IsCurrent:   isCurr,
			})
		}
		if !hasCurrent && currentPath != "" {
			bookmarks = append([]WorkspaceBookmark{{
				Path:        currentPath,
				ProjectName: filepath.Base(currentPath),
				IsCurrent:   true,
			}}, bookmarks...)
		}
	} else {
		bookmarks = append(bookmarks, WorkspaceBookmark{
			Path:        currentPath,
			ProjectName: filepath.Base(currentPath),
			IsCurrent:   true,
		})
	}

	return currentPath, bookmarks, nil
}

// FormatCurrent 生成当前激活工作空间的优雅 Markdown 摘要
func (wm *WorkspaceManager) FormatCurrent(channelID string) string {
	curr, name, err := wm.GetCurrentChecked(channelID)
	if err != nil {
		return fmt.Sprintf("无法确认当前工作区：%v", err)
	}
	mode := "生产工程态"
	badge := "正式项目"
	if wm.IsSandbox(curr) {
		mode = "默认工作区"
		badge = "默认目录"
		name = "sandbox"
	}

	var sb strings.Builder
	sb.WriteString("### 当前工作空间状态\n\n")
	fmt.Fprintf(&sb, "- **运行模式**：`%s` (%s)\n", mode, badge)
	fmt.Fprintf(&sb, "- **工程名称**：`%s`\n", name)
	fmt.Fprintf(&sb, "- **物理路径**：`%s`\n", curr)
	if wm.IsSandbox(curr) {
		sb.WriteString("\n> 当前处于默认工作区。工程工作者默认以此目录为工作目录；该目录不构成操作系统级隔离。")
	} else {
		sb.WriteString("\n> 当前处于生产工程中。工单可能直接修改该项目文件；工作目录不构成操作系统级隔离。随时可发送 `/ws exit` 切回默认工作区。")
	}
	return strings.TrimSpace(sb.String())
}

// FormatList 生成已纳管工作空间书签清单的结构化 Markdown
func (wm *WorkspaceManager) FormatList(channelID string) string {
	curr, bookmarks, err := wm.ListBookmarkedChecked(channelID)
	if err != nil {
		return fmt.Sprintf("无法读取工作空间书签列表：%v", err)
	}

	var sb strings.Builder
	sb.WriteString("### 工作空间书签管理\n\n")

	if wm.IsSandbox(curr) {
		fmt.Fprintf(&sb, "- **`[当前聚焦]` `sandbox`** `(默认工作区)`\n  - 物理路径：`%s`\n", wm.defaultDir)
	} else {
		fmt.Fprintf(&sb, "- **`[默认工作区]` `sandbox`** `(默认目录)`\n  - 物理路径：`%s`\n", wm.defaultDir)
	}

	hasProjects := false
	for _, bm := range bookmarks {
		if wm.IsSandbox(bm.Path) {
			continue
		}
		hasProjects = true
		if bm.IsCurrent {
			fmt.Fprintf(&sb, "- **`[当前聚焦]` `%s`** `(生产工程)`\n  - 物理路径：`%s`\n", bm.ProjectName, bm.Path)
		} else {
			fmt.Fprintf(&sb, "- **`[已纳管]` `%s`** `(生产工程)`\n  - 物理路径：`%s`\n", bm.ProjectName, bm.Path)
		}
	}
	if !hasProjects {
		sb.WriteString("\n*(暂未纳管其他生产工程，可使用 `/cd <项目路径>` 随时纳管)*\n")
	}

	sb.WriteString("\n---\n")
	sb.WriteString("> **操作指引**：\n")
	sb.WriteString("> - `/cd <工程名或路径>`：切入具体生产工程\n")
	sb.WriteString("> - `/ws exit`：退出工程，回退到默认工作区\n")
	sb.WriteString("> - `/ws clean`：清理专用默认工作区目录\n")
	sb.WriteString("> - `/ws rm <工程名>`：从书签列表中注销 (磁盘代码完好保留)")
	return strings.TrimSpace(sb.String())
}
