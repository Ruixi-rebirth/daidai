package runtime

import (
	"context"
	"daidai/internal/entity"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"daidai/internal/config"
	"daidai/internal/core"
)

func (a *SuperAgent) requirePrivateTicketCommand(ctx context.Context, event *entity.SensoryEvent) bool {
	if err := a.delegationPermissionError(event.Role); err != nil {
		a.replyText(ctx, event, err.Error())
		return false
	}
	if event.IsGroup {
		a.replyText(ctx, event, fmt.Sprintf("工程工单仅用于私聊工程协作，请在与%s的私聊中查看或管理。", config.BotName()))
		return false
	}
	return true
}

func (a *SuperAgent) handleTaskCommand(ctx context.Context, event *entity.SensoryEvent, content string) {
	if !a.requirePrivateTicketCommand(ctx, event) {
		return
	}
	if a.Delegates == nil {
		a.replyText(ctx, event, "工单调度器未配置。")
		return
	}
	parts := strings.Fields(content)
	if len(parts) > 3 {
		a.replyText(ctx, event, "用法：/task [工单编号] [页码]")
		return
	}
	page := 1
	var ticket *entity.Ticket
	var err error
	switch len(parts) {
	case 1:
		recent, listErr := a.Delegates.TicketsFor(ctx, event.Platform, event.SenderID, event.ChannelID, 1)
		if len(recent) == 0 {
			if listErr != nil {
				a.replyText(ctx, event, "读取近期工单失败，请稍后重试。")
				return
			}
			a.replyText(ctx, event, "当前会话暂无工程工单。")
			return
		}
		ticket, err = a.Delegates.TicketFor(ctx, event.Platform, event.SenderID, event.ChannelID, recent[0].ID)
	case 2:
		ticket, err = a.Delegates.TicketFor(ctx, event.Platform, event.SenderID, event.ChannelID, parts[1])
		if ticket == nil {
			if parsedPage, parseErr := strconv.Atoi(parts[1]); parseErr == nil && parsedPage >= 1 {
				page = parsedPage
				recent, _ := a.Delegates.TicketsFor(ctx, event.Platform, event.SenderID, event.ChannelID, 1)
				if len(recent) > 0 {
					ticket, err = a.Delegates.TicketFor(ctx, event.Platform, event.SenderID, event.ChannelID, recent[0].ID)
				}
			}
		}
	case 3:
		parsedPage, parseErr := strconv.Atoi(parts[2])
		if parseErr != nil || parsedPage < 1 {
			a.replyText(ctx, event, "页码必须是大于 0 的整数。用法：/task [工单编号] [页码]")
			return
		}
		page = parsedPage
		ticket, err = a.Delegates.TicketFor(ctx, event.Platform, event.SenderID, event.ChannelID, parts[1])
	}
	if ticket == nil {
		if err != nil {
			slog.Warn("superagent: 查询工单失败", "channel", event.ChannelID, "err", err)
			a.replyText(ctx, event, "读取工单状态失败，请稍后重试。")
			return
		}
		a.replyText(ctx, event, "当前会话中没有找到这张工单。")
		return
	}
	output := strings.TrimSpace(ticket.Output)
	if output == "" {
		output = "（暂无执行输出）"
	}
	if !event.IsPartnerPrivate() {
		output = core.RedactSensitiveIPs(output)
	}
	const outputPageSize = 3500
	pages := paginateTicketOutput(output, outputPageSize)
	pageCount := len(pages)
	if page > pageCount {
		a.replyText(ctx, event, fmt.Sprintf("这张工单共 %d 页，请选择 1 到 %d。", pageCount, pageCount))
		return
	}
	goal := core.TruncateRunes(strings.TrimSpace(ticket.Goal), 300, "…")

	var sb strings.Builder
	fmt.Fprintf(&sb, "### 🧾 工单详情 · `%s`\n\n", ticket.ID)

	statusBadge := ticketStatusBadge(ticket.Status)
	durationStr := ""
	if ticket.Status == entity.TicketPending || ticket.Status == entity.TicketRunning || ticket.Status == entity.TicketStopping {
		durationStr = fmt.Sprintf("（已运行 %s）", formatTicketDuration(time.Since(ticket.CreatedAt)))
	} else if ticket.UpdatedAt.After(ticket.CreatedAt) {
		durationStr = fmt.Sprintf("（总耗时 %s）", formatTicketDuration(ticket.UpdatedAt.Sub(ticket.CreatedAt)))
	}

	fmt.Fprintf(&sb, "- **当前状态**：%s %s\n", statusBadge, durationStr)
	fmt.Fprintf(&sb, "- **任务目标**：%s\n", goal)
	if ticket.Workspace != "" {
		fmt.Fprintf(&sb, "- **工作空间**：`%s`\n", ticket.Workspace)
	}
	if ticket.Worker != "" {
		fmt.Fprintf(&sb, "- **执行通道**：`%s`\n", ticket.Worker)
	}
	if ticket.Model != "" {
		fmt.Fprintf(&sb, "- **执行模型**：`%s`\n", ticket.Model)
	}
	timeRange := ticket.CreatedAt.Format("2006-01-02 15:04:05")
	if ticket.UpdatedAt.After(ticket.CreatedAt) {
		timeRange += " ~ " + ticket.UpdatedAt.Format("15:04:05")
	}
	fmt.Fprintf(&sb, "- **记录时间**：%s\n", timeRange)
	fmt.Fprintf(&sb, "- **输出分页**：第 %d/%d 页\n\n", page, pageCount)

	sb.WriteString("#### 💻 执行输出\n")
	sb.WriteString(core.MarkdownCodeBlock(pages[page-1], "bash"))
	sb.WriteByte('\n')

	if page < pageCount {
		fmt.Fprintf(&sb, "\n---\n💡 发送 `/task %s %d` 可查看下一页输出\n", ticket.ID, page+1)
	}
	if err != nil {
		sb.WriteString("\n> ⚠️ 注意：此工单最新状态尚未可靠写入 SQLite，当前显示的是进程内快照。\n")
	}

	a.replyInfo(ctx, event, fmt.Sprintf("🧾 工单详情 · %s", ticket.ID), strings.TrimSpace(sb.String()))
}

