package db

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
CREATE TABLE IF NOT EXISTS app_migrations (
    name       TEXT PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

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
    platform_msg_id TEXT NOT NULL DEFAULT '',
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
    scope_unknown INTEGER NOT NULL DEFAULT 0,     -- 升级前事实来源范围未知；仅私聊可见
    created_at   INTEGER NOT NULL,                -- 毫秒级时间戳
    updated_at   INTEGER NOT NULL,
    embedding    BLOB                             -- 可选稠密语义向量 (float32 小端字节序)
);
CREATE INDEX IF NOT EXISTS idx_facts_lookup ON user_facts(platform, user_id, status);
CREATE INDEX IF NOT EXISTS idx_facts_scope ON user_facts(platform, user_id, scope_type, scope_id, status);
CREATE INDEX IF NOT EXISTS idx_facts_page ON user_facts(platform, user_id, scope_type, status, updated_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS msg_cache (
    msg_id     TEXT PRIMARY KEY,
    content    TEXT NOT NULL,
    chat_type  TEXT NOT NULL DEFAULT '',
    conv_id    TEXT NOT NULL DEFAULT '',
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
    reset_at   INTEGER NOT NULL,
    reset_id   INTEGER
);

CREATE TABLE IF NOT EXISTS runtime_state (
    state_key  TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS pending_notifications (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    platform     TEXT NOT NULL,
    recipient_id TEXT NOT NULL,
    dedupe_key   TEXT NOT NULL,
    category     TEXT NOT NULL,
    body         TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    UNIQUE(platform, recipient_id, dedupe_key)
);
CREATE INDEX IF NOT EXISTS idx_pending_notifications_owner ON pending_notifications(platform, recipient_id, id);

CREATE TABLE IF NOT EXISTS agent_tickets (
    id              TEXT PRIMARY KEY,
    requester_id    TEXT NOT NULL,
    channel_id      TEXT NOT NULL,
    goal            TEXT NOT NULL,
    worker          TEXT NOT NULL DEFAULT '',
    workspace       TEXT NOT NULL DEFAULT '',
    model           TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL,
    output          TEXT NOT NULL DEFAULT '',
    allow_sensitive INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agent_tickets_owner ON agent_tickets(requester_id, channel_id, created_at DESC);

CREATE TABLE IF NOT EXISTS agy_sessions (
    requester_id    TEXT NOT NULL,
    channel_id      TEXT NOT NULL,
    workspace_path  TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY (requester_id, channel_id, workspace_path)
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
	for _, r := range p {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return nil, fmt.Errorf("invalid platform name %q: use only lowercase letters, digits, '_' or '-'", p)
		}
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

	if err := os.MkdirAll(m.baseDir, 0700); err != nil {
		return nil, fmt.Errorf("mkdir db dir failed: %w", err)
	}
	if err := os.Chmod(m.baseDir, 0700); err != nil {
		return nil, fmt.Errorf("secure db dir permissions failed: %w", err)
	}

	dbPath := filepath.Join(m.baseDir, p+".db")
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s failed: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema for %s failed: %w", p, err)
	}
	// 幂等兼容升级旧数据库；只忽略“字段已存在”，其他迁移错误必须阻止启动。
	for _, migration := range []struct {
		table  string
		column string
		def    string
	}{
		{table: "messages", column: "embedding", def: "BLOB"},
		{table: "user_facts", column: "embedding", def: "BLOB"},
		{table: "session_resets", column: "reset_id", def: "INTEGER"},
		{table: "messages", column: "platform_msg_id", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "user_facts", column: "scope_unknown", def: "INTEGER NOT NULL DEFAULT 0"},
		{table: "msg_cache", column: "chat_type", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "msg_cache", column: "conv_id", def: "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := ensureColumn(db, migration.table, migration.column, migration.def); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("migrate %s.%s failed: %w", migration.table, migration.column, err)
		}
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_messages_platform_msg_id ON messages(platform, platform_msg_id)"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create platform message ID index for %s failed: %w", p, err)
	}
	if err := markLegacyFactScopesUnknown(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mark legacy memory scope for %s failed: %w", p, err)
	}
	if err := redactLegacyAuthMessages(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("redact legacy OAuth authorization messages for %s failed: %w", p, err)
	}
	if err := redactLegacyAuthMessageCache(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("redact legacy OAuth message cache for %s failed: %w", p, err)
	}
	if err := os.Chmod(dbPath, 0600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure database file permissions for %s failed: %w", p, err)
	}

	m.conns[p] = db
	return db, nil
}

func markLegacyFactScopesUnknown(db *sql.DB) error {
	const migrationName = "mark_legacy_fact_scope_unknown_v1"
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var applied int
	err = tx.QueryRow("SELECT 1 FROM app_migrations WHERE name = ?", migrationName).Scan(&applied)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec("UPDATE user_facts SET scope_unknown = 1 WHERE scope_type = 'global'"); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO app_migrations (name, applied_at) VALUES (?, ?)", migrationName, time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// GetRuntimeState loads a small persisted service state value from the platform database.
func (m *Manager) GetRuntimeState(ctx context.Context, platform, key string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("runtime state key cannot be empty")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return "", err
	}
	var value string
	err = db.QueryRowContext(ctx, "SELECT value FROM runtime_state WHERE state_key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

// SetRuntimeState atomically persists a small service state value.
func (m *Manager) SetRuntimeState(ctx context.Context, platform, key, value string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("runtime state key cannot be empty")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO runtime_state (state_key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(state_key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, value, time.Now().UnixMilli())
	return err
}

type PendingNotification struct {
	ID        int64
	Category  string
	Body      string
	CreatedAt time.Time
}

// EnqueuePendingNotification durably queues a notification when the platform
// cannot deliver an unsolicited push. Duplicate keys are idempotent.
func (m *Manager) EnqueuePendingNotification(ctx context.Context, platform, recipientID, dedupeKey, category, body string) (bool, error) {
	platform = NormPlatform(platform)
	recipientID = strings.TrimSpace(recipientID)
	dedupeKey = strings.TrimSpace(dedupeKey)
	category = strings.TrimSpace(category)
	if recipientID == "" || dedupeKey == "" || category == "" || strings.TrimSpace(body) == "" {
		return false, fmt.Errorf("notification recipient, dedupe key, category, and body are required")
	}
	if len(body) > 64<<10 {
		return false, fmt.Errorf("notification body exceeds 64 KiB")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return false, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM pending_notifications
		WHERE platform=? AND recipient_id=? AND dedupe_key=?`, platform, recipientID, dedupeKey).Scan(&exists)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pending_notifications
		(platform, recipient_id, dedupe_key, category, body, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		platform, recipientID, dedupeKey, category, body, time.Now().UnixMilli()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (m *Manager) ListPendingNotifications(ctx context.Context, platform, recipientID string, limit int) ([]PendingNotification, error) {
	db, err := m.GetConn(platform)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, err := db.QueryContext(ctx, `SELECT id, category, body, created_at FROM pending_notifications
		WHERE platform=? AND recipient_id=? ORDER BY id ASC LIMIT ?`, NormPlatform(platform), strings.TrimSpace(recipientID), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var notifications []PendingNotification
	for rows.Next() {
		var notification PendingNotification
		var createdAt int64
		if err := rows.Scan(&notification.ID, &notification.Category, &notification.Body, &createdAt); err != nil {
			return nil, err
		}
		notification.CreatedAt = time.UnixMilli(createdAt)
		notifications = append(notifications, notification)
	}
	return notifications, rows.Err()
}

func (m *Manager) DeletePendingNotification(ctx context.Context, platform, recipientID string, id int64) error {
	if id <= 0 || strings.TrimSpace(recipientID) == "" {
		return fmt.Errorf("notification ID and recipient are required")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `DELETE FROM pending_notifications WHERE id=? AND platform=? AND recipient_id=?`,
		id, NormPlatform(platform), strings.TrimSpace(recipientID))
	return err
}

// UpdatePendingNotificationBody replaces the remaining payload after a
// notification was delivered in bounded pages.
func (m *Manager) UpdatePendingNotificationBody(ctx context.Context, platform, recipientID string, id int64, body string) error {
	if id <= 0 || strings.TrimSpace(recipientID) == "" || strings.TrimSpace(body) == "" {
		return fmt.Errorf("notification ID, recipient, and body are required")
	}
	if len(body) > 64<<10 {
		return fmt.Errorf("notification body exceeds 64 KiB")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE pending_notifications SET body = ? WHERE id = ? AND platform = ? AND recipient_id = ?`,
		body, id, NormPlatform(platform), strings.TrimSpace(recipientID))
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// AgentTicket 是持久化的工程工单记录，不包含运行时取消句柄。
type AgentTicket struct {
	ID, RequesterID, ChannelID, Goal, Worker, Workspace, Model, Status, Output string
	AllowSensitive                                                             bool
	CreatedAt, UpdatedAt                                                       time.Time
}

func (m *Manager) SaveAgentTicket(ctx context.Context, platform string, t AgentTicket) error {
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO agent_tickets
		(id, requester_id, channel_id, goal, worker, workspace, model, status, output, allow_sensitive, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET status=excluded.status, output=excluded.output,
		updated_at=excluded.updated_at WHERE excluded.updated_at > agent_tickets.updated_at`, t.ID, t.RequesterID, t.ChannelID, t.Goal,
		t.Worker, t.Workspace, t.Model, t.Status, t.Output, t.AllowSensitive,
		t.CreatedAt.UnixMilli(), t.UpdatedAt.UnixMilli())
	return err
}

func (m *Manager) InterruptActiveAgentTickets(ctx context.Context, platform string) error {
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `UPDATE agent_tickets SET status='interrupted',
		output=CASE WHEN output='' THEN '进程重启时任务尚未结束；为避免重复副作用，未自动重跑。' ELSE output || char(10) || '进程重启时任务尚未结束；未自动重跑。' END,
		updated_at=? WHERE status IN ('pending','running','stopping')`, time.Now().UnixMilli())
	return err
}

func (m *Manager) ListAgentTickets(ctx context.Context, platform, requesterID, channelID string, limit int) ([]AgentTicket, error) {
	db, err := m.GetConn(platform)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	rows, err := db.QueryContext(ctx, `SELECT id, requester_id, channel_id, goal, worker, workspace, model, status, '' AS output, allow_sensitive, created_at, updated_at
		FROM agent_tickets WHERE requester_id=? AND channel_id=? ORDER BY created_at DESC, id DESC LIMIT ?`, requesterID, channelID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tickets []AgentTicket
	for rows.Next() {
		var t AgentTicket
		var sensitive int
		var createdAt, updatedAt int64
		if err := rows.Scan(&t.ID, &t.RequesterID, &t.ChannelID, &t.Goal, &t.Worker, &t.Workspace, &t.Model, &t.Status, &t.Output, &sensitive, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		t.AllowSensitive = sensitive != 0
		t.CreatedAt = time.UnixMilli(createdAt)
		t.UpdatedAt = time.UnixMilli(updatedAt)
		tickets = append(tickets, t)
	}
	return tickets, rows.Err()
}

func (m *Manager) GetAgentTicket(ctx context.Context, platform, requesterID, channelID, ticketID string) (*AgentTicket, error) {
	db, err := m.GetConn(platform)
	if err != nil {
		return nil, err
	}
	var t AgentTicket
	var sensitive int
	var createdAt, updatedAt int64
	err = db.QueryRowContext(ctx, `SELECT id, requester_id, channel_id, goal, worker, workspace, model, status, output, allow_sensitive, created_at, updated_at
		FROM agent_tickets WHERE requester_id=? AND channel_id=? AND id=?`, requesterID, channelID, ticketID).Scan(
		&t.ID, &t.RequesterID, &t.ChannelID, &t.Goal, &t.Worker, &t.Workspace, &t.Model, &t.Status, &t.Output, &sensitive, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.AllowSensitive = sensitive != 0
	t.CreatedAt = time.UnixMilli(createdAt)
	t.UpdatedAt = time.UnixMilli(updatedAt)
	return &t, nil
}

func (m *Manager) LoadAGYSession(ctx context.Context, platform, requesterID, channelID, workspace string) (string, error) {
	db, err := m.GetConn(platform)
	if err != nil {
		return "", err
	}
	var conversationID string
	err = db.QueryRowContext(ctx, `SELECT conversation_id FROM agy_sessions
		WHERE requester_id=? AND channel_id=? AND workspace_path=?`, requesterID, channelID, workspace).Scan(&conversationID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return conversationID, err
}

func (m *Manager) SaveAGYSession(ctx context.Context, platform, requesterID, channelID, workspace, conversationID string) error {
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO agy_sessions
		(requester_id, channel_id, workspace_path, conversation_id, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(requester_id, channel_id, workspace_path) DO UPDATE SET
		conversation_id=excluded.conversation_id, updated_at=excluded.updated_at`,
		requesterID, channelID, workspace, conversationID, time.Now().UnixMilli())
	return err
}

func (m *Manager) DeleteAGYSessions(ctx context.Context, platform, channelID, workspace string) error {
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `DELETE FROM agy_sessions WHERE channel_id=? AND workspace_path=?`, channelID, workspace)
	return err
}

func redactLegacyAuthMessages(db *sql.DB) error {
	const migrationName = "redact_legacy_oauth_authorization_messages_v1"
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var applied int
	err = tx.QueryRow("SELECT 1 FROM app_migrations WHERE name = ?", migrationName).Scan(&applied)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	const batchSize = 500
	var lastID int64
	for {
		rows, err := tx.Query("SELECT id, text FROM messages WHERE id > ? AND instr(lower(text), '/agy') > 0 ORDER BY id LIMIT ?", lastID, batchSize)
		if err != nil {
			return err
		}
		type message struct {
			id   int64
			text string
		}
		batch := make([]message, 0, batchSize)
		for rows.Next() {
			var item message
			if err := rows.Scan(&item.id, &item.text); err != nil {
				_ = rows.Close()
				return err
			}
			batch = append(batch, item)
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil {
			return rowsErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(batch) == 0 {
			break
		}
		for _, item := range batch {
			lastID = item.id
			if !isLegacyOAuthCodeMessage(item.text) {
				continue
			}
			if _, err := tx.Exec("UPDATE messages SET text = ?, embedding = NULL WHERE id = ?", "[OAuth authorization code redacted]", item.id); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec("INSERT INTO app_migrations (name, applied_at) VALUES (?, ?)", migrationName, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func redactLegacyAuthMessageCache(db *sql.DB) error {
	const migrationName = "delete_legacy_oauth_message_cache_v1"
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var applied int
	err = tx.QueryRow("SELECT 1 FROM app_migrations WHERE name = ?", migrationName).Scan(&applied)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	const batchSize = 500
	var lastRowID int64
	for {
		rows, err := tx.Query(`SELECT rowid, msg_id, content FROM msg_cache
			WHERE rowid > ? AND instr(lower(content), '/agy') > 0 ORDER BY rowid LIMIT ?`, lastRowID, batchSize)
		if err != nil {
			return err
		}
		type cachedMessage struct {
			rowID   int64
			msgID   string
			content string
		}
		batch := make([]cachedMessage, 0, batchSize)
		for rows.Next() {
			var item cachedMessage
			if err := rows.Scan(&item.rowID, &item.msgID, &item.content); err != nil {
				_ = rows.Close()
				return err
			}
			batch = append(batch, item)
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil {
			return rowsErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(batch) == 0 {
			break
		}
		for _, item := range batch {
			lastRowID = item.rowID
			if isLegacyOAuthCodeMessage(item.content) {
				if _, err := tx.Exec("DELETE FROM msg_cache WHERE msg_id = ?", item.msgID); err != nil {
					return err
				}
			}
		}
	}
	if _, err := tx.Exec("INSERT INTO app_migrations (name, applied_at) VALUES (?, ?)", migrationName, time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

func isLegacyOAuthCodeMessage(content string) bool {
	command := strings.ToLower(strings.TrimSpace(content))
	for strings.HasPrefix(command, "<@!") {
		end := strings.IndexByte(command, '>')
		if end < 0 {
			break
		}
		command = strings.TrimSpace(command[end+1:])
	}
	parts := strings.Fields(command)
	return len(parts) >= 3 && parts[0] == "/agy" && (parts[1] == "code" || parts[1] == "auth")
}

func ensureColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + definition)
	return err
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
func (m *Manager) LogMessage(ctx context.Context, platform, chatType, convID, userID, name, text string, isBot bool, replyToUID, platformMsgID string) (int64, error) {
	text = core.TruncateRunes(text, core.MaxUserMessageRunes, "…[消息已截断]")
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
		"INSERT INTO messages (platform, chat_type, conv_id, user_id, name, text, is_bot, created_at, reply_to_uid, platform_msg_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		p, chatType, convID, userID, name, text, botVal, nowMs, replyToUID, platformMsgID,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// MessageContentByPlatformID retrieves the latest stored body for a platform message reference.
func (m *Manager) MessageContentByPlatformID(ctx context.Context, platform, chatType, convID, platformMsgID string) (string, error) {
	if platformMsgID == "" || (chatType != "private" && chatType != "group") || strings.TrimSpace(convID) == "" {
		return "", nil
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return "", err
	}
	var content string
	err = db.QueryRowContext(ctx, "SELECT text FROM messages WHERE platform = ? AND chat_type = ? AND conv_id = ? AND platform_msg_id = ? ORDER BY id DESC LIMIT 1", NormPlatform(platform), chatType, convID, platformMsgID).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return content, err
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
func (m *Manager) UpdateFactEmbedding(ctx context.Context, platform string, factID int64, expectedFact string, vec []float32) error {
	if factID <= 0 || len(vec) == 0 {
		return nil
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "UPDATE user_facts SET embedding = ? WHERE id = ? AND fact = ? AND status = 'active' AND (embedding IS NULL OR length(embedding) = 0)", encodeFloat32Slice(vec), factID, expectedFact)
	return err
}

func sessionHistoryBoundary(ctx context.Context, db *sql.DB, chatType, convID string) (int64, sql.NullInt64, error) {
	var resetAt int64
	var resetID sql.NullInt64
	resetKeys := []string{convID}
	if chatType != "" {
		resetKeys = append([]string{sessionResetKey(chatType, convID)}, resetKeys...)
	}
	for _, resetKey := range resetKeys {
		var candidateAt int64
		var candidateID sql.NullInt64
		err := db.QueryRowContext(ctx, "SELECT reset_at, reset_id FROM session_resets WHERE conv_id = ?", resetKey).Scan(&candidateAt, &candidateID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, sql.NullInt64{}, err
		}
		if candidateAt > resetAt || (candidateAt == resetAt && candidateID.Valid && (!resetID.Valid || candidateID.Int64 > resetID.Int64)) {
			resetAt = candidateAt
			resetID = candidateID
		}
	}
	return resetAt, resetID, nil
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

	resetAt, resetID, err := sessionHistoryBoundary(ctx, db, chatType, convID)
	if err != nil {
		return nil, err
	}

	query := `
		SELECT id, platform, chat_type, conv_id, user_id, name, substr(text, 1, %d), is_bot, created_at, reply_to_uid
		FROM messages 
		WHERE platform = ? AND chat_type = ? AND conv_id = ? AND created_at > ?
		ORDER BY id DESC
		LIMIT ?
	`
	query = fmt.Sprintf(query, core.MaxUserMessageRunes+1)
	queryArgs := []any{p, chatType, convID, resetAt, limit}
	if resetID.Valid {
		query = `
			SELECT id, platform, chat_type, conv_id, user_id, name, substr(text, 1, %d), is_bot, created_at, reply_to_uid
			FROM messages
			WHERE platform = ? AND chat_type = ? AND conv_id = ? AND id > ?
			ORDER BY id DESC
			LIMIT ?
		`
		query = fmt.Sprintf(query, core.MaxUserMessageRunes+1)
		queryArgs = []any{p, chatType, convID, resetID.Int64, limit}
	}
	rows, err := db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

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
			Text:      core.TruncateRunes(text, core.MaxUserMessageRunes, "…[消息已截断]"),
			IsBot:     isBotInt == 1,
			CreatedAt: time.UnixMilli(createdMs),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 翻转为正序（早的在前，近的在后）
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}

	return list, nil
}

// SampleMessagesInChatSince samples evenly across every message in the given
// time range while keeping the returned payload bounded. Session reset
// boundaries are applied before sampling.
func (m *Manager) SampleMessagesInChatSince(ctx context.Context, platform, chatType, convID string, since time.Time, limit int) ([]StoredMessage, error) {
	if chatType != "private" && chatType != "group" {
		return nil, fmt.Errorf("chatType must be private or group")
	}
	if strings.TrimSpace(convID) == "" {
		return nil, fmt.Errorf("conversation ID is required")
	}
	if limit <= 0 {
		limit = 500
	}
	if limit > 1000 {
		limit = 1000
	}
	p := NormPlatform(platform)
	db, err := m.GetConn(p)
	if err != nil {
		return nil, err
	}
	resetAt, resetID, err := sessionHistoryBoundary(ctx, db, chatType, convID)
	if err != nil {
		return nil, err
	}
	where := `platform = ? AND chat_type = ? AND conv_id = ? AND created_at >= ?`
	args := []any{limit, p, chatType, convID, since.UnixMilli()}
	if resetID.Valid {
		where += ` AND id > ?`
		args = append(args, resetID.Int64)
	} else if resetAt > 0 {
		where += ` AND created_at > ?`
		args = append(args, resetAt)
	}
	query := fmt.Sprintf(`
		WITH scoped AS (
			SELECT id, platform, chat_type, conv_id, user_id, name,
			       substr(text, 1, %d) AS text, is_bot, created_at,
			       NTILE(?) OVER (ORDER BY id ASC) AS sample_bucket
			FROM messages WHERE %s
		), ranked AS (
			SELECT id, platform, chat_type, conv_id, user_id, name, text, is_bot, created_at,
			       ROW_NUMBER() OVER (PARTITION BY sample_bucket ORDER BY id ASC) AS bucket_row
			FROM scoped
		)
		SELECT id, platform, chat_type, conv_id, user_id, name, text, is_bot, created_at
		FROM ranked WHERE bucket_row = 1 ORDER BY id ASC`, core.MaxSummaryMessageRunes+1, where)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var messages []StoredMessage
	for rows.Next() {
		var message StoredMessage
		var messageChatType string
		var isBot int
		var createdAt int64
		if err := rows.Scan(&message.ID, &message.Platform, &messageChatType, &message.ConvID, &message.UserID, &message.Name, &message.Text, &isBot, &createdAt); err != nil {
			return nil, err
		}
		message.IsGroup = messageChatType == "group"
		message.IsBot = isBot == 1
		message.CreatedAt = time.UnixMilli(createdAt)
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}

// RecentUserMessagesInChat retrieves only one user's own messages from one chat,
// respecting the same reset boundary as normal conversation recall.
func (m *Manager) RecentUserMessagesInChat(ctx context.Context, platform, chatType, convID, userID string, limit int) ([]StoredMessage, error) {
	if chatType != "private" && chatType != "group" {
		return nil, fmt.Errorf("chatType must be private or group")
	}
	if strings.TrimSpace(convID) == "" || strings.TrimSpace(userID) == "" {
		return nil, fmt.Errorf("conversation ID and user ID are required")
	}
	if limit <= 0 {
		limit = 40
	}
	if limit > 500 {
		limit = 500
	}

	p := NormPlatform(platform)
	db, err := m.GetConn(p)
	if err != nil {
		return nil, err
	}
	resetAt, resetID, err := sessionHistoryBoundary(ctx, db, chatType, convID)
	if err != nil {
		return nil, err
	}
	query := `SELECT id, name, substr(text, 1, %d), created_at
		FROM messages WHERE platform = ? AND chat_type = ? AND conv_id = ?
		AND user_id = ? AND is_bot = 0 AND created_at > ? ORDER BY id DESC LIMIT ?`
	args := []any{p, chatType, convID, userID, resetAt, limit}
	if resetID.Valid {
		query = `SELECT id, name, substr(text, 1, %d), created_at
			FROM messages WHERE platform = ? AND chat_type = ? AND conv_id = ?
			AND user_id = ? AND is_bot = 0 AND id > ? ORDER BY id DESC LIMIT ?`
		args = []any{p, chatType, convID, userID, resetID.Int64, limit}
	}
	query = fmt.Sprintf(query, core.MaxUserMessageRunes+1)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	messages := make([]StoredMessage, 0, limit)
	for rows.Next() {
		var message StoredMessage
		var createdAt int64
		if err := rows.Scan(&message.ID, &message.Name, &message.Text, &createdAt); err != nil {
			return nil, err
		}
		message.Platform = p
		message.IsGroup = chatType == "group"
		message.ConvID = convID
		message.UserID = userID
		message.CreatedAt = time.UnixMilli(createdAt)
		message.Text = core.TruncateRunes(message.Text, core.MaxUserMessageRunes, "…[消息已截断]")
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
	return messages, nil
}

// SearchMessages 在当前会话的历史消息中按词元集合检索匹配的对话记录 (纯数据访问层，按命中词元数与时间倒序取 top limit 后正序返回)
func (m *Manager) SearchMessages(ctx context.Context, platform, convID string, terms []string, limit int) ([]StoredMessage, error) {
	return m.searchMessages(ctx, platform, "", convID, terms, limit)
}

// SearchMessagesInChat searches history within one conversation type, preventing
// a group search from matching a private conversation that happens to share its ID.
func (m *Manager) SearchMessagesInChat(ctx context.Context, platform, chatType, convID string, terms []string, limit int) ([]StoredMessage, error) {
	return m.searchMessages(ctx, platform, chatType, convID, terms, limit)
}

func (m *Manager) searchMessages(ctx context.Context, platform, chatType, convID string, terms []string, limit int) ([]StoredMessage, error) {
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

	queryScope := "platform = ? AND conv_id = ?"
	args := make([]any, 0, len(whereArgs)+len(scoreArgs)+4)
	args = append(args, p, convID)
	if chatType != "" {
		queryScope = "platform = ? AND chat_type = ? AND conv_id = ?"
		args = []any{p, chatType, convID}
	}
	resetAt, resetID, err := sessionHistoryBoundary(ctx, db, chatType, convID)
	if err != nil {
		return nil, err
	}
	if resetID.Valid {
		queryScope += " AND id > ?"
		args = append(args, resetID.Int64)
	} else {
		queryScope += " AND created_at > ?"
		args = append(args, resetAt)
	}
	args = append(args, whereArgs...)
	args = append(args, scoreArgs...)
	args = append(args, limit)

	query := fmt.Sprintf(`
		SELECT id, platform, chat_type, conv_id, user_id, name, substr(text, 1, %d), is_bot, created_at, embedding
		FROM messages
		WHERE %s AND (%s)
		ORDER BY (%s) DESC, id DESC
		LIMIT ?
	`, core.MaxUserMessageRunes+1, queryScope, strings.Join(whereConds, " OR "), strings.Join(scoreTerms, " + "))
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

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
			Text:      core.TruncateRunes(text, core.MaxUserMessageRunes, "…[消息已截断]"),
			IsBot:     isBotInt == 1,
			Embedding: decodeFloat32Slice(embBlob),
			CreatedAt: time.UnixMilli(createdMs),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
	return list, nil
}

// RecentVectorMessages 获取指定会话中带有稠密向量的最近 limit 条历史消息（用于向量语义通道召回）
func (m *Manager) RecentVectorMessages(ctx context.Context, platform, convID string, limit int) ([]StoredMessage, error) {
	return m.recentVectorMessages(ctx, platform, "", convID, limit)
}

// RecentVectorMessagesInChat restricts vector recall to the requested chat type.
func (m *Manager) RecentVectorMessagesInChat(ctx context.Context, platform, chatType, convID string, limit int) ([]StoredMessage, error) {
	return m.recentVectorMessages(ctx, platform, chatType, convID, limit)
}

func (m *Manager) recentVectorMessages(ctx context.Context, platform, chatType, convID string, limit int) ([]StoredMessage, error) {
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

	queryScope := "platform = ? AND conv_id = ?"
	queryArgs := []any{p, convID, limit}
	if chatType != "" {
		queryScope = "platform = ? AND chat_type = ? AND conv_id = ?"
		queryArgs = []any{p, chatType, convID, limit}
	}
	resetAt, resetID, err := sessionHistoryBoundary(ctx, db, chatType, convID)
	if err != nil {
		return nil, err
	}
	if resetID.Valid {
		queryScope += " AND id > ?"
		queryArgs = append(queryArgs[:len(queryArgs)-1], resetID.Int64, limit)
	} else {
		queryScope += " AND created_at > ?"
		queryArgs = append(queryArgs[:len(queryArgs)-1], resetAt, limit)
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, platform, chat_type, conv_id, user_id, name, substr(text, 1, %d), is_bot, created_at, embedding
		FROM messages
		WHERE %s AND embedding IS NOT NULL AND length(embedding) > 0
		ORDER BY id DESC
		LIMIT ?
	`, core.MaxUserMessageRunes+1, queryScope), queryArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

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
			Text:      core.TruncateRunes(text, core.MaxUserMessageRunes, "…[消息已截断]"),
			IsBot:     isBotInt == 1,
			Embedding: decodeFloat32Slice(embBlob),
			CreatedAt: time.UnixMilli(createdMs),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

// ListActiveFacts 获取用户当前生效的全部记忆事实 (活跃状态)
func (m *Manager) ListActiveFacts(ctx context.Context, platform, userID string) ([]*core.Fact, error) {
	return m.listFacts(ctx, platform, userID, "", "")
}

// ListVisibleFacts 私聊只返回当前用户的全局事实；群聊只返回当前用户在本群记录的个人事实，避免成员画像互相泄漏。
func (m *Manager) ListVisibleFacts(ctx context.Context, platform, userID, groupID string, isGroup bool) ([]*core.Fact, error) {
	if isGroup {
		if groupID == "" {
			return nil, nil
		}
		return m.listFacts(ctx, platform, userID, `AND scope_type = 'group' AND scope_id = ?`, groupID)
	}
	return m.listFacts(ctx, platform, userID, `AND scope_type = 'global'`, "")
}

// PageVisibleFacts returns one page of confirmed facts, plus total confirmed
// and source-unknown counts, without loading the full memory list into memory.
func (m *Manager) PageVisibleFacts(ctx context.Context, platform, userID, groupID string, isGroup bool, page, pageSize int) ([]*core.Fact, int, int, error) {
	if page < 1 || pageSize < 1 || pageSize > 100 {
		return nil, 0, 0, fmt.Errorf("invalid visible fact page %d with page size %d", page, pageSize)
	}
	maxInt := int(^uint(0) >> 1)
	if page-1 > maxInt/pageSize {
		return nil, 0, 0, fmt.Errorf("visible fact page %d is too large", page)
	}
	scopeFilter := `AND scope_type = 'global'`
	args := []any{NormPlatform(platform), userID}
	if isGroup {
		if strings.TrimSpace(groupID) == "" {
			return nil, 0, 0, nil
		}
		scopeFilter = `AND scope_type = 'group' AND scope_id = ?`
		args = append(args, groupID)
	}
	database, err := m.GetConn(platform)
	if err != nil {
		return nil, 0, 0, err
	}
	var totalConfirmed, totalUnknown int
	countQuery := `SELECT COUNT(*) FROM user_facts WHERE platform = ? AND user_id = ? AND status = 'active' ` + scopeFilter
	if err := database.QueryRowContext(ctx, countQuery+` AND scope_unknown = 0`, args...).Scan(&totalConfirmed); err != nil {
		return nil, 0, 0, err
	}
	if err := database.QueryRowContext(ctx, countQuery+` AND scope_unknown = 1`, args...).Scan(&totalUnknown); err != nil {
		return nil, 0, 0, err
	}
	maxPage := totalConfirmed / pageSize
	if totalConfirmed%pageSize != 0 {
		maxPage++
	}
	if maxPage == 0 {
		maxPage = 1
	}
	if page > maxPage {
		return []*core.Fact{}, totalConfirmed, totalUnknown, nil
	}
	query := fmt.Sprintf(`
		SELECT id, user_id, scope_type, scope_id, substr(fact, 1, %d), updated_at, embedding, scope_unknown
		FROM user_facts
		WHERE platform = ? AND user_id = ? AND status = 'active' %s AND scope_unknown = 0
		ORDER BY updated_at DESC, id DESC LIMIT ? OFFSET ?`, core.MaxFactContentRunes+1, scopeFilter)
	pageArgs := append(append([]any(nil), args...), pageSize, (page-1)*pageSize)
	rows, err := database.QueryContext(ctx, query, pageArgs...)
	if err != nil {
		return nil, 0, 0, err
	}
	defer func() { _ = rows.Close() }()
	facts := make([]*core.Fact, 0, pageSize)
	for rows.Next() {
		var fact core.Fact
		var updatedAt int64
		var embedding []byte
		var scopeUnknown int
		if err := rows.Scan(&fact.ID, &fact.UserID, &fact.ScopeType, &fact.ScopeID, &fact.Content, &updatedAt, &embedding, &scopeUnknown); err != nil {
			return nil, 0, 0, err
		}
		fact.ConvID = fact.ScopeID
		fact.UpdatedAt = time.UnixMilli(updatedAt)
		fact.Embedding = decodeFloat32Slice(embedding)
		fact.ScopeUnknown = scopeUnknown == 1
		facts = append(facts, &fact)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, err
	}
	return facts, totalConfirmed, totalUnknown, nil
}

// PageUnconfirmedVisibleFacts pages source-unknown facts for private review.
// Legacy global facts must not be exposed in group conversations.
func (m *Manager) PageUnconfirmedVisibleFacts(ctx context.Context, platform, userID string, page, pageSize int) ([]*core.Fact, int, error) {
	if page < 1 || pageSize < 1 || pageSize > 100 {
		return nil, 0, fmt.Errorf("invalid unconfirmed fact page %d with page size %d", page, pageSize)
	}
	maxInt := int(^uint(0) >> 1)
	if page-1 > maxInt/pageSize {
		return nil, 0, fmt.Errorf("unconfirmed fact page %d is too large", page)
	}
	database, err := m.GetConn(platform)
	if err != nil {
		return nil, 0, err
	}
	args := []any{NormPlatform(platform), userID}
	const where = `platform = ? AND user_id = ? AND scope_type = 'global' AND status = 'active' AND scope_unknown = 1`
	var total int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_facts WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	maxPage := total / pageSize
	if total%pageSize != 0 {
		maxPage++
	}
	if page > maxPage {
		return []*core.Fact{}, total, nil
	}
	query := fmt.Sprintf(`SELECT id, user_id, scope_type, scope_id, substr(fact, 1, %d), updated_at, embedding
		FROM user_facts WHERE %s ORDER BY updated_at DESC, id DESC LIMIT ? OFFSET ?`, core.MaxFactContentRunes+1, where)
	pageArgs := append(append([]any(nil), args...), pageSize, (page-1)*pageSize)
	rows, err := database.QueryContext(ctx, query, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	facts := make([]*core.Fact, 0, pageSize)
	for rows.Next() {
		var fact core.Fact
		var updatedAt int64
		var embedding []byte
		if err := rows.Scan(&fact.ID, &fact.UserID, &fact.ScopeType, &fact.ScopeID, &fact.Content, &updatedAt, &embedding); err != nil {
			return nil, 0, err
		}
		fact.ConvID = fact.ScopeID
		fact.UpdatedAt = time.UnixMilli(updatedAt)
		fact.Embedding = decodeFloat32Slice(embedding)
		fact.ScopeUnknown = true
		facts = append(facts, &fact)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return facts, total, nil
}

func (m *Manager) listFacts(ctx context.Context, platform, userID, scopeFilter, scopeID string) ([]*core.Fact, error) {
	p := NormPlatform(platform)
	db, err := m.GetConn(p)
	if err != nil {
		return nil, err
	}

	query := `
		SELECT id, user_id, scope_type, scope_id, substr(fact, 1, %d), updated_at, embedding, scope_unknown
		FROM user_facts 
		WHERE platform = ? AND user_id = ? AND status = 'active' ` + scopeFilter + `
		ORDER BY id ASC
	`
	query = fmt.Sprintf(query, core.MaxFactContentRunes+1)
	args := []any{p, userID}
	if scopeID != "" {
		args = append(args, scopeID)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var facts []*core.Fact
	for rows.Next() {
		var (
			id           int64
			uID          string
			scopeType    string
			scID         string
			factText     string
			updatedMs    int64
			embBlob      []byte
			scopeUnknown int
		)
		if err := rows.Scan(&id, &uID, &scopeType, &scID, &factText, &updatedMs, &embBlob, &scopeUnknown); err != nil {
			return nil, err
		}
		facts = append(facts, &core.Fact{
			ID:           id,
			UserID:       uID,
			ScopeType:    scopeType,
			ScopeID:      scID,
			ConvID:       scID,
			Content:      factText,
			Embedding:    decodeFloat32Slice(embBlob),
			UpdatedAt:    time.UnixMilli(updatedMs),
			ScopeUnknown: scopeUnknown == 1,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return facts, nil
}

// ConfirmLegacyFactScope confirms an older source-ambiguous global fact as intentional private memory.
func (m *Manager) ConfirmLegacyFactScope(ctx context.Context, platform, userID string, factID int64) error {
	if factID <= 0 || strings.TrimSpace(userID) == "" {
		return fmt.Errorf("user ID and valid fact ID are required")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE user_facts SET scope_unknown = 0, updated_at = ?
		WHERE id = ? AND platform = ? AND user_id = ? AND scope_type = 'global' AND scope_unknown = 1 AND status = 'active'`,
		time.Now().UnixMilli(), factID, NormPlatform(platform), userID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("legacy fact not found or already confirmed")
	}
	return nil
}

// FactMutation 长期记忆事实演进指令
type FactMutation struct {
	Action       string    `json:"action"`               // ADD, UPDATE 或 DELETE
	FactID       int64     `json:"fact_id,omitempty"`    // 当 UPDATE 或 DELETE 时指定的目标事实 ID
	Fact         string    `json:"fact,omitempty"`       // 新增或更新后的事实陈述
	ExpectedFact string    `json:"-"`                    // 非空时要求修改前事实仍与读取时一致
	ScopeType    string    `json:"scope_type,omitempty"` // global 或 group
	ScopeID      string    `json:"scope_id,omitempty"`   // 群号 (若是群专属事实)
	Quote        string    `json:"quote,omitempty"`      // 来源原话
	Embedding    []float32 `json:"-"`                    // 可选稠密语义向量
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
	defer func() { _ = tx.Rollback() }()

	nowMs := time.Now().UnixMilli()

	for _, mut := range mutations {
		action := strings.ToUpper(strings.TrimSpace(mut.Action))
		if action != "ADD" && action != "UPDATE" && action != "DELETE" {
			return fmt.Errorf("不支持的记忆操作 %q", mut.Action)
		}
		scopeType := strings.TrimSpace(mut.ScopeType)
		if scopeType == "" {
			scopeType = "global"
		}
		scopeID := strings.TrimSpace(mut.ScopeID)
		switch scopeType {
		case "global":
			scopeID = ""
		case "group":
			if scopeID == "" {
				return fmt.Errorf("群专属记忆必须提供群 ID")
			}
		default:
			return fmt.Errorf("不支持的记忆范围类型 %q", scopeType)
		}

		switch action {
		case "ADD":
			factText := strings.TrimSpace(mut.Fact)
			if factText == "" {
				return fmt.Errorf("新增记忆事实不能为空")
			}
			if utf8.RuneCountInString(factText) > core.MaxFactContentRunes {
				return fmt.Errorf("记忆事实不能超过 %d 个字符", core.MaxFactContentRunes)
			}
			var existingID int64
			err := tx.QueryRowContext(ctx,
				"SELECT id FROM user_facts WHERE platform = ? AND user_id = ? AND scope_type = ? AND scope_id = ? AND status = 'active' AND scope_unknown = 0 AND fact = ? LIMIT 1",
				p, userID, scopeType, scopeID, factText,
			).Scan(&existingID)
			if err == nil && existingID > 0 {
				continue
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			_, err = tx.ExecContext(ctx,
				"INSERT INTO user_facts (platform, user_id, scope_type, scope_id, fact, status, quote, created_at, updated_at, embedding) VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?, ?)",
				p, userID, scopeType, scopeID, factText, mut.Quote, nowMs, nowMs, encodeFloat32Slice(mut.Embedding),
			)
			if err != nil {
				return err
			}

		case "UPDATE":
			factText := strings.TrimSpace(mut.Fact)
			if factText == "" {
				return fmt.Errorf("更新记忆事实不能为空")
			}
			if utf8.RuneCountInString(factText) > core.MaxFactContentRunes {
				return fmt.Errorf("记忆事实不能超过 %d 个字符", core.MaxFactContentRunes)
			}
			if mut.FactID <= 0 {
				return fmt.Errorf("更新记忆事实必须提供有效 ID")
			}
			var duplicateID int64
			err := tx.QueryRowContext(ctx,
				"SELECT id FROM user_facts WHERE platform = ? AND user_id = ? AND scope_type = ? AND scope_id = ? AND status = 'active' AND scope_unknown = 0 AND fact = ? AND id <> ? LIMIT 1",
				p, userID, scopeType, scopeID, factText, mut.FactID,
			).Scan(&duplicateID)
			if err == nil {
				return fmt.Errorf("更新后的记忆与同范围生效事实 ID %d 重复", duplicateID)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			query := "UPDATE user_facts SET fact = ?, quote = ?, updated_at = ?, embedding = ? WHERE id = ? AND platform = ? AND user_id = ? AND scope_type = ? AND scope_id = ? AND status = 'active' AND scope_unknown = 0"
			args := []any{factText, mut.Quote, nowMs, encodeFloat32Slice(mut.Embedding), mut.FactID, p, userID, scopeType, scopeID}
			if mut.ExpectedFact != "" {
				query += " AND fact = ?"
				args = append(args, mut.ExpectedFact)
			}
			res, err := tx.ExecContext(ctx, query, args...)
			if err != nil {
				return err
			}
			if affected, _ := res.RowsAffected(); affected == 0 {
				if mut.ExpectedFact != "" {
					return fmt.Errorf("ID 为 %d 的记忆事实已变化或不再生效，拒绝覆盖旧版本", mut.FactID)
				}
				return fmt.Errorf("未找到 ID 为 %d 的生效记忆事实", mut.FactID)
			}

		case "DELETE":
			if mut.FactID <= 0 {
				return fmt.Errorf("删除记忆事实必须提供有效 ID")
			}
			query := "UPDATE user_facts SET status = 'deleted', updated_at = ? WHERE id = ? AND platform = ? AND user_id = ? AND scope_type = ? AND scope_id = ? AND status = 'active'"
			args := []any{nowMs, mut.FactID, p, userID, scopeType, scopeID}
			if mut.ExpectedFact != "" {
				query += " AND fact = ?"
				args = append(args, mut.ExpectedFact)
			}
			res, err := tx.ExecContext(ctx, query, args...)
			if err != nil {
				return err
			}
			if affected, _ := res.RowsAffected(); affected == 0 {
				if mut.ExpectedFact != "" {
					return fmt.Errorf("ID 为 %d 的记忆事实已变化或不再生效，拒绝删除新版本", mut.FactID)
				}
				return fmt.Errorf("未找到 ID 为 %d 的生效记忆事实", mut.FactID)
			}
		}
	}

	return tx.Commit()
}

// SaveMsgCache 保存平台消息 ID 到原文的映射 (用于引用回复反查)

func (m *Manager) SaveMsgCache(ctx context.Context, platform, msgID, content, chatType, convID string) error {
	if msgID == "" || content == "" {
		return nil
	}
	if (chatType != "private" && chatType != "group") || strings.TrimSpace(convID) == "" {
		return fmt.Errorf("message cache requires a valid chat type and conversation ID")
	}
	content = core.TruncateRunes(content, core.MaxUserMessageRunes, "…[消息已截断]")
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	nowMs := time.Now().UnixMilli()
	_, err = db.ExecContext(ctx,
		`INSERT INTO msg_cache (msg_id, content, chat_type, conv_id, updated_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(msg_id) DO UPDATE SET content = excluded.content, chat_type = excluded.chat_type, conv_id = excluded.conv_id, updated_at = excluded.updated_at`,
		msgID, content, chatType, convID, nowMs,
	)
	if err != nil {
		return err
	}
	return nil
}

// DeleteMsgCache 删除指定的平台消息缓存条目。
func (m *Manager) DeleteMsgCache(platform, msgID string) error {
	if msgID == "" {
		return nil
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	_, err = db.Exec("DELETE FROM msg_cache WHERE msg_id = ?", msgID)
	return err
}

// MsgCacheEntry 消息缓存条目 (保持时间升序)
type MsgCacheEntry struct {
	MsgID    string
	Content  string
	ChatType string
	ConvID   string
}

// LoadMsgCache 按时间升序加载最近 limit 条平台消息缓存到内存；更早的持久化记录保留，仍可通过 GetMsgCache 按 ID 查询。
func (m *Manager) LoadMsgCache(platform string, limit int) ([]MsgCacheEntry, error) {
	if limit <= 0 {
		limit = 1000
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT msg_id, substr(content, 1, ?), chat_type, conv_id FROM msg_cache
		WHERE chat_type IN ('private', 'group') AND conv_id <> ''
		AND msg_id IN (SELECT msg_id FROM msg_cache WHERE chat_type IN ('private', 'group') AND conv_id <> '' ORDER BY updated_at DESC, rowid DESC LIMIT ?)
		ORDER BY updated_at ASC`, core.MaxUserMessageRunes+1, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var res []MsgCacheEntry
	for rows.Next() {
		var id, content, chatType, convID string
		if err := rows.Scan(&id, &content, &chatType, &convID); err != nil {
			return nil, err
		}
		res = append(res, MsgCacheEntry{MsgID: id, Content: core.TruncateRunes(content, core.MaxUserMessageRunes, "…[消息已截断]"), ChatType: chatType, ConvID: convID})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// GetMsgCache reads a durable reference body when it is no longer in the bounded in-memory cache.
func (m *Manager) GetMsgCache(ctx context.Context, platform, msgID, chatType, convID string) (string, error) {
	if msgID == "" || (chatType != "private" && chatType != "group") || strings.TrimSpace(convID) == "" {
		return "", nil
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return "", err
	}
	var content string
	err = db.QueryRowContext(ctx, "SELECT substr(content, 1, ?) FROM msg_cache WHERE msg_id = ? AND chat_type = ? AND conv_id = ?", core.MaxUserMessageRunes+1, msgID, chatType, convID).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return core.TruncateRunes(content, core.MaxUserMessageRunes, "…[消息已截断]"), err
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

// ResetSessionHistory 记录指定会话的历史切断游标；近期历史、关键词检索和向量召回都会忽略游标之前的消息。
func (m *Manager) ResetSessionHistory(platform, convID string) error {
	if convID == "" {
		return fmt.Errorf("convID cannot be empty")
	}
	return m.resetSessionHistory(platform, convID, "", convID)
}

func (m *Manager) resetSessionHistory(platform, resetKey, chatType, convID string) error {
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	query := "SELECT COALESCE(MAX(id), 0) FROM messages WHERE conv_id = ?"
	args := []any{convID}
	if chatType != "" {
		query = "SELECT COALESCE(MAX(id), 0) FROM messages WHERE chat_type = ? AND conv_id = ?"
		args = []any{chatType, convID}
	}
	var resetID int64
	if err := tx.QueryRow(query, args...).Scan(&resetID); err != nil {
		return err
	}

	nowMs := time.Now().UnixMilli()
	if _, err := tx.Exec(
		`INSERT INTO session_resets (conv_id, reset_at, reset_id) VALUES (?, ?, ?)
		 ON CONFLICT(conv_id) DO UPDATE SET reset_at = excluded.reset_at, reset_id = excluded.reset_id`,
		resetKey, nowMs, resetID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetSessionHistoryInChat resets only the requested conversation type.
func (m *Manager) ResetSessionHistoryInChat(platform, chatType, convID string) error {
	if chatType != "private" && chatType != "group" {
		return fmt.Errorf("chatType must be private or group")
	}
	if convID == "" {
		return fmt.Errorf("convID cannot be empty")
	}
	return m.resetSessionHistory(platform, sessionResetKey(chatType, convID), chatType, convID)
}

func sessionResetKey(chatType, convID string) string {
	return chatType + ":" + convID
}
