package tool

import (
	"context"
	"fmt"
	"log/slog"

	"daidai/internal/core"
	"daidai/internal/entity"
)

type ticketSubmission struct {
	event           *entity.SensoryEvent
	args            delegateArgs
	goal            string
	workspace       string
	attachmentScope string
	attachmentDir   string
	attachments     []*core.Attachment
}

func (t *DelegateTool) submitTicket(ctx context.Context, submission ticketSubmission) (*core.ToolResult, error) {
	event, args := submission.event, submission.args
	if err := ctx.Err(); err != nil {
		t.removeUnstartedAttachments(submission.attachmentDir)
		return nil, err
	}
	ticket := t.delegates.CreateTicketForPlatform(
		event.Platform, event.SenderID, event.ChannelID, submission.goal,
		args.Model, submission.workspace, args.Worker, event.IsPartnerPrivate(),
	)
	if ticket == nil {
		t.removeUnstartedAttachments(submission.attachmentDir)
		return &core.ToolResult{Text: "无法保存工单记录，任务未启动。请稍后重试；若持续发生，请联系管理员检查 SQLite 状态。"}, nil
	}
	if result := t.grantTicketAttachmentDirectory(ticket, submission.attachmentDir); result != nil {
		return result, nil
	}
	cleanup := t.ticketAttachmentCleanup(ticket, submission)
	if err := ctx.Err(); err != nil {
		t.delegates.FailPendingTicket(ticket.ID, "创建请求已取消，工单未进入队列。")
		cleanup(ticket, "", err)
		return nil, err
	}
	if err := t.delegates.SetTicketGroup(ticket.ID, event.IsGroup); err != nil {
		cleanup(ticket, "", err)
		t.delegates.FailPendingTicket(ticket.ID, "无法保存工单会话类型，工单未启动。")
		return &core.ToolResult{Text: "无法保存工单会话类型，工单未启动。"}, nil
	}
	slog.Info("delegate_tool: 已签发独立工程工单", "ticketID", ticket.ID, "goalBytes", len(ticket.Goal), "worker", args.Worker)
	if err := t.delegates.EnqueueTicket(ticket.ID, cleanup); err != nil {
		t.delegates.FailPendingTicket(ticket.ID, err.Error())
		cleanup(ticket, "", err)
		return &core.ToolResult{Text: "工单未能进入后台队列：" + err.Error()}, nil
	}
	return &core.ToolResult{Text: ticketQueuedNotice(ticket, event, args), DirectReply: true}, nil
}

func (t *DelegateTool) grantTicketAttachmentDirectory(ticket *entity.Ticket, attachmentDir string) *core.ToolResult {
	if attachmentDir == "" {
		return nil
	}
	if err := t.delegates.SetTicketAdditionalDirs(ticket.ID, []string{attachmentDir}); err != nil {
		if !t.delegates.FailPendingTicket(ticket.ID, "附件目录授权失败，工单未启动。") {
			slog.Warn("delegate_tool: failed to finalize unstarted attachment ticket", "ticketID", ticket.ID)
		}
		t.removeUnauthorizedAttachments(ticket, attachmentDir, "failed to remove unauthorized attachment directory")
		return &core.ToolResult{Text: "无法授权 AGY 读取当前附件目录，工单未启动。"}
	}
	if err := t.inbox.Activate(attachmentDir); err != nil {
		if !t.delegates.FailPendingTicket(ticket.ID, "附件目录保护失败，工单未启动。") {
			slog.Warn("delegate_tool: failed to finalize unprotected attachment ticket", "ticketID", ticket.ID)
		}
		t.removeUnauthorizedAttachments(ticket, attachmentDir, "failed to remove unprotected attachment directory")
		return &core.ToolResult{Text: "无法保护工单附件目录，工单未启动。"}
	}
	return nil
}

func (t *DelegateTool) removeUnauthorizedAttachments(ticket *entity.Ticket, attachmentDir, message string) {
	if err := t.inbox.Remove(attachmentDir); err != nil {
		slog.Warn("delegate_tool: "+message, "ticketID", ticket.ID, "err", err)
	}
}

func (t *DelegateTool) removeUnstartedAttachments(attachmentDir string) {
	if t.inbox == nil || attachmentDir == "" {
		return
	}
	if err := t.inbox.Remove(attachmentDir); err != nil {
		slog.Warn("delegate_tool: failed to remove unstarted attachment directory", "err", err)
	}
}

func (t *DelegateTool) ticketAttachmentCleanup(ticket *entity.Ticket, submission ticketSubmission) func(*entity.Ticket, string, error) {
	return func(_ *entity.Ticket, _ string, _ error) {
		if t.inbox != nil && submission.attachmentScope != "" {
			t.inbox.ForgetPending(submission.attachmentScope, submission.attachments)
		}
		if t.inbox != nil && submission.attachmentDir != "" {
			t.inbox.Deactivate(submission.attachmentDir)
			if err := t.inbox.Remove(submission.attachmentDir); err != nil {
				slog.Warn("delegate_tool: failed to remove completed attachment directory", "ticketID", ticket.ID, "err", err)
			}
		}
	}
}

func ticketQueuedNotice(ticket *entity.Ticket, event *entity.SensoryEvent, args delegateArgs) string {
	result := fmt.Sprintf("我先把这件事排进后台了，编号：%s。做完或遇到问题我会主动告诉你；发 `/task %s` 可以看进度和完整结果，`/stop %s` 可以只取消这件事，单独发 `/stop` 会取消你发起的全部后台任务。", ticket.ID, ticket.ID, ticket.ID)
	if args.IncludeCurrentAttachments && event.OmittedFiles > 0 {
		result += fmt.Sprintf("当前消息另有 %d 个文件未能处理。", event.OmittedFiles)
	}
	if args.IncludePendingAttachments && event.OmittedPendingFiles > 0 {
		result += fmt.Sprintf("另有 %d 个较早文件已从待处理列表移除。", event.OmittedPendingFiles)
	}
	return result
}
