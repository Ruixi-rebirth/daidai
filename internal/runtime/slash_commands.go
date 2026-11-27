package runtime

import (
	"context"
	"daidai/internal/entity"
	"fmt"
	"log/slog"
	"strings"

	"daidai/internal/config"
)

type slashCommand struct {
	section  string
	usage    string
	help     string
	access   string
	platform string
	adapter  bool
	match    func(string) bool
	handle   func(context.Context, *entity.SensoryEvent, string)
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
		{section: "对话与会话", usage: "/continue [任务编号] · 继续", help: "续发未发送内容或继续已保存的长任务草稿", match: func(content string) bool {
			_, ok := parseContinueCommand(content)
			return ok
		}, handle: a.handleContinueCommand},
		{section: "对话与会话", usage: "/stop [工单编号]", help: "中断当前思考；私聊中还可中断后台工单", access: "仅核心搭档", match: prefix("/stop"), handle: a.handleStopCommand},
		{section: "对话与会话", usage: "/reset · /new · /clear", help: "清空当前会话近期对话记忆（私聊同时重开 AGY 工程会话）", access: "仅核心搭档", match: exact("/reset", "/new", "/clear", "/ws clear", "/ws reset"), handle: a.handleResetCommand},
		{section: "状态与模型", usage: "/status · /sysinfo", help: "查看运行状态与系统指标（私聊含工作空间与活跃工单）", access: "仅核心搭档", match: exact("/status", "/sysinfo"), handle: a.handleStatusCommand},
		{section: "状态与模型", usage: "/model", help: "查看当前对话使用的模型；此命令不会切换模型", access: "仅核心搭档", match: exact("/model"), handle: a.handleModelCommand},
		{section: "状态与模型", usage: "/models", help: "列出可用模型", access: "仅核心搭档", match: exact("/models"), handle: a.handleModelsCommand},
		{section: "状态与模型", usage: "/usage", help: "查看模型用量", access: "仅核心搭档", match: exact("/usage"), handle: a.handleUsageCommand},
		{section: "状态与模型", usage: "/voice [fast|multi]", help: "查看或切换语音识别模式（fast 极速 / multi 多语精准）", access: "仅核心搭档", match: prefix("/voice"), handle: a.handleVoiceCommand},
		{section: "工程工单", usage: "/tasks", help: "查看近期工程工单", access: "仅私聊；核心搭档", match: exact("/tasks"), handle: a.handleTasksCommand},
		{section: "工程工单", usage: "/task [ID] [页码]", help: "查看最近或指定工单的实时进度与输出", access: "仅私聊；核心搭档", match: prefix("/task"), handle: a.handleTaskCommand},
		{section: "工作空间", usage: "/pwd", help: "查看当前工作空间", access: "仅私聊；核心搭档", match: exact("/pwd"), handle: a.handlePwdCommand},
		{section: "工作空间", usage: "/cd <目标>", help: "切换工作空间", access: "仅私聊；核心搭档", match: prefix("/cd"), handle: a.handleCDCommand},
		{section: "工作空间", usage: "/ws · /workspaces", help: "列出已纳管的工作空间", access: "仅私聊；核心搭档", match: exact("/ws", "/workspaces"), handle: a.handleWorkspaceListCommand},
		{section: "工作空间", usage: "/ws rm <名称>", help: "移除工作空间书签", access: "仅私聊；核心搭档", match: prefix("/ws rm", "/workspaces rm"), handle: a.handleWorkspaceRemoveCommand},
		{section: "工作空间", usage: "/ws exit", help: "退出当前工作空间", access: "仅私聊；核心搭档", match: exact("/ws exit", "/workspaces exit"), handle: a.handleWorkspaceExitCommand},
		{section: "工作空间", usage: "/ws clean · /clean", help: "清理默认 sandbox 临时文件", access: "仅私聊；核心搭档", match: exact("/ws clean", "/clean"), handle: a.handleWorkspaceCleanCommand},
		{section: "AGY 认证", usage: "/agy login [force]", help: "发起或强制重新进行 Google 认证", access: "仅私聊；核心搭档", match: exact("/agy login", "/agy auth", "/agy login force", "/agy login --force"), handle: a.handleAGYLoginCommand},
		{section: "AGY 认证", usage: "/agy code <授权码>", help: "提交 Google 授权码", access: "仅私聊；核心搭档", match: func(content string) bool { _, ok := parseAGYAuthCodeCommand(content); return ok }, handle: a.handleAGYCodeCommand},
		{section: "AGY 认证", usage: "/agy status", help: "查看 AGY 认证状态", access: "仅私聊；核心搭档", match: exact("/agy status"), handle: a.handleAGYStatusCommand},
		{section: "AGY 认证", usage: "/agy logout", help: "注销 AGY 并清除凭据", access: "仅私聊；核心搭档", match: exact("/agy logout"), handle: a.handleAGYLogoutCommand},
		{section: "群聊管理", usage: "/bot mark @成员 · /bot unmark @成员", help: "标记或取消标记群里的其他机器人，避免机器人之间互相触发", access: "仅核心搭档；QQ 群聊", platform: "qq", adapter: true, match: prefix("/bot")},
	}
}

