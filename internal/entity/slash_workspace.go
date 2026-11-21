package entity

import (
	"context"
	"strings"
)

func (a *SuperAgent) handlePwdCommand(ctx context.Context, event *SensoryEvent, _ string) {
	if a.Workspaces == nil {
		a.replyText(ctx, event, "工作空间管理器未配置。")
		return
	}
	unlock := a.lockSession(event)
	status := a.Workspaces.ExecuteAction(event.Platform, event.ChannelID, event.Role, a.Immune, "current", "", a.Delegates)
	unlock()
	a.replyInfo(ctx, event, "📁 当前工作空间", status)
}

func (a *SuperAgent) handleWorkspaceListCommand(ctx context.Context, event *SensoryEvent, _ string) {
	if a.Workspaces == nil {
		a.replyText(ctx, event, "工作空间管理器未配置。")
		return
	}
	unlock := a.lockSession(event)
	list := a.Workspaces.ExecuteAction(event.Platform, event.ChannelID, event.Role, a.Immune, "list", "", a.Delegates)
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
	a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.Platform, event.ChannelID, event.Role, a.Immune, "exit", "", a.Delegates))
}

func (a *SuperAgent) handleWorkspaceCleanCommand(ctx context.Context, event *SensoryEvent, _ string) {
	unlock := a.lockSession(event)
	defer unlock()
	if a.Workspaces == nil {
		a.replyText(ctx, event, "工作空间管理器未配置。")
		return
	}
	a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.Platform, event.ChannelID, event.Role, a.Immune, "clean", "", a.Delegates))
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
	a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.Platform, event.ChannelID, event.Role, a.Immune, "remove", target, a.Delegates))
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
	a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.Platform, event.ChannelID, event.Role, a.Immune, "switch", target, a.Delegates))
}
