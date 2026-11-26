package entity

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"daidai/internal/db"
)

func (d *Delegates) persistTicket(snapshot Ticket) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return d.persistTicketContext(ctx, snapshot)
}

func (d *Delegates) persistTicketContext(ctx context.Context, snapshot Ticket) bool {
	d.mu.RLock()
	manager, defaultPlatform := d.ticketDB, d.ticketPlatform
	d.mu.RUnlock()
	platform := strings.TrimSpace(snapshot.Platform)
	if platform == "" {
		platform = defaultPlatform
	}
	if manager == nil || platform == "" {
		return true
	}
	record := db.AgentTicket{ID: snapshot.ID, RequesterID: snapshot.RequesterID, ChannelID: snapshot.ChannelID,
		Goal: snapshot.Goal, Worker: snapshot.Worker, Workspace: snapshot.Workspace, Model: snapshot.Model,
		Status: string(snapshot.Status), Output: snapshot.Output, AllowSensitive: snapshot.AllowSensitive,
		CreatedAt: snapshot.CreatedAt, UpdatedAt: snapshot.UpdatedAt}
	err := manager.SaveAgentTicket(ctx, platform, record)
	d.mu.Lock()
	defer d.mu.Unlock()
	if current := d.tickets[snapshot.ID]; current != nil && current.UpdatedAt.Equal(snapshot.UpdatedAt) {
		if err != nil {
			current.persistenceWarning = "工单状态未能写入 SQLite；当前结果只保存在运行中的进程内。"
		} else {
			current.persistenceWarning = ""
		}
	}
	if err == nil && len(d.tickets) > 256 {
		var oldestID string
		var oldestAt time.Time
		for id, ticket := range d.tickets {
			if ticket == nil || ticket.Status == TicketPending || ticket.Status == TicketRunning || ticket.Status == TicketStopping || ticket.persistenceWarning != "" {
				continue
			}
			if oldestID == "" || ticket.UpdatedAt.Before(oldestAt) {
				oldestID = id
				oldestAt = ticket.UpdatedAt
			}
		}
		if oldestID != "" {
			delete(d.tickets, oldestID)
		}
	}
	if err != nil {
		slog.Error("delegate ticket persistence failed", "ticketID", snapshot.ID, "err", err)
		return false
	}
	return true
}

// TicketsFor returns only the caller's history in the current conversation.
func (d *Delegates) TicketsFor(ctx context.Context, platform, requesterID, channelID string, limit int) ([]*Ticket, error) {
	d.mu.RLock()
	manager := d.ticketDB
	d.mu.RUnlock()
	if manager == nil {
		return d.memoryTicketsFor(platform, requesterID, channelID, limit), fmt.Errorf("工单历史存储未配置")
	}
	records, err := manager.ListAgentTickets(ctx, platform, requesterID, channelID, limit)
	if err != nil {
		return d.memoryTicketsFor(platform, requesterID, channelID, limit), err
	}
	byID := make(map[string]*Ticket, len(records))
	for _, record := range records {
		byID[record.ID] = &Ticket{ID: record.ID, Platform: platform, RequesterID: record.RequesterID,
			ChannelID: record.ChannelID, Goal: record.Goal, Worker: record.Worker,
			Workspace: record.Workspace, Model: record.Model, Status: TicketStatus(record.Status),
			Output: record.Output, AllowSensitive: record.AllowSensitive,
			CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}
	}
	d.mu.RLock()
	for id, t := range d.tickets {
		if t.Platform != platform || t.RequesterID != requesterID || t.ChannelID != channelID {
			continue
		}
		cp := *t
		byID[id] = &cp
	}
	d.mu.RUnlock()
	tickets := make([]*Ticket, 0, len(byID))
	for _, ticket := range byID {
		tickets = append(tickets, ticket)
	}
	sort.Slice(tickets, func(i, j int) bool {
		if tickets[i].CreatedAt.Equal(tickets[j].CreatedAt) {
			return tickets[i].ID > tickets[j].ID
		}
		return tickets[i].CreatedAt.After(tickets[j].CreatedAt)
	})
	tickets = prioritizeActiveTickets(tickets, limit)
	var persistenceErr bool
	d.mu.RLock()
	for _, t := range d.tickets {
		if t.Platform == platform && t.RequesterID == requesterID && t.ChannelID == channelID && t.persistenceWarning != "" {
			persistenceErr = true
			break
		}
	}
	d.mu.RUnlock()
	if persistenceErr {
		return tickets, fmt.Errorf("部分工单最新状态尚未持久化")
	}
	return tickets, nil
}

func (d *Delegates) memoryTicketsFor(platform, requesterID, channelID string, limit int) []*Ticket {
	d.mu.RLock()
	tickets := make([]*Ticket, 0)
	for _, t := range d.tickets {
		if t.Platform != platform || t.RequesterID != requesterID || t.ChannelID != channelID {
			continue
		}
		cp := *t
		tickets = append(tickets, &cp)
	}
	d.mu.RUnlock()
	sort.Slice(tickets, func(i, j int) bool {
		if tickets[i].CreatedAt.Equal(tickets[j].CreatedAt) {
			return tickets[i].ID > tickets[j].ID
		}
		return tickets[i].CreatedAt.After(tickets[j].CreatedAt)
	})
	return prioritizeActiveTickets(tickets, limit)
}