func (a *SuperAgent) handleTasksCommand(ctx context.Context, event *entity.SensoryEvent, _ string) {
	if !a.requirePrivateTicketCommand(ctx, event) {
		return
	}
	if a.Delegates == nil {
		a.replyText(ctx, event, "工单调度器未配置。")
		return
	}
	tickets, err := a.Delegates.TicketsFor(ctx, event.Platform, event.SenderID, event.ChannelID, 10)
	if err != nil {
		slog.Warn("superagent: 查询工单历史失败", "channel", event.ChannelID, "err", err)
	}
	if err != nil && len(tickets) == 0 {
		a.replyText(ctx, event, "读取工单历史失败，请稍后重试。")
		return
	}
	if len(tickets) == 0 {
		a.replyText(ctx, event, "当前会话没有你的近期工单。")
		return
	}

	var activeTickets []*entity.Ticket
	var historyTickets []*entity.Ticket
	for _, ticket := range tickets {
		switch ticket.Status {
		case entity.TicketPending, entity.TicketRunning, entity.TicketStopping:
			activeTickets = append(activeTickets, ticket)
		default:
			historyTickets = append(historyTickets, ticket)
		}
	}

	var sb strings.Builder
	sb.WriteString("### 📋 近期工程工单\n\n")
	sb.WriteString("> 统计范围：当前会话最近发起的 10 条工单\n\n")

	if len(activeTickets) > 0 {
		fmt.Fprintf(&sb, "#### ⚡ 活跃工单 (%d)\n", len(activeTickets))
		for _, ticket := range activeTickets {
			goal := core.TruncateRunes(strings.TrimSpace(ticket.Goal), 120, "…")
			durationStr := formatTicketDuration(time.Since(ticket.CreatedAt))
			badge := ticketStatusBadge(ticket.Status)
			fmt.Fprintf(&sb, "- %s `%s` ·（已运行 %s）\n", badge, ticket.ID, durationStr)
			fmt.Fprintf(&sb, "  - 🎯 **目标**：%s\n", goal)
			if ticket.Workspace != "" {
				fmt.Fprintf(&sb, "  - 📁 **工作区**：`%s`\n", ticket.Workspace)
			}
		}
		sb.WriteString("\n")
	}

	if len(historyTickets) > 0 {
		fmt.Fprintf(&sb, "#### 📜 历史工单 (%d)\n", len(historyTickets))
		for _, ticket := range historyTickets {
			goal := core.TruncateRunes(strings.TrimSpace(ticket.Goal), 120, "…")
			badge := ticketStatusBadge(ticket.Status)
			timeStr := ticket.UpdatedAt.Format("01-02 15:04")
			durationPart := ""
			if ticket.UpdatedAt.After(ticket.CreatedAt) {
				durationPart = fmt.Sprintf(" · 耗时 %s", formatTicketDuration(ticket.UpdatedAt.Sub(ticket.CreatedAt)))
			}
			fmt.Fprintf(&sb, "- %s `%s` · %s%s\n", badge, ticket.ID, timeStr, durationPart)
			fmt.Fprintf(&sb, "  - 🎯 **目标**：%s\n", goal)
		}
		sb.WriteString("\n")
	}

	sb.WriteString("---\n")
	sb.WriteString("💡 **操作提示**：\n")
	sampleID := tickets[0].ID
	fmt.Fprintf(&sb, "- 发送 `/task <工单编号>` 查看执行详情与日志（例如 `/task %s`）\n", sampleID)
	if len(activeTickets) > 0 {
		fmt.Fprintf(&sb, "- 发送 `/stop <工单编号>` 可中止活跃工单（例如 `/stop %s`）\n", activeTickets[0].ID)
	}

	if err != nil {
		sb.WriteString("\n> ⚠️ 注意：SQLite 历史或部分最新状态不可用；以上可能只包含进程内快照。\n")
	}

	a.replyInfo(ctx, event, "📋 近期工单", strings.TrimSpace(sb.String()))
}
