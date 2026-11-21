package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

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
	db, err := OpenSQLite(dbPath, schema)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`UPDATE pending_notifications SET delivery_state = ? WHERE delivery_state = ?`,
		PendingNotificationStateUncertain, PendingNotificationStateSending); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("recover interrupted pending notifications: %w", err)
	}
	m.conns[p] = db
	return db, nil
}

// OpenSQLite opens a private SQLite store with the caller's schema. Shared
// application stores do not need to impersonate a messaging platform.
func OpenSQLite(dbPath, schema string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s failed: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema failed: %w", err)
	}
	if err := os.Chmod(dbPath, 0600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure database file permissions failed: %w", err)
	}

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