// IsAdapterSlashCommand reports whether a platform adapter owns the handling
// of a command registered in the shared slash-command catalog.
func IsAdapterSlashCommand(platform, content string) bool {
	for _, command := range slashCommandRegistry(&SuperAgent{}) {
		if command.adapter && (command.platform == "" || command.platform == platform) && command.match != nil && command.match(content) {
			return true
		}
	}
	return false
}

func (a *SuperAgent) runRegisteredSlashCommand(ctx context.Context, event *entity.SensoryEvent, content string) bool {
	for _, command := range slashCommandRegistry(a) {
		if command.handle != nil && command.match != nil && (command.platform == "" || command.platform == event.Platform) && command.match(content) {
			command.handle(ctx, event, content)
			return true
		}
	}
	return false
}

func (a *SuperAgent) handleHelpCommand(ctx context.Context, event *entity.SensoryEvent, _ string) {
	sections := make([]string, 0)
	sectionNames := make([]string, 0)
	sectionLines := make(map[string][]string)
	for _, command := range slashCommandRegistry(a) {
		if command.platform != "" && command.platform != event.Platform {
			continue
		}
		if event != nil && event.IsGroup && strings.Contains(command.access, "仅私聊") {
			continue
		}
		if event.Platform == "telegram" && strings.HasPrefix(command.usage, "/voice") {
			continue
		}
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
		icon := map[string]string{"对话与会话": "💬", "状态与模型": "📊", "工程工单": "🧾", "工作空间": "📁", "AGY 认证": "🔐", "群聊管理": "🤖"}[name]
		sections = append(sections, fmt.Sprintf("### %s %s\n%s", icon, name, strings.Join(sectionLines[name], "\n")))
	}
	accessSection := `### 👥 用户与权限管理（仅核心搭档 QQ 私聊）
- 查看申请 / 用户：“查看申请” · “查看已授权用户” · “查看 [昵称/序号] 的权限”
- 授予权限：“将第 1 个申请者加为好友 / 协作者” · “将 [昵称] 加为好友”
- 修改权限：“将第 1 个用户修改为好友 / 协作者” · “把 [昵称] 的权限修改为好友”
- 删除 / 拒绝：“删除第 1 个用户” · “撤销 [昵称] 的权限” · “拒绝第 1 个申请”
- 邀请码管理：“生成一个好友 / 协作者邀请码” · “查看邀请码”
- 权限说明：好友（friend，日常私聊对话）与协作者（collaborator，私聊对话、动态表情与群画图免限额）`
	if event.Platform == "qq" && (event == nil || !event.IsGroup) {
		sections = append(sections, accessSection)
	}

	name := config.BotName()
	groupCommandHint := fmt.Sprintf("群聊中的 slash 指令请先 @ %s。", name)
	if event.Platform == "qq" {
		groupCommandHint = fmt.Sprintf("群聊中的普通 slash 指令请先 @ %s；/bot 机器人管理指令可直接发送，仅核心搭档可用。", name)
	}
	helpText := fmt.Sprintf("# %s · 使用帮助\n\n%s\n\n想切换模型时，直接说“切换到 <模型名称>”，例如“切换到 Gemini 3.8 Flash”。未标注权限的指令可直接使用；标注权限的指令按说明授权使用。%s 普通对话中 @ %s或直接称呼“%s”都可以。工作空间、工程工单与 AGY 认证指令仅在私聊中开放。", name, strings.Join(sections, "\n\n"), groupCommandHint, name, name)
	if err := a.transmitInfo(ctx, event, "📖 指令速查", helpText, true); err != nil {
		slog.Warn("superagent: 发送指令帮助失败", "platform", event.Platform, "channel", event.ChannelID, "err", err)
	}
}
