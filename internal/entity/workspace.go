package entity

import (
	"fmt"
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

// WorkspaceManager 通道级动态工作空间中枢 (SQLite 状态持久化 + 显式激活纳管 + 沙盒演练态与生产工程态双态调度)
type WorkspaceManager struct {
	mu         sync.RWMutex
	db         *db.Manager
	platform   string
	defaultDir string            // 默认为隔离沙盒目录
	cache      map[string]string // channelID -> currentWorkspacePath
}

// ResolveSandboxDir 解析并规范化沙盒目录路径，确保沙盒处于受隔离的安全子目录中，
// 若未配置或直接指向主目录根/系统根，自动收敛为 ~/sandbox
func ResolveSandboxDir(defaultDir string) string {
	def := strings.TrimSpace(defaultDir)
	home, _ := os.UserHomeDir()
	if home != "" {
		home = filepath.Clean(home)
	}

	if def == "" {
		if home != "" {
			return filepath.Join(home, "sandbox")
		}
		return "data/workspace/sandbox"
	}

	if strings.HasPrefix(def, "~") {
		sub := strings.TrimPrefix(def, "~")
		sub = strings.TrimPrefix(sub, "/")
		sub = strings.TrimPrefix(sub, string(filepath.Separator))
		if sub == "" {
			if home != "" {
				return filepath.Join(home, "sandbox")
			}
			return "data/workspace/sandbox"
		}
		if home != "" {
			return filepath.Join(home, sub)
		}
		return filepath.Clean(sub)
	}

	if !filepath.IsAbs(def) {
		if abs, err := filepath.Abs(def); err == nil {
			def = abs
		}
	}
	def = filepath.Clean(def)

	if def == "/" || (home != "" && def == home) {
		if home != "" {
			return filepath.Join(home, "sandbox")
		}
		return "/tmp/daidai/sandbox"
	}

	return def
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
func (wm *WorkspaceManager) ExecuteAction(channelID string, role UserRole, immune *ImmuneSystem, action, target string) string {
	if immune != nil {
		if err := immune.VerifyDelegationPerm(role); err != nil {
			return err.Error()
		}
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
		return fmt.Sprintf("### 已退出生产工程\n\n已切回**沙盒演练态**：`sandbox` (`%s`)\n\n> 后续闲聊与即兴测试均在沙盒中进行，绝不污染正式项目代码。\n> 随时可通过 `/cd <项目名>` 重新切入生产工程。", newPath)

	case "clean":
		if err := wm.CleanSandbox(); err != nil {
			return fmt.Sprintf("清空沙盒失败：%v", err)
		}
		return fmt.Sprintf("### 沙盒演练区已清空\n\n沙盒目录 (`%s`) 中的临时草稿与生成物已彻底清理，恢复纯净白板。", wm.SandboxDir())

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
			return fmt.Sprintf("### 已切回沙盒演练态\n\n- **运行模式**：`沙盒演练态` (安全隔离草稿区)\n- **物理路径**：`%s`\n\n> 闲聊、即兴测试与临时 demo 均在沙盒中进行，绝不污染任何正式工程。", newPath)
		}
		projectName := filepath.Base(newPath)
		return fmt.Sprintf("### 已切入生产工程\n\n- **工程名称**：`%s`\n- **物理路径**：`%s`\n- **运行模式**：`生产工程态`\n\n> 后续工单与工程任务将严格在该目录下执行。\n> 随时可通过 `/ws exit` 退出并切回沙盒演练区。", projectName, newPath)

	default:
		return fmt.Sprintf("未知操作 %q，支持的操作为 current, list, switch, remove, clean, exit", action)
	}
}

// IsSandbox 校验指定路径是否为隔离沙盒演练区
func (wm *WorkspaceManager) IsSandbox(path string) bool {
	if path == "" {
		return false
	}
	clean := filepath.Clean(path)
	return clean == filepath.Clean(wm.defaultDir) || filepath.Base(clean) == "sandbox"
}

// CurrentMode 获取指定通道当前的运行模式名称 ("沙盒演练态" 或 "生产工程态")
func (wm *WorkspaceManager) CurrentMode(channelID string) string {
	curr, _ := wm.GetCurrent(channelID)
	if wm.IsSandbox(curr) {
		return "沙盒演练态"
	}
	return "生产工程态"
}

// SandboxDir 获取沙盒目录绝对路径
func (wm *WorkspaceManager) SandboxDir() string {
	return wm.defaultDir
}

// CleanSandbox 清空隔离沙盒目录中的所有临时草稿文件 (保留 .gitignore)
func (wm *WorkspaceManager) CleanSandbox() error {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	sandbox := wm.defaultDir
	if sandbox == "" {
		return fmt.Errorf("沙盒目录未初始化")
	}

	home, _ := os.UserHomeDir()
	cleanSandbox := filepath.Clean(sandbox)
	if cleanSandbox == "/" || (home != "" && cleanSandbox == filepath.Clean(home)) {
		return fmt.Errorf("安全拒绝：禁止清空主目录或根目录 (%s)", sandbox)
	}

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
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return wm.defaultDir, filepath.Base(wm.defaultDir)
	}

	wm.mu.RLock()
	cached, ok := wm.cache[channelID]
	wm.mu.RUnlock()

	if ok && cached != "" {
		if st, err := os.Stat(cached); err == nil && st.IsDir() {
			return cached, filepath.Base(cached)
		}
		wm.mu.Lock()
		delete(wm.cache, channelID)
		wm.mu.Unlock()
	}

	// 从 SQLite 状态表中拉取
	if wm.db != nil {
		if active, err := wm.db.GetActiveWorkspace(wm.platform, channelID); err == nil && active != nil && active.Path != "" {
			home, _ := os.UserHomeDir()
			cleanActive := filepath.Clean(active.Path)
			if cleanActive == "/" || (home != "" && cleanActive == filepath.Clean(home)) || (strings.EqualFold(active.ProjectName, "sandbox") && cleanActive != wm.defaultDir) {
				_ = wm.db.RemoveChannelWorkspace(wm.platform, channelID, active.Path)
			} else if st, sErr := os.Stat(cleanActive); sErr == nil && st.IsDir() {
				wm.mu.Lock()
				wm.cache[channelID] = cleanActive
				wm.mu.Unlock()
				return cleanActive, filepath.Base(cleanActive)
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
		_ = wm.db.SetActiveWorkspace(wm.platform, channelID, def, filepath.Base(def))
	}
	return def, filepath.Base(def)
}

// Switch 显式切换并激活指定通道的工作空间目录 (支持绝对路径、相对路径、家目录波浪号及已纳管项目名)
func (wm *WorkspaceManager) Switch(channelID, target string) (string, error) {
	channelID = strings.TrimSpace(channelID)
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("目标工作空间路径不可为空")
	}

	currPath, _ := wm.GetCurrent(channelID)
	resolved := target

	if strings.EqualFold(resolved, "default") || strings.EqualFold(resolved, "exit") || strings.EqualFold(resolved, "sandbox") {
		resolved = wm.defaultDir
		if resolved == "" {
			if wd, err := os.Getwd(); err == nil && wd != "" {
				resolved = wd
			}
		}
	} else if strings.HasPrefix(resolved, "~") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			resolved = filepath.Join(home, strings.TrimPrefix(resolved, "~"))
		}
	} else if strings.HasPrefix(resolved, "./") || strings.HasPrefix(resolved, "../") {
		resolved = filepath.Join(currPath, resolved)
	} else if !filepath.IsAbs(resolved) {
		relCandidate := filepath.Join(currPath, resolved)
		if st, err := os.Stat(relCandidate); err == nil && st.IsDir() {
			resolved = relCandidate
		} else {
			matched := ""
			if wm.db != nil {
				if list, lErr := wm.db.ListChannelWorkspaces(wm.platform, channelID); lErr == nil {
					for _, entry := range list {
						if strings.EqualFold(entry.ProjectName, resolved) {
							if st, err := os.Stat(entry.Path); err == nil && st.IsDir() {
								matched = entry.Path
								break
							} else {
								_ = wm.db.RemoveChannelWorkspace(wm.platform, channelID, entry.Path)
							}
						}
					}
				}
			}
			if matched != "" {
				resolved = matched
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
	channelID = strings.TrimSpace(channelID)
	target = strings.TrimSpace(target)
	if target == "" {
		return fmt.Errorf("目标工作空间不可为空")
	}

	currPath, _ := wm.GetCurrent(channelID)
	targetPath := target

	if wm.db != nil {
		if list, lErr := wm.db.ListChannelWorkspaces(wm.platform, channelID); lErr == nil {
			for _, entry := range list {
				if strings.EqualFold(entry.ProjectName, target) || strings.EqualFold(entry.Path, target) {
					targetPath = entry.Path
					break
				}
			}
		}
	}

	targetPath = filepath.Clean(targetPath)
	if wm.IsSandbox(targetPath) {
		return fmt.Errorf("无法注销全局沙盒演练区")
	}
	if targetPath == currPath {
		return fmt.Errorf("无法注销当前正在激活中的工作空间 (%s)，请先切入其他项目或回退至沙盒后再注销", targetPath)
	}

	if wm.db != nil {
		return wm.db.RemoveChannelWorkspace(wm.platform, channelID, targetPath)
	}
	return nil
}

// ListBookmarked 获取当前通道已激活纳管的工作空间书签列表
func (wm *WorkspaceManager) ListBookmarked(channelID string) (currentPath string, bookmarks []WorkspaceBookmark) {
	currentPath, _ = wm.GetCurrent(channelID)

	if wm.db != nil {
		entries, _ := wm.db.ListChannelWorkspaces(wm.platform, channelID)
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

	return currentPath, bookmarks
}

// FormatCurrent 生成当前激活工作空间的优雅 Markdown 摘要
func (wm *WorkspaceManager) FormatCurrent(channelID string) string {
	curr, name := wm.GetCurrent(channelID)
	mode := "生产工程态"
	badge := "正式项目"
	if wm.IsSandbox(curr) {
		mode = "沙盒演练态"
		badge = "临时草稿区"
		name = "sandbox"
	}

	var sb strings.Builder
	sb.WriteString("### 当前工作空间状态\n\n")
	sb.WriteString(fmt.Sprintf("- **运行模式**：`%s` (%s)\n", mode, badge))
	sb.WriteString(fmt.Sprintf("- **工程名称**：`%s`\n", name))
	sb.WriteString(fmt.Sprintf("- **物理路径**：`%s`\n", curr))
	if wm.IsSandbox(curr) {
		sb.WriteString("\n> 当前处于安全沙盒中。闲聊、即兴测试与临时脚本均在沙盒中进行，绝不污染生产工程。")
	} else {
		sb.WriteString("\n> 当前处于生产工程中。工单执行将直接修改该项目文件。随时可发送 `/ws exit` 切回沙盒演练区。")
	}
	return strings.TrimSpace(sb.String())
}

// FormatList 生成已纳管工作空间书签清单的结构化 Markdown
func (wm *WorkspaceManager) FormatList(channelID string) string {
	curr, _ := wm.GetCurrent(channelID)
	_, bookmarks := wm.ListBookmarked(channelID)

	var sb strings.Builder
	sb.WriteString("### 工作空间书签管理\n\n")

	if wm.IsSandbox(curr) {
		sb.WriteString(fmt.Sprintf("- **`[当前聚焦]` `sandbox`** `(沙盒演练区)`\n  - 物理路径：`%s`\n", wm.defaultDir))
	} else {
		sb.WriteString(fmt.Sprintf("- **`[草稿沙盒]` `sandbox`** `(沙盒演练区)`\n  - 物理路径：`%s`\n", wm.defaultDir))
	}

	hasProjects := false
	for _, bm := range bookmarks {
		if wm.IsSandbox(bm.Path) {
			continue
		}
		hasProjects = true
		if bm.IsCurrent {
			sb.WriteString(fmt.Sprintf("- **`[当前聚焦]` `%s`** `(生产工程)`\n  - 物理路径：`%s`\n", bm.ProjectName, bm.Path))
		} else {
			sb.WriteString(fmt.Sprintf("- **`[已纳管]` `%s`** `(生产工程)`\n  - 物理路径：`%s`\n", bm.ProjectName, bm.Path))
		}
	}
	if !hasProjects {
		sb.WriteString("\n*(暂未纳管其他生产工程，可使用 `/cd <项目路径>` 随时纳管)*\n")
	}

	sb.WriteString("\n---\n")
	sb.WriteString("> **操作指引**：\n")
	sb.WriteString("> - `/cd <工程名或路径>`：切入具体生产工程\n")
	sb.WriteString("> - `/ws exit`：退出工程，回退到沙盒演练区\n")
	sb.WriteString("> - `/ws clean`：一键清空沙盒里的临时草稿文件\n")
	sb.WriteString("> - `/ws rm <工程名>`：从书签列表中注销 (磁盘代码完好保留)")
	return strings.TrimSpace(sb.String())
}
