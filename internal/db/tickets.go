package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

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
		ON CONFLICT(id) DO UPDATE SET
			worker=CASE WHEN excluded.worker<>'' THEN excluded.worker ELSE agent_tickets.worker END,
			model=CASE WHEN excluded.model<>'' THEN excluded.model ELSE agent_tickets.model END,
			status=excluded.status,
			output=excluded.output,
			updated_at=excluded.updated_at
		WHERE excluded.updated_at > agent_tickets.updated_at`, t.ID, t.RequesterID, t.ChannelID, t.Goal,
		t.Worker, t.Workspace, t.Model, t.Status, t.Output, t.AllowSensitive,
		t.CreatedAt.UnixMilli(), t.UpdatedAt.UnixMilli())
	if err != nil {
		return err
	}
	if t.Status != "pending" && t.Status != "running" && t.Status != "stopping" && t.RequesterID != "" && t.ChannelID != "" {
		_, _ = db.ExecContext(ctx, `DELETE FROM agent_tickets
			WHERE requester_id = ? AND channel_id = ?
			  AND status NOT IN ('pending', 'running', 'stopping')
			  AND id NOT IN (
				SELECT id FROM agent_tickets
				WHERE requester_id = ? AND channel_id = ?
				  AND status NOT IN ('pending', 'running', 'stopping')
				ORDER BY created_at DESC, id DESC LIMIT 200
			  )`, t.RequesterID, t.ChannelID, t.RequesterID, t.ChannelID)
	}
	return nil
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
		return m.findAgentTicketByPrefix(ctx, db, requesterID, channelID, ticketID)
	}
	if err != nil {
		return nil, err
	}
	t.AllowSensitive = sensitive != 0
	t.CreatedAt = time.UnixMilli(createdAt)
	t.UpdatedAt = time.UnixMilli(updatedAt)
	return &t, nil
}

func (m *Manager) findAgentTicketByPrefix(ctx context.Context, conn *sql.DB, requesterID, channelID, rawPrefix string) (*AgentTicket, error) {
	prefix := strings.ToLower(strings.TrimSpace(rawPrefix))
	if len(prefix) < 4 || strings.ContainsAny(prefix, "%_") {
		return nil, nil
	}
	altPrefix := prefix
	if !strings.HasPrefix(prefix, "tkt-") && !strings.HasPrefix(prefix, "tkt_") {
		altPrefix = "tkt-" + prefix
	}
	rows, err := conn.QueryContext(ctx, `SELECT id, requester_id, channel_id, goal, worker, workspace, model, status, output, allow_sensitive, created_at, updated_at
		FROM agent_tickets
		WHERE requester_id=? AND channel_id=? AND (lower(id) LIKE ? OR lower(id) LIKE ?)
		ORDER BY created_at DESC LIMIT 2`, requesterID, channelID, prefix+"%", altPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var matches []AgentTicket
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
		matches = append(matches, t)
	}
	if err := rows.Err(); err != nil || len(matches) != 1 {
		return nil, err
	}
	return &matches[0], nil
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
