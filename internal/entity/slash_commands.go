package entity

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
)

type slashCommand struct {
	section string
	usage   string
	help    string
	access  string
	match   func(string) bool
	handle  func(context.Context, *SensoryEvent, string)
}

func slashCommandRegistry(a *SuperAgent) []slashCommand {
	exact := func(names ...string) func(string) bool {
		return func(content string) bool {
			for _, name := range names {
				if content == name {
					return true
				}
			}
			return false
		}
	}
	prefix := func(names ...string) func(string) bool {
		return func(content string) bool {
			for _, name := range names {
				if content == name || strings.HasPrefix(content, name+" ") {
					return true
				}
			}
			return false
		}
	}
	return []slashCommand{
		{section: "对话与会话", usage: "/help", help: "显示指令帮助卡", match: exact("/help"), handle: a.handleHelpCommand},
		{section: "对话与会话", usage: "/continue", help: "续发未发送完的长回复", match: exact("/continue", "继续"), handle: a.handleContinueCommand},
		{section: "对话与会话", usage: "/stop [工单编号]", help: "中断当前思考、全部后台工单或指定工单", access: "仅核心搭档/协作者", match: exact("/stop"), handle: a.handleStopCommand},
		{section: "对话与会话", usage: "/reset · /new · /clear", help: "重开当前会话并清除近期历史", access: "仅核心搭档/协作者", match: exact("/reset", "/new", "/clear", "/ws clear", "/ws reset"), handle: a.handleResetCommand},
		{section: "状态与模型", usage: "/status · /sysinfo", help: "查看运行状态、系统指标与活跃工单", match: exact("/status", "/sysinfo"), handle: a.handleStatusCommand},
		{section: "状态与模型", usage: "/model", help: "查看当前对话使用的模型；此命令不会切换模型", access: "仅核心搭档/协作者", match: exact("/model"), handle: a.handleModelCommand},
		{section: "状态与模型", usage: "/models", help: "列出可用模型", access: "仅核心搭档/协作者", match: exact("/models"), handle: a.handleModelsCommand},
		{section: "状态与模型", usage: "/usage", help: "查看模型用量", access: "仅核心搭档/协作者", match: exact("/usage"), handle: a.handleUsageCommand},
		{section: "工程工单", usage: "/tasks", help: "查看当前会话的近期工单", access: "仅核心搭档/协作者", match: exact("/tasks"), handle: a.handleTasksCommand},
		{section: "工程工单", usage: "/task <ID> [页码]", help: "查看工单状态和输出", access: "仅核心搭档/协作者", match: prefix("/task"), handle: a.handleTaskCommand},
		{section: "工作空间", usage: "/pwd", help: "查看当前工作空间", access: "仅核心搭档/协作者", match: exact("/pwd"), handle: a.handlePwdCommand},
		{section: "工作空间", usage: "/cd <目标>", help: "切换工作空间", access: "仅核心搭档/协作者", match: prefix("/cd"), handle: a.handleCDCommand},
		{section: "工作空间", usage: "/ws · /workspaces", help: "列出已纳管的工作空间", access: "仅核心搭档/协作者", match: exact("/ws", "/workspaces"), handle: a.handleWorkspaceListCommand},
		{section: "工作空间", usage: "/ws rm <名称>", help: "移除工作空间书签", access: "仅核心搭档/协作者", match: prefix("/ws rm", "/workspaces rm"), handle: a.handleWorkspaceRemoveCommand},
		{section: "工作空间", usage: "/ws exit", help: "退出当前工作空间", access: "仅核心搭档/协作者", match: exact("/ws exit", "/workspaces exit"), handle: a.handleWorkspaceExitCommand},
		{section: "工作空间", usage: "/ws clean · /clean", help: "清理默认 sandbox 临时文件", access: "仅核心搭档/协作者", match: exact("/ws clean", "/clean"), handle: a.handleWorkspaceCleanCommand},
		{section: "AGY 认证", usage: "/agy login [force]", help: "发起或强制重新进行 Google 认证", access: "仅私聊；核心搭档/协作者", match: exact("/agy login", "/agy auth", "/agy login force", "/agy login --force"), handle: a.handleAGYLoginCommand},
		{section: "AGY 认证", usage: "/agy code <授权码>", help: "提交 Google 授权码", access: "仅私聊；核心搭档/协作者", match: func(content string) bool { _, ok := parseAGYAuthCodeCommand(content); return ok }, handle: a.handleAGYCodeCommand},
		{section: "AGY 认证", usage: "/agy status", help: "查看 AGY 认证状态", access: "仅私聊；核心搭档/协作者", match: exact("/agy status"), handle: a.handleAGYStatusCommand},
		{section: "AGY 认证", usage: "/agy logout", help: "注销 AGY 并清除凭据", access: "仅私聊；核心搭档/协作者", match: exact("/agy logout"), handle: a.handleAGYLogoutCommand},
	}
}

