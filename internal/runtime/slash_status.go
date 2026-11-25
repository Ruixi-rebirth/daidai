package runtime

import (
	"context"
	"daidai/internal/entity"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/config"
	"daidai/internal/core"
)

func (a *SuperAgent) handleStatusCommand(ctx context.Context, event *entity.SensoryEvent, _ string) {
	if err := a.delegationPermissionError(event.Role); err != nil {
		a.replyText(ctx, event, err.Error())
		return
	}
	var sections []string
	sections = append(sections, fmt.Sprintf("### %s生命体当前机能状态", config.BotName()))
	if a.Channels != nil {
		sections = append(sections, a.Channels.FormatStatus())
	}
	if a.Cognition != nil {
		if mindStatus := strings.TrimSpace(a.Cognition.MindStatus()); mindStatus != "" {
			sections = append(sections, mindStatus)
		}
	}
	sections = append(sections, core.FormatSystemStatusWithPerm(event.IsPartnerPrivate()))
	if event.IsPartnerPrivate() {
		if a.Workspaces != nil {
			sections = append(sections, a.Workspaces.FormatCurrent(event.Platform, event.ChannelID))
		}
		if a.Delegates != nil {
			active := a.Delegates.ActiveTicketsFor(event.Platform, event.SenderID, event.ChannelID)
			if len(active) == 0 {
				sections = append(sections, "后台工程工单：当前空闲 (0 个活跃)")
			} else {
				var lines []string
				for _, ticket := range active {
					lines = append(lines, fmt.Sprintf("- %s [%s · %s]: %s (已持续 %s)", ticket.ID, ticket.Worker, ticketStatusLabel(ticket.Status), ticket.Goal, time.Since(ticket.CreatedAt).Truncate(time.Second)))
				}
				sections = append(sections, fmt.Sprintf("后台工程工单 (%d 个活跃):\n%s", len(active), strings.Join(lines, "\n")))
			}
		}
	}
	a.replyInfo(ctx, event, "📊 当前状态", strings.Join(sections, "\n\n"))
}

func (a *SuperAgent) handleModelCommand(ctx context.Context, event *entity.SensoryEvent, _ string) {
	if !a.checkModelCommandPermission(ctx, event) {
		return
	}
	status := strings.TrimSpace(a.Cognition.MindStatus())
	if status == "" {
		status = "当前没有可用的激活模型状态。"
	}
	a.replyInfo(ctx, event, "🧠 当前模型", status)
}

func (a *SuperAgent) handleModelsCommand(ctx context.Context, event *entity.SensoryEvent, _ string) {
	if !a.checkModelCommandPermission(ctx, event) {
		return
	}
	var models string
	if contextual, ok := a.Cognition.(interface{ ModelListContext(context.Context) string }); ok {
		models = contextual.ModelListContext(ctx)
	} else {
		models = a.Cognition.ModelList()
	}
	a.replyInfo(ctx, event, "🧠 可用模型", models)
}

func (a *SuperAgent) handleUsageCommand(ctx context.Context, event *entity.SensoryEvent, _ string) {
	if !a.checkModelCommandPermission(ctx, event) {
		return
	}
	var usage string
	if contextual, ok := a.Cognition.(interface{ ModelUsageContext(context.Context) string }); ok {
		usage = contextual.ModelUsageContext(ctx)
	} else {
		usage = a.Cognition.ModelUsage()
	}
	a.replyInfo(ctx, event, "📊 模型用量", usage)
}

