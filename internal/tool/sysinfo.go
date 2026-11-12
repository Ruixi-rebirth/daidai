package tool

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/engine/render"
	"daidai/internal/entity"
)

type SysInfoTool struct{}

func NewSysInfoTool() *SysInfoTool {
	return &SysInfoTool{}
}

func (t *SysInfoTool) Name() string {
	return "get_system_status"
}

func (t *SysInfoTool) Description() string {
	return "获取呆呆的主机标识、机器人进程状态和当前消息通道连接状态，包括进程运行时间、Go 堆已分配内存、Go 运行时向系统申请的内存、Goroutine 数、逻辑 CPU，以及 Linux 主机 CPU 使用率、内存、根分区、系统运行时间和负载。QQ 状态检查的是 WebSocket 收消息网关；主动发送 API 的可用性不会在此探测，网关断开不代表主动发送一定不可用。状态会以图片卡片展示，并保留文字回退。搭档私聊还会看到启动目录和公网 IP。当用户询问机器人运行状态时调用此工具。查询项目工作空间请使用 switch_workspace 工具。"
}

func (t *SysInfoTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *SysInfoTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	event := entity.SensoryEventFromContext(ctx)
	allowSensitive := event != nil && event.IsPartnerPrivate()
	text := core.FormatSystemStatusWithPerm(allowSensitive)
	if channelStatus := strings.TrimSpace(entity.ChannelStatusFromContext(ctx)); channelStatus != "" {
		text += "\n\n" + channelStatus
	}
	result := &core.ToolResult{Text: text, FallbackText: text}
	if ctx == nil {
		ctx = context.Background()
	}
	renderCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	imageData, err := render.RenderCard(renderCtx, "# 🖥️ 呆呆运行状态\n\n"+text)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Warn("sysinfo: 状态卡片渲染失败，回退为文字", "err", err)
		return result, nil
	}
	result.ImageData = imageData
	return result, nil
}