func (a *SuperAgent) runRegisteredSlashCommand(ctx context.Context, event *SensoryEvent, content string) bool {
	for _, command := range slashCommandRegistry(a) {
		if command.match(content) {
			command.handle(ctx, event, content)
			return true
		}
	}
	return false
}

func (a *SuperAgent) handleHelpCommand(ctx context.Context, event *SensoryEvent, _ string) {
	sections := make([]string, 0)
	sectionNames := make([]string, 0)
	sectionLines := make(map[string][]string)
	for _, command := range slashCommandRegistry(a) {
		if _, exists := sectionLines[command.section]; !exists {
			sectionNames = append(sectionNames, command.section)
		}
		line := fmt.Sprintf("- %s：%s", command.usage, command.help)
		if command.access != "" {
			line += "（" + command.access + "）"
		}
		sectionLines[command.section] = append(sectionLines[command.section], line)
	}
	for _, name := range sectionNames {
		icon := map[string]string{"对话与会话": "💬", "状态与模型": "📊", "工程工单": "🧾", "工作空间": "📁", "AGY 认证": "🔐"}[name]
		sections = append(sections, fmt.Sprintf("### %s %s\n%s", icon, name, strings.Join(sectionLines[name], "\n")))
	}
	accessSection := `### 👥 用户与权限管理（仅核心搭档 QQ 私聊）
- 查看申请 / 用户：“查看申请” · “查看已授权用户” · “查看 [昵称/序号] 的权限”
- 授予权限：“将第 1 个申请者加为好友 / 协作者” · “将 [昵称] 加为好友”
- 修改权限：“将第 1 个用户修改为好友 / 协作者” · “把 [昵称] 的权限修改为好友”
- 删除 / 拒绝：“删除第 1 个用户” · “撤销 [昵称] 的权限” · “拒绝第 1 个申请”
- 邀请码管理：“生成一个好友 / 协作者邀请码” · “查看邀请码”
- 权限说明：好友（friend，仅私聊对话）与协作者（collaborator，完整工程沙箱权限）`
	sections = append(sections, accessSection)

	helpText := fmt.Sprintf("# %s · 使用帮助\n\n%s\n\n想切换模型时，直接说“切换到 <模型名称>”，例如“切换到 Gemini 3.8 Flash”。未标注权限的指令可直接使用；标注“仅核心搭档/协作者”的指令需要系统配置授权。群聊中的 slash 指令请先 @ 呆呆；普通对话中 @ 呆呆或直接称呼“呆呆”都可以。AGY 认证指令只能在私聊中使用。", a.Persona.Name, strings.Join(sections, "\n\n"))
	if err := a.transmitInfoCard(ctx, event, "📖 指令速查", helpText); err != nil {
		slog.Warn("superagent: 发送指令帮助失败", "platform", event.Platform, "channel", event.ChannelID, "err", err)
	}
}

