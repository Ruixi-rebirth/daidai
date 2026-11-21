package tool

import (
	"fmt"
	"strings"

	"daidai/internal/core"
	"daidai/internal/entity"
)

func (t *DelegateTool) selectTicketAttachments(event *entity.SensoryEvent, args *delegateArgs) ([]*core.Attachment, error) {
	var selected []*core.Attachment
	if args.IncludeCurrentAttachments {
		if len(event.Files) == 0 {
			if event.OmittedFiles > 0 {
				return nil, fmt.Errorf("当前消息有 %d 个文件未进入处理队列，无法读取。请分批重新发送文件，并告诉我具体处理目标。", event.OmittedFiles)
			}
			return nil, fmt.Errorf("当前消息没有可处理的新文件附件。")
		}
		selected = append(selected, event.Files...)
	}
	if args.IncludePendingAttachments {
		if len(event.PendingFiles) == 0 && event.OmittedPendingFiles == 0 {
			return nil, fmt.Errorf("当前会话没有仍可处理的待确认文件；附件可能已过期，请重新发送后告诉我处理目标。")
		}
		selected = append(selected, event.PendingFiles...)
	}
	selected = uniqueTicketAttachments(selected)
	if len(selected) == 0 && args.IncludePendingAttachments && event.OmittedPendingFiles > 0 {
		return nil, fmt.Errorf("待确认列表中的文件已超出保留容量或过期（有 %d 个更早文件被移除），无法处理；请重新发送。", event.OmittedPendingFiles)
	}
	if len(selected) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(event.Content) == "" {
		return nil, fmt.Errorf("我收到了文件。你想让我读取、总结、转换还是做其他处理？告诉我具体目标后我再打开文件。")
	}
	if args.Worker != "" && !strings.EqualFold(strings.TrimSpace(args.Worker), "agy") {
		return nil, fmt.Errorf("QQ 文件附件目前只能交给 AGY 工作者处理。")
	}
	if !hasAGYWorker(t.delegates) || t.inbox == nil {
		return nil, fmt.Errorf("文件处理暂不可用：AGY 工作者或附件暂存目录未配置。")
	}
	args.Worker = "agy"
	return selected, nil
}

func uniqueTicketAttachments(attachments []*core.Attachment) []*core.Attachment {
	seen := make(map[string]struct{}, len(attachments))
	unique := make([]*core.Attachment, 0, len(attachments))
	for _, attachment := range attachments {
		if attachment == nil {
			continue
		}
		if _, exists := seen[attachment.URL]; exists {
			continue
		}
		seen[attachment.URL] = struct{}{}
		unique = append(unique, attachment)
	}
	return unique
}

func hasAGYWorker(delegates *entity.Delegates) bool {
	if delegates == nil {
		return false
	}
	for _, worker := range delegates.ListWorkers() {
		if strings.EqualFold(worker.Name, "agy") {
			return true
		}
	}
	return false
}
