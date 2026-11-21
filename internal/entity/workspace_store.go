package entity

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"daidai/internal/db"
)

func (wm *WorkspaceManager) GetCurrent(platform, channelID string) (string, string, error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	return wm.getCurrent(platform, channelID)
}

func (wm *WorkspaceManager) getCurrent(platform, channelID string) (string, string, error) {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return wm.defaultDir, filepath.Base(wm.defaultDir), nil
	}

	cached, ok := wm.cache[workspaceKey{db.NormPlatform(platform), channelID}]

	if ok && cached != "" {
		resolved, pathErr := canonicalWorkspacePath(cached)
		if pathErr == nil {
			st, statErr := os.Stat(resolved)
			if statErr == nil && st.IsDir() {
				wm.cache[workspaceKey{db.NormPlatform(platform), channelID}] = resolved
				return resolved, filepath.Base(resolved), nil
			}
			if statErr != nil && !os.IsNotExist(statErr) {
				return "", "", fmt.Errorf("检查缓存工作区失败: %w", statErr)
			}
		}
		delete(wm.cache, workspaceKey{db.NormPlatform(platform), channelID})
	}

	// 从 SQLite 状态表中拉取
	if wm.db != nil {
		active, err := wm.db.GetActiveWorkspace(platform, channelID)
		if err != nil {
			return "", "", fmt.Errorf("读取活动工作区失败: %w", err)
		}
		if active != nil && active.Path != "" {
			cleanActive, pathErr := canonicalWorkspacePath(active.Path)
			if pathErr != nil || (strings.EqualFold(active.ProjectName, "sandbox") && cleanActive != wm.defaultDir) {
				if removeErr := wm.db.RemoveChannelWorkspace(platform, channelID, active.Path); removeErr != nil {
					return "", "", fmt.Errorf("清理无效活动工作区失败: %w", removeErr)
				}
			} else if st, sErr := os.Stat(cleanActive); sErr == nil && st.IsDir() {
				wm.cache[workspaceKey{db.NormPlatform(platform), channelID}] = cleanActive
				return cleanActive, filepath.Base(cleanActive), nil
			} else if sErr == nil || os.IsNotExist(sErr) {
				// 物理目录已不存在，自动自愈剔除失效记录
				if removeErr := wm.db.RemoveChannelWorkspace(platform, channelID, active.Path); removeErr != nil {
					return "", "", fmt.Errorf("清理失效活动工作区失败: %w", removeErr)
				}
			} else {
				return "", "", fmt.Errorf("检查活动工作区失败: %w", sErr)
			}
		}
	}

	// 兜底并自动激活默认目录
	def := wm.defaultDir
	if wm.db != nil {
		if err := wm.db.SetActiveWorkspace(platform, channelID, def, filepath.Base(def)); err != nil {
			return "", "", fmt.Errorf("保存默认工作区状态失败: %w", err)
		}
	}
	wm.cache[workspaceKey{db.NormPlatform(platform), channelID}] = def
	return def, filepath.Base(def), nil
}

// Switch 显式切换并激活指定通道的工作空间目录 (支持绝对路径、相对路径、家目录波浪号及已纳管项目名)
func (wm *WorkspaceManager) Switch(platform, channelID, target string) (string, error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	channelID = strings.TrimSpace(channelID)
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("目标工作空间路径不可为空")
	}

	currPath, _, err := wm.getCurrent(platform, channelID)
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
				list, lErr := wm.db.ListChannelWorkspaces(platform, channelID)
				if lErr != nil {
					return "", fmt.Errorf("读取工作空间书签失败: %w", lErr)
				}
				for _, entry := range list {
					if !strings.EqualFold(entry.ProjectName, resolved) {
						continue
					}
					canonicalPath, pathErr := canonicalWorkspacePath(entry.Path)
					if pathErr != nil {
						if removeErr := wm.db.RemoveChannelWorkspace(platform, channelID, entry.Path); removeErr != nil {
							return "", fmt.Errorf("清理无效工作空间书签失败: %w", removeErr)
						}
						continue
					}
					st, statErr := os.Stat(canonicalPath)
					if statErr == nil && st.IsDir() {
						matched = append(matched, WorkspaceBookmark{Path: entry.Path, ProjectName: entry.ProjectName})
					} else if statErr == nil || os.IsNotExist(statErr) {
						if removeErr := wm.db.RemoveChannelWorkspace(platform, channelID, entry.Path); removeErr != nil {
							return "", fmt.Errorf("清理失效工作空间书签失败: %w", removeErr)
						}
					} else {
						return "", fmt.Errorf("检查工作空间书签失败: %w", statErr)
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
		if err := wm.db.SetActiveWorkspace(platform, channelID, resolved, projectName); err != nil {
			return "", fmt.Errorf("保存工作空间状态失败: %w", err)
		}
	}

	wm.cache[workspaceKey{db.NormPlatform(platform), channelID}] = resolved

	return resolved, nil
}

// Remove 从通道书签列表中注销指定工作空间 (物理文件完好保留)
func (wm *WorkspaceManager) Remove(platform, channelID, target string) error {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	channelID = strings.TrimSpace(channelID)
	target = strings.TrimSpace(target)
	if target == "" {
		return fmt.Errorf("目标工作空间不可为空")
	}

	currPath, _, err := wm.getCurrent(platform, channelID)
	if err != nil {
		return fmt.Errorf("无法确认当前工作区: %w", err)
	}
	if wm.db == nil {
		return fmt.Errorf("工作空间书签存储未配置")
	}
	entries, err := wm.db.ListChannelWorkspaces(platform, channelID)
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

	return wm.db.RemoveChannelWorkspace(platform, channelID, targetPath)
}

// ListBookmarked 获取当前通道书签列表，并将数据库读取错误返回给调用方。
func (wm *WorkspaceManager) ListBookmarked(platform, channelID string) (currentPath string, bookmarks []WorkspaceBookmark, err error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	currentPath, _, err = wm.getCurrent(platform, channelID)
	if err != nil {
		return "", nil, fmt.Errorf("无法确认当前工作区: %w", err)
	}

	if wm.db != nil {
		entries, listErr := wm.db.ListChannelWorkspaces(platform, channelID)
		if listErr != nil {
			return currentPath, nil, fmt.Errorf("读取工作空间书签失败: %w", listErr)
		}
		hasCurrent := false
		for _, e := range entries {
			canonicalPath, pathErr := canonicalWorkspacePath(e.Path)
			if pathErr != nil {
				if removeErr := wm.db.RemoveChannelWorkspace(platform, channelID, e.Path); removeErr != nil {
					return currentPath, nil, fmt.Errorf("清理无效工作空间书签失败: %w", removeErr)
				}
				continue
			}
			if !wm.IsSandbox(canonicalPath) {
				st, statErr := os.Stat(canonicalPath)
				if statErr != nil && !os.IsNotExist(statErr) {
					return currentPath, nil, fmt.Errorf("检查工作空间书签失败: %w", statErr)
				}
				if statErr != nil || !st.IsDir() {
					if removeErr := wm.db.RemoveChannelWorkspace(platform, channelID, e.Path); removeErr != nil {
						return currentPath, nil, fmt.Errorf("清理失效工作空间书签失败: %w", removeErr)
					}
					continue
				}
			}

			isCurr := filepath.Clean(canonicalPath) == filepath.Clean(currentPath)
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