func (a *SuperAgent) handleContinueCommand(ctx context.Context, event *SensoryEvent, _ string) {
	continueCtx, continueCancel := context.WithCancel(ctx)
	cancellation := &thinkCancellation{key: thinkKey(event)}
	a.sessionCancels.Store(cancellation, continueCancel)
	defer func() {
		a.sessionCancels.Delete(cancellation)
		continueCancel()
	}()
	unlock := a.lockSession(event)
	defer unlock()
	if continueCtx.Err() != nil || (event.StillValid != nil && !event.StillValid()) {
		return
	}
	continuation, ok := a.takeReplyContinuation(event)
	if !ok {
		a.replyText(continueCtx, event, "当前没有待续发的长回复。")
		return
	}
	if !event.IsPartnerPrivate() {
		continuation = core.RedactSensitiveIPs(continuation)
	}
	sendCtx, cancel := context.WithTimeout(continueCtx, 45*time.Second)
	continuationUtterance := &Utterance{Platform: event.Platform, IsGroup: event.IsGroup, ChannelID: event.ChannelID, QuoteMsgID: event.RawMessageID}
	deliveredText, err := a.transmitTextFallbackWithContinuationContext(sendCtx, continueCtx, continuationUtterance, continuation, event.SenderID)
	cancel()
	if err != nil {
		if continueCtx.Err() == nil {
			a.restoreReplyContinuation(continueCtx, event, continuation)
			slog.Warn("superagent: 续发长回复失败，已保留续发内容", "channel", event.ChannelID, "err", err)
		}
	} else if committer, ok := a.Cognition.(interface {
		CommitDeliveredReply(context.Context, *SensoryEvent, string)
	}); ok {
		commitCtx, cancelCommit := context.WithTimeout(continueCtx, 15*time.Second)
		committer.CommitDeliveredReply(commitCtx, event, deliveredText)
		cancelCommit()
	}
}

