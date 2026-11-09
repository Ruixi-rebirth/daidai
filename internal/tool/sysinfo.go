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
	return "获取呆呆机器人宿主机实时硬件与系统运行状态（系统运行时间、内存占用、协程数、CPU 核心、宿主机节点）。当用户询问机器硬件状态、运行健康度或服务器负载时调用此工具（查询项目工作空间请使用 switch_workspace 工具）。"
}

func (t *SysInfoTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *SysInfoTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	event := entity.SensoryEventFromContext(ctx)
	return &core.ToolResult{
		Text: core.FormatSystemStatusWithPerm(event.IsPartnerPrivate()),
	}, nil
}
