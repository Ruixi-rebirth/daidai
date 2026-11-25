package runtime

import (
	"context"
	"daidai/internal/entity"
	"errors"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/core"
)

func (a *SuperAgent) notifyCognitionDeadline(event *entity.SensoryEvent, deadlineErr, detailErr error) {
	if !errors.Is(deadlineErr, context.DeadlineExceeded) || (event.StillValid != nil && !event.StillValid()) {
		return
	}

	message := "这次处理超过时限，结果还没完成。要我重新处理并补上吗？"
	var checkpoint *entity.ReplyDeadlineWithDraft
	var uncertain *core.UncertainExecutionError
	switch {
	case errors.As(detailErr, &uncertain):
		message = uncertainExecutionReply(detailErr)
	case errors.As(detailErr, &checkpoint):
		message = "这次处理超过时限，已有工具结果已保存。回复“/continue " + checkpoint.DraftID + "”接着写。" + checkpoint.Progress
	}

	notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer notifyCancel()
	a.replyText(notifyCtx, event, message)
}

func (a *SuperAgent) handleCognitionFailure(thinkCtx context.Context, event *entity.SensoryEvent, thinkErr error) {
	if event.GroupMessageObserved {
		slog.Warn("superagent: 群聊插话判断失败，已静默记录消息", "channel", event.ChannelID, "err", thinkErr)
		a.commitObservedGroupMessage(event)
		return
	}
	if thinkCtx.Err() != nil || (event.StillValid != nil && !event.StillValid()) {
		a.notifyCognitionDeadline(event, thinkCtx.Err(), thinkErr)
		slog.Info("superagent: 当前会话思考上下文已结束", "channel", event.ChannelID, "err", thinkCtx.Err())
		return
	}

	slog.Error("superagent: 思考过程发生异常", "err", thinkErr)
	errorReply := "这次处理没有完成。模型服务或任务运行发生错误；必要时回复“/continue”查看已保存的任务草稿。"
	var uncertain *core.UncertainExecutionError
	if errors.As(thinkErr, &uncertain) {
		errorReply = uncertainExecutionReply(thinkErr)
	}
	sendCtx, cancel := context.WithTimeout(thinkCtx, 15*time.Second)
	defer cancel()
	if err := a.transmitReplyText(sendCtx, event, errorReply); err != nil {
		return
	}
	commitCtx, commitCancel := context.WithTimeout(thinkCtx, 15*time.Second)
	a.commitCognitionReply(commitCtx, event, errorReply)
	commitCancel()
}

func uncertainExecutionReply(err error) string {
	var uncertain *core.UncertainExecutionError
	if errors.As(err, &uncertain) && uncertain.Err != nil {
		err = uncertain.Err
	}
	detail := "执行返回异常，但没有提供错误详情"
	if err != nil {
		if text := strings.Join(strings.Fields(err.Error()), " "); text != "" {
			detail = core.RedactSensitiveIPs(text)
		}
	}
	detail = core.TruncateRunes(detail, 700, "…（错误详情已截断）")
	return "AGY 执行结果未确认，我没有自动重试以免重复操作。当前错误：\n" + detail + "\n请先检查任务状态，再告诉我下一步。"
}
