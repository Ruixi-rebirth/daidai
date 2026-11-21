package agent

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/provider/llm"
)

func (w *AGYWorker) retainConversationID(session *AgySessionState, ticket *entity.Ticket, cwd string, output []byte) error {
	parsed, err := llm.ParseAGYResult(output)
	if err != nil || strings.TrimSpace(parsed.ConversationID) == "" {
		return nil
	}
	session.Lock()
	session.ConversationID = strings.TrimSpace(parsed.ConversationID)
	session.loaded = true
	conversationID := session.ConversationID
	session.Unlock()
	return w.saveConversationID(ticket, cwd, conversationID)
}

func (w *AGYWorker) saveConversationID(ticket *entity.Ticket, cwd, conversationID string) error {
	if w.sessionDB == nil || ticket == nil || conversationID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.sessionDB.SaveAGYSession(ctx, ticket.Platform, ticket.RequesterID, ticket.ChannelID, cwd, conversationID); err != nil {
		slog.Error("agy: save conversation ID failed", "requesterID", ticket.RequesterID, "channelID", ticket.ChannelID, "workspace", cwd, "err", err)
		return err
	}
	return nil
}

func appendSessionSaveWarning(output string, err error) string {
	if err == nil {
		return output
	}
	warning := "\n\n【续接状态未保存】当前进程内仍可继续，但服务重启后可能无法续接此 AGY 会话。"
	return strings.TrimSpace(output) + warning
}

func partialAGYOutput(stdout, stderr string, stdoutTruncated, stderrTruncated bool) string {
	var parts []string
	if stdout = strings.TrimSpace(stdout); stdout != "" {
		parts = append(parts, "标准输出：\n"+stdout)
	}
	if stderr = strings.TrimSpace(stderr); stderr != "" {
		parts = append(parts, "标准错误：\n"+stderr)
	}
	if len(parts) == 0 {
		return ""
	}
	result := strings.Join(parts, "\n\n")
	if stdoutTruncated || stderrTruncated {
		result += "\n[子进程输出已截断]"
	}
	return "【本阶段未完成，以下是子进程部分输出】\n" + core.TruncateRunes(result, 6000, "\n[阶段输出超过长度限制，后续内容已截断]")
}
