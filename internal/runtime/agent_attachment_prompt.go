package runtime

import (
	"context"
	"daidai/internal/entity"
	"fmt"
)

func (a *SuperAgent) askBeforeUnrequestedAttachmentProcessing(ctx context.Context, event *entity.SensoryEvent, content string) bool {
	if event == nil || (event.Platform != "qq" && event.Platform != "telegram") || content != "" ||
		(len(event.Images) == 0 && event.OmittedImages == 0 && len(event.Files) == 0 && event.OmittedFiles == 0) {
		return false
	}

	// Never inspect an attachment just because it was uploaded. Ask for a task
	// before invoking vision, cognition or any tool.
	attachmentCtx, attachmentCancel := context.WithCancel(ctx)
	cancellation := &thinkCancellation{key: thinkKey(event)}
	a.sessionCancels.Store(cancellation, attachmentCancel)
	defer func() {
		a.sessionCancels.Delete(cancellation)
		attachmentCancel()
	}()
	unlock := a.lockSession(event)
	defer unlock()
	if attachmentCtx.Err() != nil || (event.StillValid != nil && !event.StillValid()) {
		return true
	}
	if event.IsPartnerPrivate() {
		a.deliverPendingNotifications(attachmentCtx, event)
		if attachmentCtx.Err() != nil || (event.StillValid != nil && !event.StillValid()) {
			return true
		}
	}

	message := "我收到了附件，还没有查看或读取。你想让我怎么处理？例如描述图片、总结文件、提取信息或转换格式。"
	if len(event.Images) == 0 && len(event.Files) == 0 {
		message = "我收到附件消息，但本轮附件没有接收完整。请重新发送要处理的附件，并告诉我希望怎么处理。"
		if event.OmittedImages > 0 || event.OmittedFiles > 0 {
			message = fmt.Sprintf("我收到附件消息，但本轮有 %d 张图片和 %d 个文件因数量上限未接收。请重新发送要处理的附件，并告诉我希望怎么处理。", event.OmittedImages, event.OmittedFiles)
		}
	} else {
		if event.OmittedImages > 0 {
			message += fmt.Sprintf("另外有 %d 张图片因本轮数量上限未接收；需要处理的话请重新发送。", event.OmittedImages)
		}
		if event.OmittedFiles > 0 {
			message += fmt.Sprintf("另外有 %d 个文件因本轮数量上限未接收；需要处理的话请重新发送。", event.OmittedFiles)
		}
	}
	if attachmentCtx.Err() == nil && (event.StillValid == nil || event.StillValid()) {
		a.replyText(attachmentCtx, event, message)
	}
	return true
}
