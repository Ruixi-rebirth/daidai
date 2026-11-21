package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/config"
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
	return fmt.Sprintf("查询%s的进程、主机资源和消息通道状态。当用户询问运行状态时调用；QQ 状态只表示 WebSocket 收消息网关，不代表主动发送 API 状态。搭档私聊还会显示启动目录和公网 IP。", config.BotName())
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
	imageData, err := render.RenderCard(renderCtx, fmt.Sprintf("# 🖥️ %s运行状态\n\n%s", config.BotName(), text))
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
