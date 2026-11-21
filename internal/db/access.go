package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

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
	_, err := m.GrantUserRole(ctx, platform, userID, "collaborator", grantedBy)
	return err
}

func (m *Manager) GrantUserRole(ctx context.Context, platform, userID, role, grantedBy string) (string, error) {
	platform = NormPlatform(platform)
	userID = strings.TrimSpace(userID)
	role = strings.TrimSpace(role)
	if platform == "" || userID == "" || (role != "friend" && role != "collaborator") {
		return "", fmt.Errorf("platform, user ID, and valid role are required")
	}
	conn, err := m.GetConn(platform)
	if err != nil {
		return "", err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	var effectiveRole string
	if err := tx.QueryRowContext(ctx, `INSERT INTO user_roles (platform, user_id, role, granted_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(platform, user_id) DO UPDATE SET
		role = CASE WHEN user_roles.role = 'collaborator' OR excluded.role = 'collaborator' THEN 'collaborator' ELSE 'friend' END,
		granted_by = excluded.granted_by, updated_at = excluded.updated_at RETURNING role`,
		platform, userID, role, strings.TrimSpace(grantedBy), now, now).Scan(&effectiveRole); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE private_access_requests SET status = 'approved', reviewed_by = ?, reviewed_at = ?
		WHERE platform = ? AND user_id = ?`, strings.TrimSpace(grantedBy), now, platform, userID); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return effectiveRole, nil
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
	defer func() { _ = rows.Close() }()
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
	defer func() { _ = rows.Close() }()
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
