package tool

import (
	"context"
	"encoding/json"

	"daidai/internal/core"
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
	return "获取呆呆的主机标识和机器人进程状态，包括进程运行时间、Go 堆已分配内存、Go 运行时向系统申请的内存、Goroutine 数、可用逻辑 CPU、系统与架构；搭档私聊还会看到启动目录和公网 IP。当用户询问机器人运行状态时调用此工具；它不提供主机总内存或系统开机时长。查询项目工作空间请使用 switch_workspace 工具。"
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
	return &core.ToolResult{
		Text: core.FormatSystemStatusWithPerm(allowSensitive),
	}, nil
}
