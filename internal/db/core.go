package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
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
CREATE TABLE IF NOT EXISTS messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    platform     TEXT NOT NULL CHECK (length(platform) > 0),
    chat_type    TEXT NOT NULL DEFAULT 'private' CHECK (chat_type IN ('private', 'group')),
    conv_id      TEXT NOT NULL CHECK (length(conv_id) > 0),
    user_id      TEXT NOT NULL,                   -- 纯净用户ID (纯QQ号或OpenID)
    name         TEXT NOT NULL,
    text         TEXT NOT NULL,
    is_bot       INTEGER NOT NULL DEFAULT 0 CHECK (is_bot IN (0, 1)),
    created_at   INTEGER NOT NULL,                -- 毫秒级时间戳
    reply_to_uid TEXT NOT NULL DEFAULT '',
    reply_to_message_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
    platform_msg_id TEXT NOT NULL DEFAULT '',
    embedding    BLOB,
    embedding_model TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_messages_lookup ON messages(platform, chat_type, conv_id, id);
CREATE INDEX IF NOT EXISTS idx_messages_time ON messages(platform, chat_type, conv_id, created_at, id);
CREATE INDEX IF NOT EXISTS idx_messages_reset ON messages(conv_id, id);
CREATE INDEX IF NOT EXISTS idx_messages_reply_to_message ON messages(reply_to_message_id) WHERE reply_to_message_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_inbound_event ON messages(platform, chat_type, conv_id, platform_msg_id)
    WHERE is_bot = 0 AND platform_msg_id <> '';
CREATE INDEX IF NOT EXISTS idx_messages_reply_lookup ON messages(platform, chat_type, conv_id, user_id, is_bot, id DESC);
CREATE INDEX IF NOT EXISTS idx_messages_vector_lookup ON messages(platform, chat_type, conv_id, embedding_model, id DESC) WHERE embedding IS NOT NULL;

CREATE TABLE IF NOT EXISTS user_facts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    platform     TEXT NOT NULL CHECK (length(platform) > 0),
    user_id      TEXT NOT NULL CHECK (length(user_id) > 0),
    scope_type   TEXT NOT NULL DEFAULT 'global' CHECK (scope_type IN ('global', 'group')),
    scope_id     TEXT NOT NULL DEFAULT '',
    fact         TEXT NOT NULL CHECK (length(trim(fact)) > 0),
    status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'superseded', 'deleted')),
    quote        TEXT NOT NULL DEFAULT '',        -- 来源用户原话
    created_at   INTEGER NOT NULL,                -- 毫秒级时间戳
    updated_at   INTEGER NOT NULL,
    embedding    BLOB,
    embedding_model TEXT NOT NULL DEFAULT '',
    CHECK ((scope_type = 'global' AND scope_id = '') OR (scope_type = 'group' AND length(scope_id) > 0))
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_facts_active_unique ON user_facts(platform, user_id, scope_type, scope_id, fact) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_facts_page ON user_facts(platform, user_id, scope_type, scope_id, updated_at DESC, id DESC) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_facts_embedding_model ON user_facts(platform, user_id, embedding_model) WHERE embedding IS NOT NULL AND status = 'active';

