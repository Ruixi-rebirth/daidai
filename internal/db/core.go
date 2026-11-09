package db

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"daidai/internal/core"

	_ "modernc.org/sqlite"
)

func encodeFloat32Slice(vec []float32) []byte {
	if len(vec) == 0 {
		return nil
	}
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

func decodeFloat32Slice(blob []byte) []float32 {
	if len(blob) == 0 || len(blob)%4 != 0 {
		return nil
	}
	n := len(blob) / 4
	vec := make([]float32, n)
	for i := 0; i < n; i++ {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return vec
}

const schema = `
CREATE TABLE IF NOT EXISTS messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    platform     TEXT NOT NULL,
    chat_type    TEXT NOT NULL DEFAULT 'private', -- 'private' 或 'group'
    conv_id      TEXT NOT NULL,                   -- 纯净会话ID (纯群号或纯私聊标识)
    user_id      TEXT NOT NULL,                   -- 纯净用户ID (纯QQ号或OpenID)
    name         TEXT NOT NULL,
    text         TEXT NOT NULL,
    is_bot       INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,                -- 毫秒级时间戳
    reply_to_uid TEXT NOT NULL DEFAULT '',
    embedding    BLOB                             -- 可选稠密语义向量 (float32 小端字节序)
);
CREATE INDEX IF NOT EXISTS idx_messages_lookup ON messages(platform, chat_type, conv_id, id);
CREATE INDEX IF NOT EXISTS idx_messages_conv ON messages(platform, conv_id, id);
CREATE INDEX IF NOT EXISTS idx_messages_time ON messages(platform, chat_type, conv_id, created_at);

CREATE TABLE IF NOT EXISTS user_facts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    platform     TEXT NOT NULL,
    user_id      TEXT NOT NULL,
    scope_type   TEXT NOT NULL DEFAULT 'global',  -- 'global': 跨场景全局画像, 'group': 特定群专属画像
    scope_id     TEXT NOT NULL DEFAULT '',        -- 当 scope_type 为 group 时的纯群号
    fact         TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'active',  -- 'active': 生效, 'superseded': 已被新事实覆盖, 'deleted': 用户否定废弃
    quote        TEXT NOT NULL DEFAULT '',        -- 来源用户原话
    created_at   INTEGER NOT NULL,                -- 毫秒级时间戳
    updated_at   INTEGER NOT NULL,
    embedding    BLOB                             -- 可选稠密语义向量 (float32 小端字节序)
);
CREATE INDEX IF NOT EXISTS idx_facts_lookup ON user_facts(platform, user_id, status);

CREATE TABLE IF NOT EXISTS msg_cache (
    msg_id     TEXT PRIMARY KEY,
    content    TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_msg_cache_time ON msg_cache(updated_at);

CREATE TABLE IF NOT EXISTS channel_workspaces (
    channel_id     TEXT NOT NULL,
    workspace_path TEXT NOT NULL,
    project_name   TEXT NOT NULL,
    is_current     INTEGER NOT NULL DEFAULT 0,
    last_active_at INTEGER NOT NULL,
    PRIMARY KEY (channel_id, workspace_path)
);
CREATE INDEX IF NOT EXISTS idx_workspace_channel ON channel_workspaces(channel_id, is_current);

CREATE TABLE IF NOT EXISTS session_resets (
    conv_id    TEXT PRIMARY KEY,
    reset_at   INTEGER NOT NULL
);
`

// Manager 统一多平台 SQLite 分库连接管理器 (纯净零前缀规范)
type Manager struct {
	baseDir string
	mu      sync.RWMutex
	conns   map[string]*sql.DB
}

var (
	defaultMgr  *Manager
	defaultOnce sync.Once
)

// DefaultManager 全局默认单例 (位于 data/db/)
func DefaultManager() *Manager {
	defaultOnce.Do(func() {
		defaultMgr = NewManager("data/db")
	})
	return defaultMgr
}

// NewManager 指定目录新建数据库管理器
func NewManager(baseDir string) *Manager {
	return &Manager{
		baseDir: baseDir,
		conns:   make(map[string]*sql.DB),
	}
}

// NormPlatform 规范化平台名称
func NormPlatform(platform string) string {
	return strings.ToLower(strings.TrimSpace(platform))
}

// GetConn 获取指定平台的分库连接池 (qq.db, telegram.db 等)
func (m *Manager) GetConn(platform string) (*sql.DB, error) {
	p := NormPlatform(platform)
	if p == "" {
		return nil, fmt.Errorf("empty platform name")
	}

	m.mu.RLock()
	conn, exists := m.conns[p]
	m.mu.RUnlock()
	if exists {
		return conn, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if conn, exists = m.conns[p]; exists {
		return conn, nil
	}

	if err := os.MkdirAll(m.baseDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir db dir failed: %w", err)
	}

	dbPath := filepath.Join(m.baseDir, p+".db")
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s failed: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema for %s failed: %w", p, err)
	}
	// 幂等兼容平滑升级既有数据库表结构与索引
	_, _ = db.Exec("ALTER TABLE messages ADD COLUMN embedding BLOB")
	_, _ = db.Exec("ALTER TABLE user_facts ADD COLUMN embedding BLOB")
	_, _ = db.Exec("CREATE INDEX IF NOT EXISTS idx_messages_conv ON messages(platform, conv_id, id)")

	m.conns[p] = db
	return db, nil
}

// Close 关闭所有数据库连接
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var firstErr error
	for _, db := range m.conns {
		if err := db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	m.conns = make(map[string]*sql.DB)
	return firstErr
}

// LogMessage 写入纯净原始消息 (零前缀)
func (m *Manager) LogMessage(ctx context.Context, platform, chatType, convID, userID, name, text string, isBot bool, replyToUID string) (int64, error) {
	p := NormPlatform(platform)
	db, err := m.GetConn(p)
	if err != nil {
		return 0, err
	}

	botVal := 0
	if isBot {
		botVal = 1
	}

	nowMs := time.Now().UnixMilli()
	res, err := db.ExecContext(ctx,
		"INSERT INTO messages (platform, chat_type, conv_id, user_id, name, text, is_bot, created_at, reply_to_uid) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		p, chatType, convID, userID, name, text, botVal, nowMs, replyToUID,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// StoredMessage 持久化的会话历史消息记录
type StoredMessage struct {
	ID        int64
	Platform  string
	IsGroup   bool
	ConvID    string
	UserID    string
	Name      string
	Text      string
	IsBot     bool
	Embedding []float32
	CreatedAt time.Time
}

// UpdateMessageEmbedding 异步回填消息的稠密语义向量
func (m *Manager) UpdateMessageEmbedding(ctx context.Context, platform string, msgID int64, vec []float32) error {
	if msgID <= 0 || len(vec) == 0 {
		return nil
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "UPDATE messages SET embedding = ? WHERE id = ?", encodeFloat32Slice(vec), msgID)
	return err
}

// UpdateFactEmbedding 异步回填长期事实的稠密语义向量
func (m *Manager) UpdateFactEmbedding(ctx context.Context, platform string, factID int64, vec []float32) error {
	if factID <= 0 || len(vec) == 0 {
		return nil
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "UPDATE user_facts SET embedding = ? WHERE id = ?", encodeFloat32Slice(vec), factID)
	return err
}

// RecentMessages 读取指定会话最近 limit 条纯净消息 (正序返回)
func (m *Manager) RecentMessages(ctx context.Context, platform, chatType, convID string, limit int) ([]StoredMessage, error) {
	if limit <= 0 {
		limit = 30
	}

	p := NormPlatform(platform)
	db, err := m.GetConn(p)
	if err != nil {
		return nil, err
	}

	var resetAt int64
	_ = db.QueryRowContext(ctx, "SELECT reset_at FROM session_resets WHERE conv_id = ?", convID).Scan(&resetAt)

	query := `
		SELECT id, platform, chat_type, conv_id, user_id, name, text, is_bot, created_at, reply_to_uid 
		FROM messages 
		WHERE platform = ? AND chat_type = ? AND conv_id = ? AND created_at > ?
		ORDER BY id DESC 
		LIMIT ?
	`
	rows, err := db.QueryContext(ctx, query, p, chatType, convID, resetAt, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []StoredMessage
	for rows.Next() {
		var (
			id         int64
			plat       string
			cType      string
			cID        string
			uID        string
			name       string
			text       string
			isBotInt   int
			createdMs  int64
			replyToUID string
		)
		if err := rows.Scan(&id, &plat, &cType, &cID, &uID, &name, &text, &isBotInt, &createdMs, &replyToUID); err != nil {
			return nil, err
		}

		list = append(list, StoredMessage{
			ID:        id,
			Platform:  plat,
			IsGroup:   cType == "group",
			ConvID:    cID,
			UserID:    uID,
			Name:      name,
			Text:      text,
			IsBot:     isBotInt == 1,
			CreatedAt: time.UnixMilli(createdMs),
		})
	}

	// 翻转为正序（早的在前，近的在后）
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}

	return list, nil
}

// SearchMessages 在当前会话的历史消息中按词元集合检索匹配的对话记录 (纯数据访问层，按命中词元数与时间倒序取 top limit 后正序返回)
func (m *Manager) SearchMessages(ctx context.Context, platform, convID string, terms []string, limit int) ([]StoredMessage, error) {
	if convID == "" || len(terms) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}

	p := NormPlatform(platform)
	db, err := m.GetConn(p)
	if err != nil {
		return nil, err
	}

	var (
		whereConds []string
		scoreTerms []string
		whereArgs  []any
		scoreArgs  []any
	)
	for _, t := range terms {
		if t == "" {
			continue
		}
		likeVal := "%" + t + "%"
		whereConds = append(whereConds, "text LIKE ?")
		whereArgs = append(whereArgs, likeVal)
		scoreTerms = append(scoreTerms, "CASE WHEN text LIKE ? THEN 1 ELSE 0 END")
		scoreArgs = append(scoreArgs, likeVal)
	}
	if len(whereConds) == 0 {
		return nil, nil
	}

	args := make([]any, 0, len(whereArgs)+len(scoreArgs)+3)
	args = append(args, p, convID)
	args = append(args, whereArgs...)
	args = append(args, scoreArgs...)
	args = append(args, limit)

	query := fmt.Sprintf(`
		SELECT id, platform, chat_type, conv_id, user_id, name, text, is_bot, created_at, embedding
		FROM messages
		WHERE platform = ? AND conv_id = ? AND (%s)
		ORDER BY (%s) DESC, id DESC
		LIMIT ?
	`, strings.Join(whereConds, " OR "), strings.Join(scoreTerms, " + "))
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []StoredMessage
	for rows.Next() {
		var (
			id        int64
			plat      string
			cType     string
			cID       string
			uID       string
			name      string
			text      string
			isBotInt  int
			createdMs int64
			embBlob   []byte
		)
		if err := rows.Scan(&id, &plat, &cType, &cID, &uID, &name, &text, &isBotInt, &createdMs, &embBlob); err != nil {
			return nil, err
		}
		list = append(list, StoredMessage{
			ID:        id,
			Platform:  plat,
			IsGroup:   cType == "group",
			ConvID:    cID,
			UserID:    uID,
			Name:      name,
			Text:      text,
			IsBot:     isBotInt == 1,
			Embedding: decodeFloat32Slice(embBlob),
			CreatedAt: time.UnixMilli(createdMs),
		})
	}

	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
	return list, nil
}

// RecentVectorMessages 获取指定会话中带有稠密向量的最近 limit 条历史消息（用于向量语义通道召回）
func (m *Manager) RecentVectorMessages(ctx context.Context, platform, convID string, limit int) ([]StoredMessage, error) {
	if convID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 500
	}
	p := NormPlatform(platform)
	db, err := m.GetConn(p)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, `
		SELECT id, platform, chat_type, conv_id, user_id, name, text, is_bot, created_at, embedding
		FROM messages
		WHERE platform = ? AND conv_id = ? AND embedding IS NOT NULL AND length(embedding) > 0
		ORDER BY id DESC
		LIMIT ?
	`, p, convID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []StoredMessage
	for rows.Next() {
		var (
			id        int64
			plat      string
			cType     string
			cID       string
			uID       string
			name      string
			text      string
			isBotInt  int
			createdMs int64
			embBlob   []byte
		)
		if err := rows.Scan(&id, &plat, &cType, &cID, &uID, &name, &text, &isBotInt, &createdMs, &embBlob); err != nil {
			return nil, err
		}
		list = append(list, StoredMessage{
			ID:        id,
			Platform:  plat,
			IsGroup:   cType == "group",
			ConvID:    cID,
			UserID:    uID,
			Name:      name,
			Text:      text,
			IsBot:     isBotInt == 1,
			Embedding: decodeFloat32Slice(embBlob),
			CreatedAt: time.UnixMilli(createdMs),
		})
	}
	return list, nil
}

// ListActiveFacts 获取用户当前生效的全部记忆事实 (活跃状态)
func (m *Manager) ListActiveFacts(ctx context.Context, platform, userID string) ([]*core.Fact, error) {
	p := NormPlatform(platform)
	db, err := m.GetConn(p)
	if err != nil {
		return nil, err
	}

	query := `
		SELECT id, user_id, scope_id, fact, updated_at, embedding
		FROM user_facts 
		WHERE platform = ? AND user_id = ? AND status = 'active'
		ORDER BY id ASC
	`
	rows, err := db.QueryContext(ctx, query, p, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var facts []*core.Fact
	for rows.Next() {
		var (
			id        int64
			uID       string
			scID      string
			factText  string
			updatedMs int64
			embBlob   []byte
		)
		if err := rows.Scan(&id, &uID, &scID, &factText, &updatedMs, &embBlob); err != nil {
			return nil, err
		}
		facts = append(facts, &core.Fact{
			ID:        id,
			UserID:    uID,
			ConvID:    scID,
			Content:   factText,
			Embedding: decodeFloat32Slice(embBlob),
			UpdatedAt: time.UnixMilli(updatedMs),
		})
	}
	return facts, nil
}

// FactMutation 长期记忆事实演进指令
type FactMutation struct {
	Action    string    `json:"action"`               // ADD, UPDATE, DELETE, NONE
	FactID    int64     `json:"fact_id,omitempty"`    // 当 UPDATE 或 DELETE 时指定的目标事实 ID
	Fact      string    `json:"fact,omitempty"`       // 新增或更新后的事实陈述
	ScopeType string    `json:"scope_type,omitempty"` // global 或 group
	ScopeID   string    `json:"scope_id,omitempty"`   // 群号 (若是群专属事实)
	Quote     string    `json:"quote,omitempty"`      // 来源原话
	Embedding []float32 `json:"-"`                    // 可选稠密语义向量
}

// ApplyFactMutations 事务内批量执行事实新增、原地更新与删除（保持 FactID 稳定并校验目标存在性）
func (m *Manager) ApplyFactMutations(ctx context.Context, platform, userID string, mutations []FactMutation) error {
	if len(mutations) == 0 {
		return nil
	}

	p := NormPlatform(platform)
	db, err := m.GetConn(p)
	if err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	nowMs := time.Now().UnixMilli()

	for _, mut := range mutations {
		scopeType := strings.TrimSpace(mut.ScopeType)
		if scopeType == "" {
			scopeType = "global"
		}

		switch strings.ToUpper(strings.TrimSpace(mut.Action)) {
		case "ADD":
			factText := strings.TrimSpace(mut.Fact)
			if factText == "" {
				continue
			}
			var existingID int64
			if err := tx.QueryRowContext(ctx,
				"SELECT id FROM user_facts WHERE platform = ? AND user_id = ? AND status = 'active' AND fact = ? LIMIT 1",
				p, userID, factText,
			).Scan(&existingID); err == nil && existingID > 0 {
				continue
			}
			_, err = tx.ExecContext(ctx,
				"INSERT INTO user_facts (platform, user_id, scope_type, scope_id, fact, status, quote, created_at, updated_at, embedding) VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?, ?)",
				p, userID, scopeType, mut.ScopeID, factText, mut.Quote, nowMs, nowMs, encodeFloat32Slice(mut.Embedding),
			)
			if err != nil {
				return err
			}

		case "UPDATE":
			factText := strings.TrimSpace(mut.Fact)
			if factText == "" || mut.FactID <= 0 {
				continue
			}
			res, err := tx.ExecContext(ctx,
				"UPDATE user_facts SET fact = ?, quote = ?, updated_at = ?, embedding = ? WHERE id = ? AND platform = ? AND user_id = ? AND status = 'active'",
				factText, mut.Quote, nowMs, encodeFloat32Slice(mut.Embedding), mut.FactID, p, userID,
			)
			if err != nil {
				return err
			}
			if affected, _ := res.RowsAffected(); affected == 0 {
				return fmt.Errorf("未找到 ID 为 %d 的生效记忆事实", mut.FactID)
			}

		case "DELETE":
			if mut.FactID <= 0 {
				continue
			}
			res, err := tx.ExecContext(ctx,
				"UPDATE user_facts SET status = 'deleted', updated_at = ? WHERE id = ? AND platform = ? AND user_id = ? AND status = 'active'",
				nowMs, mut.FactID, p, userID,
			)
			if err != nil {
				return err
			}
			if affected, _ := res.RowsAffected(); affected == 0 {
				return fmt.Errorf("未找到 ID 为 %d 的生效记忆事实", mut.FactID)
			}
		}
	}

	return tx.Commit()
}

// SaveMsgCache 保存平台消息 ID 到原文的映射 (用于引用回复反查)
func (m *Manager) SaveMsgCache(platform, msgID, content string) error {
	if msgID == "" || content == "" {
		return nil
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	nowMs := time.Now().UnixMilli()
	_, err = db.Exec(
		`INSERT INTO msg_cache (msg_id, content, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(msg_id) DO UPDATE SET content = excluded.content, updated_at = excluded.updated_at`,
		msgID, content, nowMs,
	)
	return err
}

// MsgCacheEntry 消息缓存条目 (保持时间升序)
type MsgCacheEntry struct {
	MsgID   string
	Content string
}

// LoadMsgCache 按时间升序加载最近 limit 条平台消息缓存，并清理超出上限的陈旧缓存
func (m *Manager) LoadMsgCache(platform string, limit int) ([]MsgCacheEntry, error) {
	if limit <= 0 {
		limit = 1000
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return nil, err
	}
	_, _ = db.Exec("DELETE FROM msg_cache WHERE msg_id NOT IN (SELECT msg_id FROM msg_cache ORDER BY updated_at DESC LIMIT ?)", limit)
	rows, err := db.Query("SELECT msg_id, content FROM msg_cache ORDER BY updated_at ASC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var res []MsgCacheEntry
	for rows.Next() {
		var id, content string
		if err := rows.Scan(&id, &content); err == nil {
			res = append(res, MsgCacheEntry{MsgID: id, Content: content})
		}
	}
	return res, nil
}

// WorkspaceEntry 工作空间条目
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
	defer tx.Rollback()

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
		"SELECT workspace_path, project_name, is_current, last_active_at FROM channel_workspaces WHERE channel_id = ? ORDER BY is_current DESC, last_active_at DESC LIMIT 50",
		channelID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []WorkspaceEntry
	for rows.Next() {
		var e WorkspaceEntry
		var isCurr int
		if err := rows.Scan(&e.Path, &e.ProjectName, &isCurr, &e.LastActiveAt); err == nil {
			e.ChannelID = channelID
			e.IsCurrent = isCurr == 1
			list = append(list, e)
		}
	}
	return list, nil
}

// RemoveChannelWorkspace 从通道书签库中注销工作空间条目 (物理磁盘文件完好保留)
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

// ResetSessionHistory 记录指定会话的历史切断游标 (不删除底层 messages 表日志，但 RecentMessages 将不再读取游标之前的旧对话)
func (m *Manager) ResetSessionHistory(platform, convID string) error {
	if convID == "" {
		return fmt.Errorf("convID cannot be empty")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	nowMs := time.Now().UnixMilli()
	_, err = db.Exec(
		`INSERT INTO session_resets (conv_id, reset_at) VALUES (?, ?)
		 ON CONFLICT(conv_id) DO UPDATE SET reset_at = excluded.reset_at`,
		convID, nowMs,
	)
	return err
}
