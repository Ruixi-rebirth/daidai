package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var ErrAccessInviteConflict = errors.New("access invite ID or code already exists")

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
	if err != nil {
		var sqliteErr *sqlite.Error
		if errors.As(err, &sqliteErr) && (sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY) {
			return fmt.Errorf("%w: %v", ErrAccessInviteConflict, err)
		}
	}
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
	defer func() { _ = rows.Close() }()
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