CREATE TABLE IF NOT EXISTS user_fact_sources (
    fact_id     INTEGER NOT NULL,
    message_id  INTEGER NOT NULL,
    linked_at   INTEGER NOT NULL,
    PRIMARY KEY (fact_id, message_id),
    FOREIGN KEY (fact_id) REFERENCES user_facts(id) ON DELETE CASCADE,
    FOREIGN KEY (message_id) REFERENCES messages(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_fact_sources_message ON user_fact_sources(message_id, fact_id);

CREATE TABLE IF NOT EXISTS qq_quote_records (
    msg_id        TEXT NOT NULL,
    chat_type     TEXT NOT NULL CHECK (chat_type IN ('private', 'group')),
    conv_id       TEXT NOT NULL CHECK (length(conv_id) > 0),
    direction     TEXT NOT NULL CHECK (direction IN ('inbound', 'outbound', 'legacy')),
    content       TEXT NOT NULL DEFAULT '',
    content_kind  TEXT NOT NULL CHECK (content_kind IN ('text', 'text_card', 'mixed_media', 'image', 'file', 'unknown')),
    text_complete INTEGER NOT NULL CHECK (text_complete IN (0, 1)),
    updated_at    INTEGER NOT NULL,
    PRIMARY KEY (msg_id, chat_type, conv_id)
);
CREATE INDEX IF NOT EXISTS idx_qq_quote_records_time ON qq_quote_records(updated_at);

CREATE TABLE IF NOT EXISTS channel_workspaces (
    channel_id     TEXT NOT NULL CHECK (length(channel_id) > 0),
    workspace_path TEXT NOT NULL CHECK (length(workspace_path) > 0),
    project_name   TEXT NOT NULL,
    is_current     INTEGER NOT NULL DEFAULT 0 CHECK (is_current IN (0, 1)),
    last_active_at INTEGER NOT NULL,
    PRIMARY KEY (channel_id, workspace_path)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_workspace_one_current ON channel_workspaces(channel_id) WHERE is_current = 1;

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
    category     TEXT NOT NULL CHECK (length(category) > 0),
    body         TEXT NOT NULL CHECK (length(body) > 0),
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
    status          TEXT NOT NULL CHECK (status IN ('pending', 'running', 'stopping', 'completed', 'failed', 'aborted', 'interrupted')),
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

CREATE TABLE IF NOT EXISTS user_roles (
    platform     TEXT NOT NULL,
    user_id      TEXT NOT NULL CHECK (length(user_id) > 0),
    role         TEXT NOT NULL CHECK (role IN ('friend', 'collaborator')),
    granted_by   TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (platform, user_id)
);
CREATE INDEX IF NOT EXISTS idx_user_roles_role ON user_roles(platform, role, updated_at DESC);

CREATE TABLE IF NOT EXISTS private_access_requests (
    platform        TEXT NOT NULL,
    user_id         TEXT NOT NULL CHECK (length(user_id) > 0),
    display_name    TEXT NOT NULL DEFAULT '',
    last_message    TEXT NOT NULL DEFAULT '',
    first_seen_at   INTEGER NOT NULL,
    last_seen_at    INTEGER NOT NULL,
    request_count   INTEGER NOT NULL DEFAULT 1 CHECK (request_count > 0),
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'revoked')),
    reviewed_by     TEXT NOT NULL DEFAULT '',
    reviewed_at     INTEGER,
    PRIMARY KEY (platform, user_id)
);
CREATE INDEX IF NOT EXISTS idx_private_access_pending ON private_access_requests(platform, status, last_seen_at DESC);

CREATE TABLE IF NOT EXISTS private_access_invites (
    id            TEXT PRIMARY KEY,
    platform      TEXT NOT NULL,
    code_hash     TEXT NOT NULL UNIQUE,
    role          TEXT NOT NULL CHECK (role IN ('friend', 'collaborator')),
    created_by    TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL,
    max_uses      INTEGER NOT NULL CHECK (max_uses > 0),
    used_count    INTEGER NOT NULL DEFAULT 0 CHECK (used_count >= 0),
    revoked_at    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_private_access_invites_active ON private_access_invites(platform, expires_at DESC, created_at DESC);
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
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s failed: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema for %s failed: %w", p, err)
	}
	if err := os.Chmod(dbPath, 0600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure database file permissions for %s failed: %w", p, err)
	}

	m.conns[p] = db
	return db, nil
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

type UserRole struct {
	Platform    string
	UserID      string
	DisplayName string
	Role        string
	GrantedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type PrivateAccessRequest struct {
	UserID       string
	DisplayName  string
	LastMessage  string
	FirstSeenAt  time.Time
	LastSeenAt   time.Time
	RequestCount int
}

type PrivateAccessInvite struct {
	ID        string
	Role      string
	CreatedAt time.Time
	ExpiresAt time.Time
	MaxUses   int
	UsedCount int
	RevokedAt *time.Time
}

type InviteRedemption struct {
	InviteID      string
	Role          string
	EffectiveRole string
	UsesRemaining int
}

func (m *Manager) SetUserRole(ctx context.Context, platform, userID, role, grantedBy string) error {
	platform = NormPlatform(platform)
	userID = strings.TrimSpace(userID)
	role = strings.TrimSpace(role)
	if platform == "" || userID == "" || (role != "friend" && role != "collaborator") {
		return fmt.Errorf("platform, user ID, and valid role ('friend' or 'collaborator') are required")
	}
	conn, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO user_roles (platform, user_id, role, granted_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(platform, user_id) DO UPDATE SET role = excluded.role, granted_by = excluded.granted_by, updated_at = excluded.updated_at`,
		platform, userID, role, strings.TrimSpace(grantedBy), now, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE private_access_requests SET status = 'approved', reviewed_by = ?, reviewed_at = ?
		WHERE platform = ? AND user_id = ?`, strings.TrimSpace(grantedBy), now, platform, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Manager) GetUserRole(ctx context.Context, platform, userID string) (*UserRole, error) {
	platform = NormPlatform(platform)
	userID = strings.TrimSpace(userID)
	if platform == "" || userID == "" {
		return nil, fmt.Errorf("platform and user ID are required")
	}
	conn, err := m.GetConn(platform)
	if err != nil {
		return nil, err
	}
	var role UserRole
	var createdAt, updatedAt int64
	var displayName sql.NullString
	err = conn.QueryRowContext(ctx, `SELECT u.platform, u.user_id, u.role, u.granted_by, u.created_at, u.updated_at, p.display_name
		FROM user_roles u
		LEFT JOIN private_access_requests p ON u.platform = p.platform AND u.user_id = p.user_id
		WHERE u.platform = ? AND u.user_id = ?`, platform, userID).
		Scan(&role.Platform, &role.UserID, &role.Role, &role.GrantedBy, &createdAt, &updatedAt, &displayName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	role.DisplayName = displayName.String
	role.CreatedAt = time.UnixMilli(createdAt)
	role.UpdatedAt = time.UnixMilli(updatedAt)
	return &role, nil
}

func (m *Manager) GrantCollaborator(ctx context.Context, platform, userID, grantedBy string) error {
	return m.GrantUserRole(ctx, platform, userID, "collaborator", grantedBy)
}

func (m *Manager) GrantUserRole(ctx context.Context, platform, userID, role, grantedBy string) error {
	platform = NormPlatform(platform)
	userID = strings.TrimSpace(userID)
	role = strings.TrimSpace(role)
	if platform == "" || userID == "" || (role != "friend" && role != "collaborator") {
		return fmt.Errorf("platform, user ID, and valid role are required")
	}
	conn, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO user_roles (platform, user_id, role, granted_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(platform, user_id) DO UPDATE SET
		role = CASE WHEN user_roles.role = 'collaborator' OR excluded.role = 'collaborator' THEN 'collaborator' ELSE 'friend' END,
		granted_by = excluded.granted_by, updated_at = excluded.updated_at`,
		platform, userID, role, strings.TrimSpace(grantedBy), now, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE private_access_requests SET status = 'approved', reviewed_by = ?, reviewed_at = ?
		WHERE platform = ? AND user_id = ?`, strings.TrimSpace(grantedBy), now, platform, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Manager) RevokeCollaborator(ctx context.Context, platform, userID, reviewer string) error {
	platform = NormPlatform(platform)
	userID = strings.TrimSpace(userID)
	if platform == "" || userID == "" {
		return fmt.Errorf("platform and user ID are required")
	}
	conn, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "DELETE FROM user_roles WHERE platform = ? AND user_id = ?", platform, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE private_access_requests SET status = 'revoked', reviewed_by = ?, reviewed_at = ?
		WHERE platform = ? AND user_id = ?`, strings.TrimSpace(reviewer), time.Now().UnixMilli(), platform, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Manager) ListUserRoles(ctx context.Context, platform string) ([]UserRole, error) {
	conn, err := m.GetConn(platform)
	if err != nil {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT u.platform, u.user_id, u.role, u.granted_by, u.created_at, u.updated_at, COALESCE(p.display_name, '')
		FROM user_roles u
		LEFT JOIN private_access_requests p ON u.platform = p.platform AND u.user_id = p.user_id
		WHERE u.platform = ? ORDER BY u.updated_at DESC, u.user_id`, NormPlatform(platform))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roles []UserRole
	for rows.Next() {
		var role UserRole
		var createdAt, updatedAt int64
		if err := rows.Scan(&role.Platform, &role.UserID, &role.Role, &role.GrantedBy, &createdAt, &updatedAt, &role.DisplayName); err != nil {
			return nil, err
		}
		role.CreatedAt = time.UnixMilli(createdAt)
		role.UpdatedAt = time.UnixMilli(updatedAt)
		roles = append(roles, role)
	}
	return roles, rows.Err()
}

func (m *Manager) RecordPrivateAccessRequest(ctx context.Context, platform, userID, displayName, message string) (bool, error) {
	platform = NormPlatform(platform)
	userID = strings.TrimSpace(userID)
	if platform == "" || userID == "" {
		return false, fmt.Errorf("platform and user ID are required")
	}
	conn, err := m.GetConn(platform)
	if err != nil {
		return false, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var previousStatus string
	err = tx.QueryRowContext(ctx, "SELECT status FROM private_access_requests WHERE platform = ? AND user_id = ?", platform, userID).Scan(&previousStatus)
	newPendingRequest := errors.Is(err, sql.ErrNoRows) || previousStatus != "pending"
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	now := time.Now().UnixMilli()
	_, err = tx.ExecContext(ctx, `INSERT INTO private_access_requests
		(platform, user_id, display_name, last_message, first_seen_at, last_seen_at, request_count, status)
		VALUES (?, ?, ?, ?, ?, ?, 1, 'pending')
		ON CONFLICT(platform, user_id) DO UPDATE SET
		display_name = excluded.display_name,
		last_message = excluded.last_message,
		last_seen_at = excluded.last_seen_at,
		request_count = private_access_requests.request_count + 1,
		first_seen_at = CASE WHEN private_access_requests.status = 'pending' THEN private_access_requests.first_seen_at ELSE excluded.first_seen_at END,
		status = 'pending', reviewed_by = '', reviewed_at = NULL`,
		platform, userID, strings.TrimSpace(displayName), message, now, now)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return newPendingRequest, nil
}

func (m *Manager) ListPendingPrivateAccessRequests(ctx context.Context, platform string, limit int) ([]PrivateAccessRequest, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	conn, err := m.GetConn(platform)
	if err != nil {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT user_id, display_name, last_message, first_seen_at, last_seen_at, request_count
		FROM private_access_requests WHERE platform = ? AND status = 'pending'
		ORDER BY last_seen_at DESC LIMIT ?`, NormPlatform(platform), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var requests []PrivateAccessRequest
	for rows.Next() {
		var request PrivateAccessRequest
		var firstSeenAt, lastSeenAt int64
		if err := rows.Scan(&request.UserID, &request.DisplayName, &request.LastMessage, &firstSeenAt, &lastSeenAt, &request.RequestCount); err != nil {
			return nil, err
		}
		request.FirstSeenAt = time.UnixMilli(firstSeenAt)
		request.LastSeenAt = time.UnixMilli(lastSeenAt)
		requests = append(requests, request)
	}
	return requests, rows.Err()
}

func (m *Manager) BootstrapConfiguredCollaborators(ctx context.Context, platform string, collaborators []string) error {
	key := "configured_collaborators_imported_v1"
	imported, err := m.GetRuntimeState(ctx, platform, key)
	if err != nil || imported != "" {
		return err
	}
	for _, userID := range collaborators {
		if err := m.SetUserRole(ctx, platform, userID, "collaborator", "legacy-config"); err != nil {
			return err
		}
	}
	return m.SetRuntimeState(ctx, platform, key, "done")
}

func HashAccessInviteCode(code string) string {
	hash := sha256.Sum256([]byte(strings.ToUpper(strings.TrimSpace(code))))
	return hex.EncodeToString(hash[:])
}

func NormalizeAccessInviteCode(code string) (string, bool) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) != len("DAIDAI-")+12 || !strings.HasPrefix(code, "DAIDAI-") {
		return "", false
	}
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	for _, c := range code[len("DAIDAI-"):] {
		if !strings.ContainsRune(alphabet, c) {
			return "", false
		}
	}
	return code, true
}

func (m *Manager) CreatePrivateAccessInvite(ctx context.Context, platform, id, codeHash, role, createdBy string, expiresAt time.Time, maxUses int) error {
	platform = NormPlatform(platform)
	if platform == "" || strings.TrimSpace(id) == "" || len(codeHash) != 64 || (role != "friend" && role != "collaborator") || maxUses < 1 || maxUses > 100 || !expiresAt.After(time.Now()) {
		return fmt.Errorf("invalid private access invite")
	}
	conn, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO private_access_invites
		(id, platform, code_hash, role, created_by, created_at, expires_at, max_uses)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(id), platform, strings.ToLower(codeHash), role, strings.TrimSpace(createdBy), time.Now().UnixMilli(), expiresAt.UnixMilli(), maxUses)
	return err
}

func (m *Manager) ListPrivateAccessInvites(ctx context.Context, platform string, limit int) ([]PrivateAccessInvite, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	conn, err := m.GetConn(platform)
	if err != nil {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT id, role, created_at, expires_at, max_uses, used_count, revoked_at
		FROM private_access_invites WHERE platform = ? ORDER BY created_at DESC LIMIT ?`, NormPlatform(platform), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var invites []PrivateAccessInvite
	for rows.Next() {
		var invite PrivateAccessInvite
		var createdAt, expiresAt int64
		var revokedAt sql.NullInt64
		if err := rows.Scan(&invite.ID, &invite.Role, &createdAt, &expiresAt, &invite.MaxUses, &invite.UsedCount, &revokedAt); err != nil {
			return nil, err
		}
		invite.CreatedAt = time.UnixMilli(createdAt)
		invite.ExpiresAt = time.UnixMilli(expiresAt)
		if revokedAt.Valid {
			revoked := time.UnixMilli(revokedAt.Int64)
			invite.RevokedAt = &revoked
		}
		invites = append(invites, invite)
	}
	return invites, rows.Err()
}

func (m *Manager) RevokePrivateAccessInvite(ctx context.Context, platform, inviteID string) error {
	conn, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	result, err := conn.ExecContext(ctx, `UPDATE private_access_invites SET revoked_at = ?
		WHERE platform = ? AND id = ? AND revoked_at IS NULL`, time.Now().UnixMilli(), NormPlatform(platform), strings.TrimSpace(inviteID))
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (m *Manager) RedeemPrivateAccessInvite(ctx context.Context, platform, codeHash, userID string) (*InviteRedemption, error) {
	platform = NormPlatform(platform)
	userID = strings.TrimSpace(userID)
	if platform == "" || len(codeHash) != 64 || userID == "" {
		return nil, fmt.Errorf("invalid invite redemption")
	}
	conn, err := m.GetConn(platform)
	if err != nil {
		return nil, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var inviteID, role string
	var expiresAt int64
	var maxUses, usedCount int
	var revokedAt sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT id, role, expires_at, max_uses, used_count, revoked_at
		FROM private_access_invites WHERE platform = ? AND code_hash = ?`, platform, strings.ToLower(codeHash)).
		Scan(&inviteID, &role, &expiresAt, &maxUses, &usedCount, &revokedAt)
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	if revokedAt.Valid || expiresAt <= now || usedCount >= maxUses {
		return nil, sql.ErrNoRows
	}
	result, err := tx.ExecContext(ctx, `UPDATE private_access_invites SET used_count = used_count + 1
		WHERE platform = ? AND id = ? AND revoked_at IS NULL AND expires_at > ? AND used_count < max_uses`, platform, inviteID, now)
	if err != nil {
		return nil, err
	}
	updated, err := result.RowsAffected()
	if err != nil || updated != 1 {
		if err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO user_roles (platform, user_id, role, granted_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(platform, user_id) DO UPDATE SET
		role = CASE WHEN user_roles.role = 'collaborator' OR excluded.role = 'collaborator' THEN 'collaborator' ELSE 'friend' END,
		granted_by = excluded.granted_by, updated_at = excluded.updated_at`,
		platform, userID, role, "invite:"+inviteID, now, now); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE private_access_requests SET status = 'approved', reviewed_by = ?, reviewed_at = ?
		WHERE platform = ? AND user_id = ?`, "invite:"+inviteID, now, platform, userID); err != nil {
		return nil, err
	}
	var effectiveRole string
	if err := tx.QueryRowContext(ctx, "SELECT role FROM user_roles WHERE platform = ? AND user_id = ?", platform, userID).Scan(&effectiveRole); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &InviteRedemption{InviteID: inviteID, Role: role, EffectiveRole: effectiveRole, UsesRemaining: maxUses - usedCount - 1}, nil
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
	result, err := db.ExecContext(ctx, `INSERT INTO pending_notifications
		(platform, recipient_id, dedupe_key, category, body, created_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(platform, recipient_id, dedupe_key) DO NOTHING`,
		platform, recipientID, dedupeKey, category, body, time.Now().UnixMilli())
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return inserted > 0, nil
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

func (m *Manager) DeletePendingNotificationByKey(ctx context.Context, platform, recipientID, dedupeKey string) error {
	if strings.TrimSpace(recipientID) == "" || strings.TrimSpace(dedupeKey) == "" {
		return fmt.Errorf("notification recipient and dedupe key are required")
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `DELETE FROM pending_notifications WHERE platform=? AND recipient_id=? AND dedupe_key=?`,
		NormPlatform(platform), strings.TrimSpace(recipientID), strings.TrimSpace(dedupeKey))
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
	return m.logMessage(ctx, platform, chatType, convID, userID, name, text, isBot, replyToUID, platformMsgID, 0)
}

// LogBotReply records which exact inbound message a delivered bot reply answers.
func (m *Manager) LogBotReply(ctx context.Context, platform, chatType, convID, text, replyToUID string, replyToMessageID int64) (int64, error) {
	return m.logMessage(ctx, platform, chatType, convID, "daidai", "呆呆", text, true, replyToUID, "", replyToMessageID)
}

func (m *Manager) logMessage(ctx context.Context, platform, chatType, convID, userID, name, text string, isBot bool, replyToUID, platformMsgID string, replyToMessageID int64) (int64, error) {
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
		"INSERT INTO messages (platform, chat_type, conv_id, user_id, name, text, is_bot, created_at, reply_to_uid, reply_to_message_id, platform_msg_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		p, chatType, convID, userID, name, text, botVal, nowMs, replyToUID, nullableMessageID(replyToMessageID), platformMsgID,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func nullableMessageID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

// LogInboundMessage records one raw user event once, keyed by its platform
// message ID. It runs before debounce so merged turns cannot erase messages.
func (m *Manager) LogInboundMessage(ctx context.Context, platform, chatType, convID, userID, name, text, platformMsgID string) (int64, error) {
	if platformMsgID == "" {
		return m.LogMessage(ctx, platform, chatType, convID, userID, name, text, false, "", "")
	}
	text = core.TruncateRunes(text, core.MaxUserMessageRunes, "…[消息已截断]")
	p := NormPlatform(platform)
	database, err := m.GetConn(p)
	if err != nil {
		return 0, err
	}
	result, err := database.ExecContext(ctx, `
		INSERT INTO messages (platform, chat_type, conv_id, user_id, name, text, is_bot, created_at, reply_to_uid, platform_msg_id)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?, '', ?)
		ON CONFLICT DO NOTHING`, p, chatType, convID, userID, name, text, time.Now().UnixMilli(), platformMsgID)
	if err != nil {
		return 0, err
	}
	if inserted, err := result.RowsAffected(); err != nil {
		return 0, err
	} else if inserted > 0 {
		return result.LastInsertId()
	}
	var id int64
	err = database.QueryRowContext(ctx, `SELECT id FROM messages
		WHERE platform = ? AND chat_type = ? AND conv_id = ? AND platform_msg_id = ? AND is_bot = 0
		ORDER BY id DESC LIMIT 1`, p, chatType, convID, platformMsgID).Scan(&id)
	return id, err
}

// MessageTextByID loads one bounded history row for its asynchronous embedding.
func (m *Manager) MessageTextByID(ctx context.Context, platform string, id int64) (string, error) {
	if id <= 0 {
		return "", nil
	}
	database, err := m.GetConn(platform)
	if err != nil {
		return "", err
	}
	var text string
	err = database.QueryRowContext(ctx, `SELECT substr(text, 1, ?) FROM messages WHERE platform = ? AND id = ? AND is_bot = 0`, core.MaxUserMessageRunes+1, NormPlatform(platform), id).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return text, err
}

// AppendMessageText adds a derived note, such as a vision summary, to an
// already persisted inbound row without duplicating the note on retries.
func (m *Manager) AppendMessageText(ctx context.Context, platform string, id int64, addition string) (string, error) {
	if id <= 0 || strings.TrimSpace(addition) == "" {
		return m.MessageTextByID(ctx, platform, id)
	}
	database, err := m.GetConn(platform)
	if err != nil {
		return "", err
	}
	var current string
	if err := database.QueryRowContext(ctx, `SELECT text FROM messages WHERE platform = ? AND id = ? AND is_bot = 0`, NormPlatform(platform), id).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if !strings.Contains(current, addition) {
		separator := "\n"
		if current == "" {
			separator = ""
		}
		current = core.TruncateRunes(current+separator+addition, core.MaxUserMessageRunes, "\n[消息过长，后续内容已截断]")
		if _, err := database.ExecContext(ctx, `UPDATE messages SET text = ? WHERE platform = ? AND id = ? AND is_bot = 0`, current, NormPlatform(platform), id); err != nil {
			return "", err
		}
	}
	return current, nil
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
	ID             int64
	Platform       string
	IsGroup        bool
	ConvID         string
	UserID         string
	Name           string
	Text           string
	IsBot          bool
	Embedding      []float32
	EmbeddingModel string
	CreatedAt      time.Time
}

type FactSource struct {
	MessageID int64
	Text      string
	CreatedAt time.Time
	Excerpt   bool
}

// ListVisibleFactSources returns source messages only when the fact belongs to
// the user's currently visible scope. It also exposes the saved quote for facts
// created before message-level provenance was recorded.
func (m *Manager) ListVisibleFactSources(ctx context.Context, platform, userID, groupID string, isGroup bool, factID int64) ([]FactSource, error) {
	database, err := m.GetConn(platform)
	if err != nil {
		return nil, err
	}
	scopeType, scopeID := "global", ""
	if isGroup {
		if strings.TrimSpace(groupID) == "" {
			return nil, fmt.Errorf("group ID is required to read group fact sources")
		}
		scopeType, scopeID = "group", groupID
	}
	var quote string
	err = database.QueryRowContext(ctx, `SELECT quote FROM user_facts
		WHERE id = ? AND platform = ? AND user_id = ? AND scope_type = ? AND scope_id = ? AND status = 'active'`,
		factID, NormPlatform(platform), userID, scopeType, scopeID).Scan(&quote)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := database.QueryContext(ctx, `SELECT m.id, m.text, m.created_at
		FROM user_fact_sources s
		JOIN user_facts f ON f.id = s.fact_id
		JOIN messages m ON m.id = s.message_id AND m.platform = f.platform AND m.user_id = f.user_id AND m.is_bot = 0
			AND ((f.scope_type = 'global' AND m.chat_type = 'private') OR (f.scope_type = 'group' AND m.chat_type = 'group' AND m.conv_id = f.scope_id))
		WHERE s.fact_id = ? AND f.platform = ? AND f.user_id = ? AND f.scope_type = ? AND f.scope_id = ? AND f.status = 'active'
		ORDER BY s.linked_at DESC, m.id DESC LIMIT 20`, factID, NormPlatform(platform), userID, scopeType, scopeID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	sources := make([]FactSource, 0, 4)
	for rows.Next() {
		var source FactSource
		var createdAt int64
		if err := rows.Scan(&source.MessageID, &source.Text, &createdAt); err != nil {
			return nil, err
		}
		source.CreatedAt = time.UnixMilli(createdAt)
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(sources) == 0 && strings.TrimSpace(quote) != "" {
		sources = append(sources, FactSource{Text: quote, Excerpt: true})
	}
	return sources, nil
}

// UpdateMessageEmbedding 异步回填消息的稠密语义向量
func (m *Manager) UpdateMessageEmbedding(ctx context.Context, platform string, msgID int64, vec []float32, modelIdentity string) error {
	if msgID <= 0 || len(vec) == 0 || strings.TrimSpace(modelIdentity) == "" {
		return nil
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "UPDATE messages SET embedding = ?, embedding_model = ? WHERE id = ?", encodeFloat32Slice(vec), modelIdentity, msgID)
	return err
}

// UpdateFactEmbedding 异步回填长期事实的稠密语义向量
func (m *Manager) UpdateFactEmbedding(ctx context.Context, platform string, factID int64, expectedFact string, vec []float32, modelIdentity string) error {
	if factID <= 0 || len(vec) == 0 || strings.TrimSpace(modelIdentity) == "" {
		return nil
	}
	db, err := m.GetConn(platform)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "UPDATE user_facts SET embedding = ?, embedding_model = ? WHERE id = ? AND fact = ? AND status = 'active' AND (embedding IS NULL OR length(embedding) = 0 OR embedding_model <> ?)", encodeFloat32Slice(vec), modelIdentity, factID, expectedFact, modelIdentity)
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
		SELECT id, platform, chat_type, conv_id, user_id, name, substr(text, 1, %d), is_bot, created_at, embedding, embedding_model
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
			embModel  string
		)
		if err := rows.Scan(&id, &plat, &cType, &cID, &uID, &name, &text, &isBotInt, &createdMs, &embBlob, &embModel); err != nil {
			return nil, err
		}
		list = append(list, StoredMessage{
			ID:             id,
			Platform:       plat,
			IsGroup:        cType == "group",
			ConvID:         cID,
			UserID:         uID,
			Name:           name,
			Text:           core.TruncateRunes(text, core.MaxUserMessageRunes, "…[消息已截断]"),
			IsBot:          isBotInt == 1,
			Embedding:      decodeFloat32Slice(embBlob),
			EmbeddingModel: embModel,
			CreatedAt:      time.UnixMilli(createdMs),
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
func (m *Manager) RecentVectorMessages(ctx context.Context, platform, convID, modelIdentity string, limit int) ([]StoredMessage, error) {
	return m.recentVectorMessages(ctx, platform, "", convID, modelIdentity, limit)
}

// RecentVectorMessagesInChat restricts vector recall to the requested chat type.
func (m *Manager) RecentVectorMessagesInChat(ctx context.Context, platform, chatType, convID, modelIdentity string, limit int) ([]StoredMessage, error) {
	return m.recentVectorMessages(ctx, platform, chatType, convID, modelIdentity, limit)
}

func (m *Manager) recentVectorMessages(ctx context.Context, platform, chatType, convID, modelIdentity string, limit int) ([]StoredMessage, error) {
	if convID == "" || strings.TrimSpace(modelIdentity) == "" {
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
	queryScope += " AND embedding_model = ?"
	queryArgs = append(queryArgs[:len(queryArgs)-1], modelIdentity, limit)
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, platform, chat_type, conv_id, user_id, name, substr(text, 1, %d), is_bot, created_at, embedding, embedding_model
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
			embModel  string
		)
		if err := rows.Scan(&id, &plat, &cType, &cID, &uID, &name, &text, &isBotInt, &createdMs, &embBlob, &embModel); err != nil {
			return nil, err
		}
		list = append(list, StoredMessage{
			ID:             id,
			Platform:       plat,
			IsGroup:        cType == "group",
			ConvID:         cID,
			UserID:         uID,
			Name:           name,
			Text:           core.TruncateRunes(text, core.MaxUserMessageRunes, "…[消息已截断]"),
			IsBot:          isBotInt == 1,
			Embedding:      decodeFloat32Slice(embBlob),
			EmbeddingModel: embModel,
			CreatedAt:      time.UnixMilli(createdMs),
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

// PageVisibleFacts returns one page of facts visible in the current conversation.
func (m *Manager) PageVisibleFacts(ctx context.Context, platform, userID, groupID string, isGroup bool, page, pageSize int) ([]*core.Fact, int, error) {
	if page < 1 || pageSize < 1 || pageSize > 100 {
		return nil, 0, fmt.Errorf("invalid visible fact page %d with page size %d", page, pageSize)
	}
	maxInt := int(^uint(0) >> 1)
	if page-1 > maxInt/pageSize {
		return nil, 0, fmt.Errorf("visible fact page %d is too large", page)
	}
	scopeFilter := `AND scope_type = 'global'`
	args := []any{NormPlatform(platform), userID}
	if isGroup {
		if strings.TrimSpace(groupID) == "" {
			return nil, 0, nil
		}
		scopeFilter = `AND scope_type = 'group' AND scope_id = ?`
		args = append(args, groupID)
	}
	database, err := m.GetConn(platform)
	if err != nil {
		return nil, 0, err
	}
	var total int
	countQuery := `SELECT COUNT(*) FROM user_facts WHERE platform = ? AND user_id = ? AND status = 'active' ` + scopeFilter
	if err := database.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	maxPage := total / pageSize
	if total%pageSize != 0 {
		maxPage++
	}
	if maxPage == 0 {
		maxPage = 1
	}
	if page > maxPage {
		return []*core.Fact{}, total, nil
	}
	query := fmt.Sprintf(`
		SELECT id, user_id, scope_type, scope_id, substr(fact, 1, %d), updated_at, embedding, embedding_model
		FROM user_facts
		WHERE platform = ? AND user_id = ? AND status = 'active' %s
		ORDER BY updated_at DESC, id DESC LIMIT ? OFFSET ?`, core.MaxFactContentRunes+1, scopeFilter)
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
		if err := rows.Scan(&fact.ID, &fact.UserID, &fact.ScopeType, &fact.ScopeID, &fact.Content, &updatedAt, &embedding, &fact.EmbeddingModel); err != nil {
			return nil, 0, err
		}
		fact.ConvID = fact.ScopeID
		fact.UpdatedAt = time.UnixMilli(updatedAt)
		fact.Embedding = decodeFloat32Slice(embedding)
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
		SELECT id, user_id, scope_type, scope_id, substr(fact, 1, %d), updated_at, embedding, embedding_model
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
			id        int64
			uID       string
			scopeType string
			scID      string
			factText  string
			updatedMs int64
			embBlob   []byte
			embModel  string
		)
		if err := rows.Scan(&id, &uID, &scopeType, &scID, &factText, &updatedMs, &embBlob, &embModel); err != nil {
			return nil, err
		}
		facts = append(facts, &core.Fact{
			ID:             id,
			UserID:         uID,
			ScopeType:      scopeType,
			ScopeID:        scID,
			ConvID:         scID,
			Content:        factText,
			Embedding:      decodeFloat32Slice(embBlob),
			EmbeddingModel: embModel,
			UpdatedAt:      time.UnixMilli(updatedMs),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return facts, nil
}

// FactMutation 长期记忆事实演进指令
type FactMutation struct {
	Action          string    `json:"action"`               // ADD, UPDATE 或 DELETE
	FactID          int64     `json:"fact_id,omitempty"`    // 当 UPDATE 或 DELETE 时指定的目标事实 ID
	SourceMessageID int64     `json:"-"`                    // 支撑该事实的入站消息记录 ID
	EmbeddingModel  string    `json:"-"`                    // 生成向量的编码器配置哈希
	Fact            string    `json:"fact,omitempty"`       // 新增或更新后的事实陈述
	ExpectedFact    string    `json:"-"`                    // 非空时要求修改前事实仍与读取时一致
	ScopeType       string    `json:"scope_type,omitempty"` // global 或 group
	ScopeID         string    `json:"scope_id,omitempty"`   // 群号 (若是群专属事实)
	Quote           string    `json:"quote,omitempty"`      // 来源原话
	Embedding       []float32 `json:"-"`                    // 可选稠密语义向量
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
				"SELECT id FROM user_facts WHERE platform = ? AND user_id = ? AND scope_type = ? AND scope_id = ? AND status = 'active' AND fact = ? LIMIT 1",
				p, userID, scopeType, scopeID, factText,
			).Scan(&existingID)
			if err == nil && existingID > 0 {
				if err := insertFactSource(ctx, tx, existingID, mut.SourceMessageID, p, userID, scopeType, scopeID, nowMs); err != nil {
					return err
				}
				continue
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			result, err := tx.ExecContext(ctx,
				"INSERT INTO user_facts (platform, user_id, scope_type, scope_id, fact, status, quote, created_at, updated_at, embedding, embedding_model) VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?, ?, ?)",
				p, userID, scopeType, scopeID, factText, mut.Quote, nowMs, nowMs, encodeFloat32Slice(mut.Embedding), mut.EmbeddingModel,
			)
			if err != nil {
				return err
			}
			insertedFactID, err := result.LastInsertId()
			if err != nil {
				return err
			}
			if err := insertFactSource(ctx, tx, insertedFactID, mut.SourceMessageID, p, userID, scopeType, scopeID, nowMs); err != nil {
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
				"SELECT id FROM user_facts WHERE platform = ? AND user_id = ? AND scope_type = ? AND scope_id = ? AND status = 'active' AND fact = ? AND id <> ? LIMIT 1",
				p, userID, scopeType, scopeID, factText, mut.FactID,
			).Scan(&duplicateID)
			if err == nil {
				return fmt.Errorf("更新后的记忆与同范围生效事实 ID %d 重复", duplicateID)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			query := "UPDATE user_facts SET fact = ?, quote = ?, updated_at = ?, embedding = ?, embedding_model = ? WHERE id = ? AND platform = ? AND user_id = ? AND scope_type = ? AND scope_id = ? AND status = 'active'"
			args := []any{factText, mut.Quote, nowMs, encodeFloat32Slice(mut.Embedding), mut.EmbeddingModel, mut.FactID, p, userID, scopeType, scopeID}
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
			if mut.SourceMessageID > 0 {
				if _, err := tx.ExecContext(ctx, `DELETE FROM user_fact_sources WHERE fact_id = ?`, mut.FactID); err != nil {
					return err
				}
				if err := insertFactSource(ctx, tx, mut.FactID, mut.SourceMessageID, p, userID, scopeType, scopeID, nowMs); err != nil {
					return err
				}
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

func insertFactSource(ctx context.Context, tx *sql.Tx, factID, messageID int64, platform, userID, scopeType, scopeID string, linkedAt int64) error {
	if messageID <= 0 {
		return nil
	}
	chatType := "private"
	convID := ""
	if scopeType == "group" {
		chatType = "group"
		convID = scopeID
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO user_fact_sources (fact_id, message_id, linked_at)
		SELECT ?, id, ? FROM messages
		WHERE id = ? AND platform = ? AND user_id = ? AND chat_type = ? AND (chat_type = 'private' OR conv_id = ?) AND is_bot = 0
		ON CONFLICT(fact_id, message_id) DO NOTHING`,
		factID, linkedAt, messageID, platform, userID, chatType, convID)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM user_fact_sources WHERE fact_id = ? AND message_id = ?`, factID, messageID).Scan(&exists); err == nil {
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return fmt.Errorf("memory source message %d is missing or outside the fact scope", messageID)
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
	if err != nil && strings.Contains(err.Error(), "no such table: msg_cache") {
		return nil
	}
	return err
}

// QQQuoteRecord stores the semantic content available when a QQ message is
// quoted. It keeps rendered transport payloads separate from readable text.
type QQQuoteRecord struct {
	MsgID        string
	Content      string
	ChatType     string
	ConvID       string
	Direction    string
	ContentKind  string
	TextComplete bool
}

func (m *Manager) SaveQQQuoteRecord(ctx context.Context, record QQQuoteRecord) error {
	if record.MsgID == "" || (record.ChatType != "private" && record.ChatType != "group") || strings.TrimSpace(record.ConvID) == "" {
		return fmt.Errorf("QQ quote record requires message ID and valid conversation scope")
	}
	if record.Direction != "inbound" && record.Direction != "outbound" && record.Direction != "legacy" {
		return fmt.Errorf("invalid QQ quote direction %q", record.Direction)
	}
	switch record.ContentKind {
	case "text", "text_card", "mixed_media", "image", "file", "unknown":
	default:
		return fmt.Errorf("invalid QQ quote content kind %q", record.ContentKind)
	}
	if record.ContentKind == "image" || record.ContentKind == "file" {
		record.Content = ""
		record.TextComplete = false
	}
	if record.Content == "" {
		record.TextComplete = false
	}
	if len([]rune(record.Content)) > core.MaxUserMessageRunes {
		record.Content = core.TruncateRunes(record.Content, core.MaxUserMessageRunes, "…[引用正文已截断]")
		record.TextComplete = false
	}
	database, err := m.GetConn("qq")
	if err != nil {
		return err
	}
	_, err = database.ExecContext(ctx, `
		INSERT INTO qq_quote_records (msg_id, chat_type, conv_id, direction, content, content_kind, text_complete, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(msg_id, chat_type, conv_id) DO UPDATE SET
			direction = excluded.direction,
			content = excluded.content,
			content_kind = excluded.content_kind,
			text_complete = excluded.text_complete,
			updated_at = excluded.updated_at`,
		record.MsgID, record.ChatType, record.ConvID, record.Direction, record.Content, record.ContentKind, boolToSQLite(record.TextComplete), time.Now().UnixMilli())
	return err
}

func (m *Manager) LoadQQQuoteRecords(limit int) ([]QQQuoteRecord, error) {
	if limit <= 0 {
		limit = 1000
	}
	database, err := m.GetConn("qq")
	if err != nil {
		return nil, err
	}
	rows, err := database.Query(`
		SELECT msg_id, content, chat_type, conv_id, direction, content_kind, text_complete
		FROM qq_quote_records
		WHERE (msg_id, chat_type, conv_id) IN (
			SELECT msg_id, chat_type, conv_id FROM qq_quote_records ORDER BY updated_at DESC, rowid DESC LIMIT ?
		)
		ORDER BY updated_at ASC, rowid ASC`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []QQQuoteRecord
	for rows.Next() {
		var record QQQuoteRecord
		var complete int
		if err := rows.Scan(&record.MsgID, &record.Content, &record.ChatType, &record.ConvID, &record.Direction, &record.ContentKind, &complete); err != nil {
			return nil, err
		}
		record.TextComplete = complete == 1
		records = append(records, record)
	}
	return records, rows.Err()
}

func (m *Manager) GetQQQuoteRecord(ctx context.Context, msgID, chatType, convID string) (QQQuoteRecord, bool, error) {
	if msgID == "" || (chatType != "private" && chatType != "group") || strings.TrimSpace(convID) == "" {
		return QQQuoteRecord{}, false, nil
	}
	database, err := m.GetConn("qq")
	if err != nil {
		return QQQuoteRecord{}, false, err
	}
	var record QQQuoteRecord
	var complete int
	err = database.QueryRowContext(ctx, `
		SELECT msg_id, content, chat_type, conv_id, direction, content_kind, text_complete
		FROM qq_quote_records WHERE msg_id = ? AND chat_type = ? AND conv_id = ?`,
		msgID, chatType, convID).Scan(&record.MsgID, &record.Content, &record.ChatType, &record.ConvID, &record.Direction, &record.ContentKind, &complete)
	if errors.Is(err, sql.ErrNoRows) {
		return QQQuoteRecord{}, false, nil
	}
	if err != nil {
		return QQQuoteRecord{}, false, err
	}
	record.TextComplete = complete == 1
	return record, true, nil
}

func (m *Manager) DeleteQQQuoteRecord(msgID, chatType, convID string) error {
	if msgID == "" || (chatType != "private" && chatType != "group") || strings.TrimSpace(convID) == "" {
		return nil
	}
	database, err := m.GetConn("qq")
	if err != nil {
		return err
	}
	_, err = database.Exec(`DELETE FROM qq_quote_records WHERE msg_id = ? AND chat_type = ? AND conv_id = ?`, msgID, chatType, convID)
	return err
}

func boolToSQLite(value bool) int {
	if value {
		return 1
	}
	return 0
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
	if err != nil && strings.Contains(err.Error(), "no such table: msg_cache") {
		return "", nil
	}
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