func (a *SuperAgent) handleTaskCommand(ctx context.Context, event *SensoryEvent, content string) {
	if err := a.delegationPermissionError(event.Role); err != nil {
		a.replyText(ctx, event, err.Error())
		return
	}
	parts := strings.Fields(content)
	if (len(parts) != 2 && len(parts) != 3) || a.Delegates == nil {
		a.replyText(ctx, event, "用法：/task <工单编号> [页码]")
		return
	}
	page := 1
	if len(parts) == 3 {
		parsedPage, parseErr := strconv.Atoi(parts[2])
		if parseErr != nil || parsedPage < 1 {
			a.replyText(ctx, event, "页码必须是大于 0 的整数。用法：/task <工单编号> [页码]")
			return
		}
		page = parsedPage
	}
	ticket, err := a.Delegates.TicketFor(ctx, event.Platform, event.SenderID, event.ChannelID, parts[1])
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
	pages := core.SplitMarkdownChunks(output, outputPageSize)
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
	if ticket.Status == TicketPending || ticket.Status == TicketRunning || ticket.Status == TicketStopping {
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
	timeRange := ticket.CreatedAt.Format("2006-01-02 15:04:05")
	if ticket.UpdatedAt.After(ticket.CreatedAt) {
		timeRange += " ~ " + ticket.UpdatedAt.Format("15:04:05")
	}
	fmt.Fprintf(&sb, "- **记录时间**：%s\n", timeRange)
	fmt.Fprintf(&sb, "- **输出分页**：第 %d/%d 页\n\n", page, pageCount)

	fence := "```"
	if strings.Contains(pages[page-1], "```") {
		fence = "````"
	}
	sb.WriteString("#### 💻 执行输出\n")
	sb.WriteString(fence + "bash\n")
	sb.WriteString(pages[page-1])
	sb.WriteString("\n" + fence + "\n")

	if page < pageCount {
		fmt.Fprintf(&sb, "\n---\n💡 发送 `/task %s %d` 可查看下一页输出\n", ticket.ID, page+1)
	}
	if err != nil {
		sb.WriteString("\n> ⚠️ 注意：此工单最新状态尚未可靠写入 SQLite，当前显示的是进程内快照。\n")
	}

	a.replyInfo(ctx, event, fmt.Sprintf("🧾 工单详情 · %s", ticket.ID), strings.TrimSpace(sb.String()))
}

func (a *SuperAgent) handleTasksCommand(ctx context.Context, event *SensoryEvent, _ string) {
	if err := a.delegationPermissionError(event.Role); err != nil {
		a.replyText(ctx, event, err.Error())
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

	var activeTickets []*Ticket
	var historyTickets []*Ticket
	for _, ticket := range tickets {
		switch ticket.Status {
		case TicketPending, TicketRunning, TicketStopping:
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

func (a *SuperAgent) handleStatusCommand(ctx context.Context, event *SensoryEvent, _ string) {
	var sections []string
	sections = append(sections, "### 呆呆生命体当前机能状态")
	if a.Channels != nil {
		sections = append(sections, a.Channels.FormatStatus())
	}
	if a.Cognition != nil {
		if mindStatus := strings.TrimSpace(a.Cognition.MindStatus()); mindStatus != "" {
			sections = append(sections, mindStatus)
		}
	}
	sections = append(sections, core.FormatSystemStatusWithPerm(event.IsPartnerPrivate()))
	if event.Role != RoleGuest {
		if a.Workspaces != nil {
			sections = append(sections, a.Workspaces.FormatCurrent(event.ChannelID))
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

func (a *SuperAgent) handleModelCommand(ctx context.Context, event *SensoryEvent, _ string) {
	if !a.checkModelCommandPermission(ctx, event) {
		return
	}
	status := strings.TrimSpace(a.Cognition.MindStatus())
	if status == "" {
		status = "当前没有可用的激活模型状态。"
	}
	a.replyInfo(ctx, event, "🧠 当前模型", status)
}

func (a *SuperAgent) handleModelsCommand(ctx context.Context, event *SensoryEvent, _ string) {
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

func (a *SuperAgent) handleUsageCommand(ctx context.Context, event *SensoryEvent, _ string) {
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

func (a *SuperAgent) checkModelCommandPermission(ctx context.Context, event *SensoryEvent) bool {
	if err := a.delegationPermissionError(event.Role); err != nil {
		a.replyText(ctx, event, err.Error())
		return false
	}
	return a.Cognition != nil
}

func (a *SuperAgent) handleStopCommand(ctx context.Context, event *SensoryEvent, content string) {
	if err := a.delegationPermissionError(event.Role); err != nil {
		a.replyText(ctx, event, err.Error())
		return
	}
	parts := strings.Fields(strings.TrimSpace(content))
	if len(parts) > 1 {
		if len(parts) != 2 || a.Delegates == nil {
			a.replyText(ctx, event, "用法：/stop [工单编号]")
			return
		}
		ticket, _ := a.Delegates.TicketFor(ctx, event.Platform, event.SenderID, event.ChannelID, parts[1])
		if ticket == nil {
			a.replyText(ctx, event, "当前会话中没有找到这张工单。")
			return
		}
		if ticket.Status != TicketPending && ticket.Status != TicketRunning && ticket.Status != TicketStopping {
			a.replyText(ctx, event, fmt.Sprintf("这张工单已经结束，状态：%s。", ticketStatusLabel(ticket.Status)))
			return
		}
		if a.Delegates.AbortTicketForRequester(ticket.ID, event.Platform, event.ChannelID, event.SenderID) > 0 {
			if ticket.Status == TicketPending {
				a.replyText(ctx, event, "已取消这张排队中的工单。")
			} else {
				a.replyText(ctx, event, "已请求停止这张工单；工作者退出后会释放执行容量。")
			}
			return
		}
		current, ok := a.Delegates.GetTicket(ticket.ID)
		if ok && current.Status == TicketStopping {
			a.replyText(ctx, event, "这张工单正在停止；工作者退出后会释放执行容量。")
			return
		}
		a.replyText(ctx, event, "这张工单当前无法取消，请用 /task 查看最新状态。")
		return
	}
	stoppedThinking := a.cancelThinkingRequester(event)
	a.clearReplyContinuation(event)
	aborted, stopping := 0, 0
	if a.Delegates != nil {
		aborted = a.Delegates.AbortByRequester(event.Platform, event.ChannelID, event.SenderID)
		for _, ticket := range a.Delegates.ActiveTicketsFor(event.Platform, event.SenderID, event.ChannelID) {
			if ticket.Status == TicketStopping {
				stopping++
			}
		}
	}
	msg := "当前没有正在运行中的思考任务或后台工单。"
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

func (a *SuperAgent) handleResetCommand(ctx context.Context, event *SensoryEvent, _ string) {
	if err := a.delegationPermissionError(event.Role); err != nil {
		a.replyText(ctx, event, err.Error())
		return
	}
	a.cancelThinkingSession(event)
	unlock := a.lockSession(event)
	defer unlock()
	a.clearReplyContinuation(event)
	currWs, wsName := "", ""
	if a.Workspaces != nil {
		var err error
		currWs, wsName, err = a.Workspaces.GetCurrentChecked(event.ChannelID)
		if err != nil {
			a.replyText(ctx, event, fmt.Sprintf("【会话重开失败】\n无法确认当前工作区，近期历史和 AGY 会话均未重置：%v", err))
			return
		}
	}
	if a.Delegates != nil {
		_ = a.Delegates.AbortByChannel(event.Platform, event.ChannelID)
	}
	chatType := "private"
	if event.IsGroup {
		chatType = "group"
	}
	if err := db.DefaultManager().ResetSessionHistoryInChat(event.Platform, chatType, event.ChannelID); err != nil {
		slog.Error("superagent: 重开会话时清理近期历史失败", "platform", event.Platform, "channelID", event.ChannelID, "err", err)
		a.replyText(ctx, event, "【会话重开失败】\n近期对话历史未能清理，AGY 会话记录未清空；已中断的任务不会自动恢复。请稍后重试。")
		return
	}
	if attachmentCleaner, ok := a.Cognition.(interface{ ClearPendingAttachments(*SensoryEvent) }); ok {
		attachmentCleaner.ClearPendingAttachments(event)
	}
	if a.Delegates != nil {
		if err := a.Delegates.ResetSession(event.ChannelID, currWs); err != nil {
			slog.Error("superagent: 重开会话时清理 AGY 会话失败", "platform", event.Platform, "channelID", event.ChannelID, "workspace", currWs, "err", err)
			a.replyText(ctx, event, fmt.Sprintf("【会话重开未完成】\n近期历史游标已重置，但 AGY 会话未能确认清除：%v。请先检查服务日志，再重试 `/reset`。", err))
			return
		}
	}
	a.replyText(ctx, event, fmt.Sprintf("【当前会话已完成重开】\n已清空工作空间 `%s` (`%s`) 的 agy 会话与近期对话记忆，长期画像与项目文件完好保留。", wsName, currWs))
}

func (a *SuperAgent) handlePwdCommand(ctx context.Context, event *SensoryEvent, _ string) {
	if a.Workspaces == nil {
		a.replyText(ctx, event, "工作空间管理器未配置。")
		return
	}
	unlock := a.lockSession(event)
	status := a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "current", "", a.Delegates)
	unlock()
	a.replyInfo(ctx, event, "📁 当前工作空间", status)
}

func (a *SuperAgent) handleWorkspaceListCommand(ctx context.Context, event *SensoryEvent, _ string) {
	if a.Workspaces == nil {
		a.replyText(ctx, event, "工作空间管理器未配置。")
		return
	}
	unlock := a.lockSession(event)
	list := a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "list", "", a.Delegates)
	unlock()
	a.replyInfo(ctx, event, "📚 工作空间书签", list)
}

func (a *SuperAgent) handleWorkspaceExitCommand(ctx context.Context, event *SensoryEvent, _ string) {
	unlock := a.lockSession(event)
	defer unlock()
	if a.Workspaces == nil {
		a.replyText(ctx, event, "工作空间管理器未配置。")
		return
	}
	a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "exit", "", a.Delegates))
}

func (a *SuperAgent) handleWorkspaceCleanCommand(ctx context.Context, event *SensoryEvent, _ string) {
	unlock := a.lockSession(event)
	defer unlock()
	if a.Workspaces == nil {
		a.replyText(ctx, event, "工作空间管理器未配置。")
		return
	}
	a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "clean", "", a.Delegates))
}

func (a *SuperAgent) handleAGYLoginCommand(ctx context.Context, event *SensoryEvent, content string) {
	if event.IsGroup {
		a.replyText(ctx, event, "AGY 认证链接包含账号授权信息，请在与呆呆的私聊中发起登录。")
		return
	}
	if err := a.delegationPermissionError(event.Role); err != nil {
		a.replyText(ctx, event, err.Error())
		return
	}
	if a.AGYAuth == nil {
		a.replyText(ctx, event, "AGY 认证管理器未挂载。")
		return
	}
	force := strings.HasSuffix(content, "force")
	if !force {
		if status := a.AGYAuth.GetStatus(); status.IsLoggedIn {
			a.replyText(ctx, event, fmt.Sprintf("当前已登录 Google 账号 (%s)。如需重新登录或切换账号，请发送 `/agy logout` 注销后再登录，或发送 `/agy login force` 强制重新认证。", status.Email))
			return
		}
	} else if err := a.AGYAuth.Logout(); err != nil {
		slog.Warn("superagent: 强制重新认证前清理 AGY 凭据失败", "err", err)
		a.replyText(ctx, event, "清理现有 AGY 登录状态失败，未发起重新认证。请检查服务日志后重试。")
		return
	}
	a.replyText(ctx, event, "正在发起 Google 账号认证，请稍候获取授权链接...")
	url, err := a.AGYAuth.StartLogin(ctx)
	if err != nil {
		slog.Warn("superagent: 启动 AGY 登录失败", "err", err)
		a.replyText(ctx, event, "没能启动 AGY 登录流程，请检查服务日志中的具体原因后重试。")
		return
	}
	reply := fmt.Sprintf("【Google Antigravity 登录授权】\n请在浏览器或手机中打开以下链接，登录 Google 账号并完成授权：\n\n```\n%s\n```\n\n授权成功后，页面会显示一串授权码 (Authorization Code)。\n请直接回复：\n`/agy code <授权码>`\n（授权会话 5 分钟内有效）", url)
	a.replyText(ctx, event, reply)
}

func (a *SuperAgent) handleAGYCodeCommand(ctx context.Context, event *SensoryEvent, content string) {
	if event.IsGroup {
		a.replyText(ctx, event, "授权码属于私人凭据，请勿在群聊中发送。请在与呆呆的私聊中提交授权码。")
		return
	}
	if err := a.delegationPermissionError(event.Role); err != nil {
		a.replyText(ctx, event, err.Error())
		return
	}
	if a.AGYAuth == nil {
		a.replyText(ctx, event, "AGY 认证管理器未挂载。")
		return
	}
	code, ok := parseAGYAuthCodeCommand(content)
	if !ok || code == "" {
		a.replyText(ctx, event, "用法：/agy code <授权码>")
		return
	}
	a.replyText(ctx, event, "正在验证并换取凭证，请稍候...")
	email, err := a.AGYAuth.SubmitCode(ctx, code)
	if err != nil {
		slog.Warn("superagent: AGY 授权码交换失败", "err", err)
		a.replyText(ctx, event, "认证未完成。请确认授权码来自最近一次登录链接且尚未过期；若仍失败，请检查服务日志。")
		return
	}
	if a.OnAuthSuccess != nil {
		a.OnAuthSuccess(ctx)
	}
	a.replyText(ctx, event, fmt.Sprintf("【AGY 登录成功】\n已成功绑定 Google 账号：%s！\n心智模型池已自动刷新就绪。", email))
}

func (a *SuperAgent) handleAGYStatusCommand(ctx context.Context, event *SensoryEvent, _ string) {
	if event.IsGroup {
		a.replyText(ctx, event, "AGY 账号状态包含私人信息，请在与呆呆的私聊中查询。")
		return
	}
	if err := a.delegationPermissionError(event.Role); err != nil {
		a.replyText(ctx, event, err.Error())
		return
	}
	if a.AGYAuth == nil {
		a.replyText(ctx, event, "AGY 认证管理器未挂载。")
		return
	}
	status := a.AGYAuth.GetStatus()
	if !status.IsLoggedIn {
		a.replyInfo(ctx, event, "🔐 AGY 认证状态", "【AGY 认证状态】\n当前尚未登录 Google 账号或凭证已失效。可发送 `/agy login` 发起登录。")
		return
	}
	expires := "长期有效"
	if !status.Expiry.IsZero() {
		expires = status.Expiry.Local().Format("2006-01-02 15:04:05")
	}
	a.replyInfo(ctx, event, "🔐 AGY 认证状态", fmt.Sprintf("【AGY 认证状态】\n- 状态：已登录在线\n- 账号：%s\n- Token 有效期至：%s", status.Email, expires))
}

func (a *SuperAgent) handleAGYLogoutCommand(ctx context.Context, event *SensoryEvent, _ string) {
	if event.IsGroup {
		a.replyText(ctx, event, "账号注销操作请在与呆呆的私聊中进行。")
		return
	}
	if err := a.delegationPermissionError(event.Role); err != nil {
		a.replyText(ctx, event, err.Error())
		return
	}
	if a.AGYAuth == nil {
		a.replyText(ctx, event, "AGY 认证管理器未挂载。")
		return
	}
	status := a.AGYAuth.GetStatus()
	if err := a.AGYAuth.Logout(); err != nil {
		slog.Warn("superagent: 注销 AGY 账号失败", "err", err)
		a.replyText(ctx, event, "注销未完成，凭据状态未确认。请检查服务日志后重试。")
		return
	}
	if !status.IsLoggedIn {
		a.replyText(ctx, event, "【AGY 登录已取消】\n当前没有已登录账号；等待中的登录会话已清理。如需使用可发送 `/agy login` 重新认证。")
		return
	}
	hint := ""
	if status.Email != "" {
		hint = fmt.Sprintf(" (%s)", status.Email)
	}
	a.replyText(ctx, event, fmt.Sprintf("【AGY 账号已注销】\n已安全清除 Google 账号凭据%s。如需再次使用可发送 `/agy login` 重新认证。", hint))
}

func (a *SuperAgent) handleWorkspaceRemoveCommand(ctx context.Context, event *SensoryEvent, content string) {
	unlock := a.lockSession(event)
	defer unlock()
	if a.Workspaces == nil {
		a.replyText(ctx, event, "工作空间管理器未配置。")
		return
	}
	target := strings.TrimSpace(strings.TrimPrefix(content, "/workspaces rm "))
	if strings.HasPrefix(content, "/ws rm ") {
		target = strings.TrimSpace(strings.TrimPrefix(content, "/ws rm "))
	}
	a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "remove", target, a.Delegates))
}

func (a *SuperAgent) handleCDCommand(ctx context.Context, event *SensoryEvent, content string) {
	unlock := a.lockSession(event)
	defer unlock()
	if a.Workspaces == nil {
		a.replyText(ctx, event, "工作空间管理器未配置。")
		return
	}
	target := strings.TrimSpace(strings.TrimPrefix(content, "/cd"))
	if target == "" {
		target = "~"
	}
	a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "switch", target, a.Delegates))
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