func (a *SuperAgent) handleVoiceCommand(ctx context.Context, event *entity.SensoryEvent, content string) {
	if err := a.delegationPermissionError(event.Role); err != nil {
		a.replyText(ctx, event, err.Error())
		return
	}
	if a.Channels == nil {
		a.replyText(ctx, event, "未初始化通道管理器。")
		return
	}
	ch, ok := a.Channels.Get(event.Platform)
	if !ok {
		a.replyText(ctx, event, fmt.Sprintf("未找到平台通道：%s", event.Platform))
		return
	}
	vmc, ok := ch.(entity.VoiceModedChannel)
	if !ok {
		a.replyText(ctx, event, fmt.Sprintf("当前平台（%s）暂不支持语音模式切换。", event.Platform))
		return
	}

	parts := strings.Fields(strings.TrimSpace(content))
	if len(parts) == 1 || (len(parts) == 2 && (parts[1] == "status" || parts[1] == "info")) {
		current := vmc.GetVoiceMode()
		var desc, switchHint string
		if current == "fast" {
			desc = "极速模式 (fast)\n- 特点：腾讯官方 ASR 毫秒级直出，零等待即时响应（适合普通中文对话）\n- 兜底：若官方未提供 ASR 文本，自动调用多模态原声转录"
			switchHint = "发送 `/voice multi` 切换为多语精准模式"
		} else {
			desc = "多语精准模式 (multi)\n- 特点：Gemini 原生多模态音频原声倾听，精准识别日语、英语及中英混杂，杜绝空耳（耗时约 3.5s）\n- 兜底：若网络波动转录超时，自动回退腾讯官方 ASR"
			switchHint = "发送 `/voice fast` 切换为极速模式"
		}
		msg := fmt.Sprintf("【语音识别设置】\n当前模式：%s\n\n💡 提示：%s", desc, switchHint)
		a.replyInfo(ctx, event, "🎙️ 语音识别设置", msg)
		return
	}

	if len(parts) == 2 {
		target := strings.ToLower(parts[1])
		switch target {
		case "fast", "quick", "speed":
			if err := vmc.SetVoiceMode("fast"); err != nil {
				a.replyText(ctx, event, fmt.Sprintf("切换极速语音模式失败：%v", err))
				return
			}
			a.replyInfo(ctx, event, "🎙️ 语音模式已切换", "已切换为 **极速模式 (fast)**。\n优先使用腾讯官方 ASR 毫秒级直出，快速响应中文（官方无 ASR 文本时自动回退多模态转录兜底）。")
			return
		case "multi", "accurate", "gemini":
			if err := vmc.SetVoiceMode("multi"); err != nil {
				a.replyText(ctx, event, fmt.Sprintf("切换多语精准模式失败：%v", err))
				return
			}
			a.replyInfo(ctx, event, "🎙️ 语音模式已切换", "已切换为 **多语精准模式 (multi)**。\n优先调用 Gemini 原生多模态音频倾听原声，精准识别日语/英语/多语种，杜绝空耳（超时自动回退官方 ASR 兜底）。")
			return
		}
	}

	a.replyText(ctx, event, "用法：\n- `/voice` 或 `/voice status`：查看当前语音识别模式\n- `/voice fast`：切换为极速模式（腾讯 ASR 零延时）\n- `/voice multi`：切换为多语精准模式（Gemini 原生倾听日语/英语/多语）")
}

func (a *SuperAgent) checkModelCommandPermission(ctx context.Context, event *entity.SensoryEvent) bool {
	if err := a.delegationPermissionError(event.Role); err != nil {
		a.replyText(ctx, event, err.Error())
		return false
	}
	return a.Cognition != nil
}

func (a *SuperAgent) handleStopCommand(ctx context.Context, event *entity.SensoryEvent, content string) {
	if err := a.delegationPermissionError(event.Role); err != nil {
		a.replyText(ctx, event, err.Error())
		return
	}
	parts := strings.Fields(strings.TrimSpace(content))
	if len(parts) > 1 {
		if event.IsGroup {
			a.replyText(ctx, event, fmt.Sprintf("工程工单仅用于私聊工程协作，请在与%s的私聊中管理。", config.BotName()))
			return
		}
		if len(parts) != 2 || a.Delegates == nil {
			a.replyText(ctx, event, "用法：/stop [工单编号]")
			return
		}
		ticket, _ := a.Delegates.TicketFor(ctx, event.Platform, event.SenderID, event.ChannelID, parts[1])
		if ticket == nil {
			a.replyText(ctx, event, "当前会话中没有找到这张工单。")
			return
		}
		if ticket.Status != entity.TicketPending && ticket.Status != entity.TicketRunning && ticket.Status != entity.TicketStopping {
			a.replyText(ctx, event, fmt.Sprintf("这张工单已经结束，状态：%s。", ticketStatusLabel(ticket.Status)))
			return
		}
		if a.Delegates.AbortTicketForRequester(ticket.ID, event.Platform, event.ChannelID, event.SenderID) > 0 {
			if ticket.Status == entity.TicketPending {
				a.replyText(ctx, event, "已取消这张排队中的工单。")
			} else {
				a.replyText(ctx, event, "已请求停止这张工单；工作者退出后会释放执行容量。")
			}
			return
		}
		current, ok := a.Delegates.GetTicket(ticket.ID)
		if ok && current.Status == entity.TicketStopping {
			a.replyText(ctx, event, "这张工单正在停止；工作者退出后会释放执行容量。")
			return
		}
		a.replyText(ctx, event, "这张工单当前无法取消，请用 /task 查看最新状态。")
		return
	}
	stoppedThinking := a.cancelThinkingRequester(event)
	if a.OnStop != nil && a.OnStop(ctx, event) {
		stoppedThinking = true
	}
	a.clearReplyContinuation(event)
	aborted, stopping := 0, 0
	if !event.IsGroup && a.Delegates != nil {
		aborted = a.Delegates.AbortByRequester(event.Platform, event.ChannelID, event.SenderID)
		for _, ticket := range a.Delegates.ActiveTicketsFor(event.Platform, event.SenderID, event.ChannelID) {
			if ticket.Status == entity.TicketStopping {
				stopping++
			}
		}
	}
	msg := "当前没有正在运行中的思考任务或后台工单。"
	if event.IsGroup {
		msg = "当前没有正在进行的思考任务。"
	}
	switch {
	case aborted > 0 && stoppedThinking:
		msg = fmt.Sprintf("已紧急中断当前思考进程，并向 %d 个后台工单发出停止请求（尚未启动的任务已取消；运行中任务退出后释放执行容量）。", aborted)
	case aborted > 0:
		msg = fmt.Sprintf("已向 %d 个后台工单发出停止请求（尚未启动的任务已取消；运行中任务退出后释放执行容量）。", aborted)
	case stopping > 0 && stoppedThinking:
		msg = fmt.Sprintf("已紧急中断当前思考进程；另有 %d 个后台工单仍在停止过程中，工作者退出后才会释放执行容量。", stopping)
	case stopping > 0:
		msg = fmt.Sprintf("仍有 %d 个后台工单正在停止；工作者退出后才会释放执行容量。", stopping)
	case stoppedThinking:
		msg = "已紧急中断当前正在进行的思考与工具调用。"
	}
	a.replyText(ctx, event, msg)
}

