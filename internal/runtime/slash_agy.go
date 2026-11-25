package runtime

import (
	"context"
	"daidai/internal/entity"
	"fmt"
	"log/slog"
	"strings"

	"daidai/internal/config"
)

func (a *SuperAgent) handleAGYLoginCommand(ctx context.Context, event *entity.SensoryEvent, content string) {
	if event.IsGroup {
		a.replyText(ctx, event, fmt.Sprintf("AGY 认证链接包含账号授权信息，请在与%s的私聊中发起登录。", config.BotName()))
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

func (a *SuperAgent) handleAGYCodeCommand(ctx context.Context, event *entity.SensoryEvent, content string) {
	if event.IsGroup {
		a.replyText(ctx, event, fmt.Sprintf("授权码属于私人凭据，请勿在群聊中发送。请在与%s的私聊中提交授权码。", config.BotName()))
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

func (a *SuperAgent) handleAGYStatusCommand(ctx context.Context, event *entity.SensoryEvent, _ string) {
	if event.IsGroup {
		a.replyText(ctx, event, fmt.Sprintf("AGY 账号状态包含私人信息，请在与%s的私聊中查询。", config.BotName()))
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

func (a *SuperAgent) handleAGYLogoutCommand(ctx context.Context, event *entity.SensoryEvent, _ string) {
	if event.IsGroup {
		a.replyText(ctx, event, fmt.Sprintf("账号注销操作请在与%s的私聊中进行。", config.BotName()))
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
