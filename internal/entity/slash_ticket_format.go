package entity

import (
	"fmt"
	"time"
)

// paginateTicketOutput prefers a nearby line break and preserves literal logs.
func paginateTicketOutput(output string, limit int) []string {
	if limit <= 0 {
		return []string{output}
	}
	runes := []rune(output)
	var pages []string
	for len(runes) > limit {
		end := limit
		for i := end - 1; i >= end/2; i-- {
			if runes[i] == '\n' {
				end = i + 1
				break
			}
		}
		pages = append(pages, string(runes[:end]))
		runes = runes[end:]
	}
	return append(pages, string(runes))
}

func ticketStatusLabel(status TicketStatus) string {
	switch status {
	case TicketPending:
		return "排队中"
	case TicketRunning:
		return "执行中"
	case TicketStopping:
		return "正在停止"
	case TicketCompleted:
		return "已完成"
	case TicketFailed:
		return "未完成"
	case TicketAborted:
		return "已停止"
	case TicketInterrupted:
		return "重启时中断"
	default:
		return string(status)
	}
}

func ticketStatusBadge(status TicketStatus) string {
	switch status {
	case TicketPending:
		return "⏳ 排队中"
	case TicketRunning:
		return "🟡 执行中"
	case TicketStopping:
		return "⏹️ 正在停止"
	case TicketCompleted:
		return "🟢 已完成"
	case TicketFailed:
		return "🔴 未完成"
	case TicketAborted:
		return "⏹️ 已停止"
	case TicketInterrupted:
		return "⚠️ 重启时中断"
	default:
		return string(status)
	}
}

func formatTicketDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d <= 0 {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if m < 60 {
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%ds", m, s)
	}
	h := m / 60
	m = m % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}