func (a *SuperAgent) handleResetCommand(ctx context.Context, event *entity.SensoryEvent, content string) {
	if err := a.delegationPermissionError(event.Role); err != nil {
		a.replyText(ctx, event, err.Error())
		return
	}
	if event.IsGroup && strings.HasPrefix(strings.TrimSpace(content), "/ws") {
		a.replyText(ctx, event, fmt.Sprintf("工作空间仅用于私聊工程协作，请在与%s的私聊中查看或切换。", config.BotName()))
		return
	}
	a.cancelThinkingSession(event)
	unlock := a.lockSession(event)
	defer unlock()
	currWs, wsName := "", ""
	if !event.IsGroup && a.Workspaces != nil {
		var err error
		currWs, wsName, err = a.Workspaces.GetCurrent(event.Platform, event.ChannelID)
		if err != nil {
			a.replyText(ctx, event, fmt.Sprintf("【会话重开失败】\n无法确认当前工作区，近期历史和 AGY 会话均未重置：%v", err))
			return
		}
	}
	if !event.IsGroup && a.Delegates != nil {
		_ = a.Delegates.AbortByChannel(event.Platform, event.ChannelID)
	}
	chatType := "private"
	if event.IsGroup {
		chatType = "group"
	}
	if a.database == nil {
		a.replyText(ctx, event, "【会话重开失败】\n近期对话历史存储未配置。")
		return
	}
	if err := a.database.ResetConversation(event.Platform, chatType, event.ChannelID); err != nil {
		slog.Error("superagent: 重开会话时清理近期历史失败", "platform", event.Platform, "channelID", event.ChannelID, "err", err)
		if event.IsGroup {
			a.replyText(ctx, event, "【会话重开失败】\n本群近期对话历史未能清理，请稍后重试。")
		} else {
			a.replyText(ctx, event, "【会话重开失败】\n近期对话历史未能清理，AGY 会话记录未清空；已中断的任务不会自动恢复。请稍后重试。")
		}
		return
	}
	a.clearSessionReplyContinuations(event)
	if attachmentCleaner, ok := a.Cognition.(interface{ ClearPendingAttachments(*entity.SensoryEvent) }); ok {
		attachmentCleaner.ClearPendingAttachments(event)
	}
	if a.OnSessionReset != nil {
		if err := a.OnSessionReset(ctx, event); err != nil {
			a.replyText(ctx, event, "近期对话已重置，但早报指代上下文未能清除，请稍后重试。 ")
			return
		}
	}
	if event.IsGroup {
		a.replyText(ctx, event, "【当前群聊会话已完成重开】\n已清空本群近期对话记忆，长期画像完好保留。")
		return
	}
	if a.Delegates != nil {
		if err := a.Delegates.ResetSession(event.Platform, event.ChannelID, currWs); err != nil {
			slog.Error("superagent: 重开会话时清理 AGY 会话失败", "platform", event.Platform, "channelID", event.ChannelID, "workspace", currWs, "err", err)
			a.replyText(ctx, event, fmt.Sprintf("【会话重开未完成】\n近期历史游标已重置，但 AGY 会话未能确认清除：%v。请先检查服务日志，再重试 `/reset`。", err))
			return
		}
	}
	a.replyText(ctx, event, fmt.Sprintf("【当前会话已完成重开】\n已清空工作空间 `%s` (`%s`) 的 agy 会话与近期对话记忆，长期画像与项目文件完好保留。", wsName, currWs))
}
