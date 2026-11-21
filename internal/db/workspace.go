package db

import (
	"database/sql"
	"fmt"
	"time"
)

type WorkspaceEntry struct {
	ChannelID    string `json:"channel_id"`
	Path         string `json:"path"`
	ProjectName  string `json:"project_name"`
	IsCurrent    bool   `json:"is_current"`
	LastActiveAt int64  `json:"last_active_at"`
}

// SetActiveWorkspace 设置并持久化当前通道的活跃工作空间
func (m *Manager) SetActiveWorkspace(platform, channelID, path, projectName string) error {
	if channelID == "" || path == "" {
		return fmt.Errorf("channelID and path cannot be empty")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	nowMs := time.Now().UnixMilli()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// 先将本通道下所有工作空间标记为非当前
	if _, err := tx.Exec("UPDATE channel_workspaces SET is_current = 0 WHERE channel_id = ?", channelID); err != nil {
		return err
	}

	// 插入或更新目标工作空间为当前活跃
	_, err = tx.Exec(
		`INSERT INTO channel_workspaces (channel_id, workspace_path, project_name, is_current, last_active_at)
		 VALUES (?, ?, ?, 1, ?)
		 ON CONFLICT(channel_id, workspace_path) DO UPDATE SET
		   is_current = 1,
		   project_name = excluded.project_name,
		   last_active_at = excluded.last_active_at`,
		channelID, path, projectName, nowMs,
	)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// GetActiveWorkspace 获取指定通道当前激活的工作空间
func (m *Manager) GetActiveWorkspace(platform, channelID string) (*WorkspaceEntry, error) {
	if channelID == "" {
		return nil, fmt.Errorf("channelID cannot be empty")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return nil, err
	}

	row := db.QueryRow(
		"SELECT workspace_path, project_name, is_current, last_active_at FROM channel_workspaces WHERE channel_id = ? AND is_current = 1 LIMIT 1",
		channelID,
	)
	var e WorkspaceEntry
	var isCurr int
	if err := row.Scan(&e.Path, &e.ProjectName, &isCurr, &e.LastActiveAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	e.ChannelID = channelID
	e.IsCurrent = isCurr == 1
	return &e, nil
}

// ListChannelWorkspaces 查询指定通道的历史工作空间列表 (按活跃时间倒序)
func (m *Manager) ListChannelWorkspaces(platform, channelID string) ([]WorkspaceEntry, error) {
	if channelID == "" {
		return nil, fmt.Errorf("channelID cannot be empty")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(
		"SELECT workspace_path, project_name, is_current, last_active_at FROM channel_workspaces WHERE channel_id = ? ORDER BY is_current DESC, last_active_at DESC",
		channelID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var list []WorkspaceEntry
	for rows.Next() {
		var e WorkspaceEntry
		var isCurr int
		if err := rows.Scan(&e.Path, &e.ProjectName, &isCurr, &e.LastActiveAt); err != nil {
			return nil, err
		}
		e.ChannelID = channelID
		e.IsCurrent = isCurr == 1
		list = append(list, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

// RemoveChannelWorkspace 从通道书签库中注销工作空间条目 (物理磁盘文件完好保留)。重复删除视为成功，便于并发清理失效条目。
func (m *Manager) RemoveChannelWorkspace(platform, channelID, path string) error {
	if channelID == "" || path == "" {
		return fmt.Errorf("channelID and path cannot be empty")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	_, err = db.Exec("DELETE FROM channel_workspaces WHERE channel_id = ? AND workspace_path = ?", channelID, path)
	return err
}

func (m *Manager) resetSessionHistory(platform, chatType, convID string, clearDrafts bool) error {
	platform = NormPlatform(platform)
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var resetID int64
	if err := tx.QueryRow("SELECT COALESCE(MAX(id), 0) FROM messages WHERE chat_type = ? AND conv_id = ?", chatType, convID).Scan(&resetID); err != nil {
		return err
	}

	if _, err := tx.Exec(
		`INSERT INTO session_resets (conv_id, reset_id) VALUES (?, ?)
		 ON CONFLICT(conv_id) DO UPDATE SET reset_id = excluded.reset_id`,
		sessionResetKey(chatType, convID), resetID,
	); err != nil {
		return err
	}
	if clearDrafts {
		if _, err := tx.Exec("DELETE FROM conversation_drafts WHERE platform = ? AND chat_type = ? AND conv_id = ?", platform, chatType, convID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ResetSessionHistoryInChat resets only the requested conversation type.
func (m *Manager) ResetSessionHistoryInChat(platform, chatType, convID string) error {
	return m.resetChat(platform, chatType, convID, false)
}

// ResetConversation clears both working history visibility and resumable drafts.
func (m *Manager) ResetConversation(platform, chatType, convID string) error {
	return m.resetChat(platform, chatType, convID, true)
}

func (m *Manager) resetChat(platform, chatType, convID string, clearDrafts bool) error {
	if chatType != "private" && chatType != "group" {
		return fmt.Errorf("chatType must be private or group")
	}
	if convID == "" {
		return fmt.Errorf("convID cannot be empty")
	}
	return m.resetSessionHistory(platform, chatType, convID, clearDrafts)
}

func sessionResetKey(chatType, convID string) string {
	return chatType + ":" + convID
}
