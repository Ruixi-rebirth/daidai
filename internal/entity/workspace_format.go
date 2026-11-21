package entity

import (
	"fmt"
	"strings"
)

func (wm *WorkspaceManager) FormatCurrent(platform, channelID string) string {
	curr, name, err := wm.GetCurrent(platform, channelID)
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
func (wm *WorkspaceManager) FormatList(platform, channelID string) string {
	curr, bookmarks, err := wm.ListBookmarked(platform, channelID)
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