func prioritizeActiveTickets(tickets []*Ticket, limit int) []*Ticket {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	selected := make([]*Ticket, 0, limit)
	selectedIDs := make(map[string]struct{}, len(tickets))
	for _, ticket := range tickets {
		if ticket.Status != TicketPending && ticket.Status != TicketRunning && ticket.Status != TicketStopping {
			continue
		}
		if len(selected) >= limit {
			break
		}
		selected = append(selected, ticket)
		selectedIDs[ticket.ID] = struct{}{}
	}
	for _, ticket := range tickets {
		if len(selected) >= limit {
			break
		}
		if _, exists := selectedIDs[ticket.ID]; exists {
			continue
		}
		selected = append(selected, ticket)
		selectedIDs[ticket.ID] = struct{}{}
	}
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].CreatedAt.Equal(selected[j].CreatedAt) {
			return selected[i].ID > selected[j].ID
		}
		return selected[i].CreatedAt.After(selected[j].CreatedAt)
	})
	return selected
}

func matchTicketIdentifier(candidateID, rawQuery string) bool {
	q := strings.ToLower(strings.TrimSpace(rawQuery))
	id := strings.ToLower(strings.TrimSpace(candidateID))
	if q == "" || id == "" {
		return false
	}
	if id == q {
		return true
	}
	if len(q) < 4 {
		return false
	}
	if strings.HasPrefix(id, q) {
		return true
	}
	if !strings.HasPrefix(q, "tkt-") && !strings.HasPrefix(q, "tkt_") {
		return strings.HasPrefix(id, "tkt-"+q) || strings.HasPrefix(id, "tkt_"+q)
	}
	return false
}

func (d *Delegates) TicketFor(ctx context.Context, platform, requesterID, channelID, ticketID string) (*Ticket, error) {
	ticketID = strings.TrimSpace(ticketID)
	d.mu.RLock()
	manager := d.ticketDB
	if t, ok := d.tickets[ticketID]; ok && t.Platform == platform && t.RequesterID == requesterID && t.ChannelID == channelID {
		cp := *t
		d.mu.RUnlock()
		if cp.persistenceWarning != "" {
			return &cp, fmt.Errorf("%s", cp.persistenceWarning)
		}
		return &cp, nil
	}
	var prefixMatch *Ticket
	ambiguous := false
	for _, t := range d.tickets {
		if t.Platform != platform || t.RequesterID != requesterID || t.ChannelID != channelID {
			continue
		}
		if matchTicketIdentifier(t.ID, ticketID) {
			if prefixMatch != nil {
				ambiguous = true
				break
			}
			cp := *t
			prefixMatch = &cp
		}
	}
	d.mu.RUnlock()
	if prefixMatch != nil && !ambiguous {
		if prefixMatch.persistenceWarning != "" {
			return prefixMatch, fmt.Errorf("%s", prefixMatch.persistenceWarning)
		}
		return prefixMatch, nil
	}
	if manager == nil {
		return nil, fmt.Errorf("工单历史存储未配置")
	}
	record, err := manager.GetAgentTicket(ctx, platform, requesterID, channelID, ticketID)
	if err != nil || record == nil {
		return nil, err
	}
	return &Ticket{ID: record.ID, Platform: platform, RequesterID: record.RequesterID, ChannelID: record.ChannelID,
		Goal: record.Goal, Worker: record.Worker, Workspace: record.Workspace, Model: record.Model,
		Status: TicketStatus(record.Status), Output: record.Output, AllowSensitive: record.AllowSensitive,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}, nil
}

// GetTicket 查询工单状态
func (d *Delegates) GetTicket(id string) (*Ticket, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	t, ok := d.tickets[id]
	if !ok || t == nil {
		return nil, false
	}
	cp := *t
	return &cp, true
}

// ActiveTickets 获取当前正在执行或待执行的所有活跃工单快照
func (d *Delegates) ActiveTickets() []*Ticket {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var list []*Ticket
	for _, t := range d.tickets {
		if t.Status == TicketRunning || t.Status == TicketStopping || t.Status == TicketPending {
			cp := *t
			list = append(list, &cp)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].ID < list[j].ID
		}
		return list[i].CreatedAt.Before(list[j].CreatedAt)
	})
	return list
}

func (d *Delegates) ActiveTicketsFor(platform, requesterID, channelID string) []*Ticket {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var list []*Ticket
	for _, t := range d.tickets {
		if t.Platform != platform || t.RequesterID != requesterID || t.ChannelID != channelID {
			continue
		}
		if t.Status == TicketRunning || t.Status == TicketStopping || t.Status == TicketPending {
			cp := *t
			list = append(list, &cp)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].ID < list[j].ID
		}
		return list[i].CreatedAt.Before(list[j].CreatedAt)
	})
	return list
}

// AbortByChannel 请求中断指定会话通道中的运行工单，并取消尚未启动的工单。
