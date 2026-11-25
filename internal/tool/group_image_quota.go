package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/groupchat"
)

// GroupImageQuotaTool lets the partner manage per-group daily drawing quotas via natural language.
type GroupImageQuotaTool struct {
	store *groupchat.ImageQuotaStore
}

// NewGroupImageQuotaTool constructs the group image quota management tool.
func NewGroupImageQuotaTool(store *groupchat.ImageQuotaStore) *GroupImageQuotaTool {
	return &GroupImageQuotaTool{store: store}
}

func (t *GroupImageQuotaTool) Name() string {
	return "manage_group_image_quota"
}

func (t *GroupImageQuotaTool) Description() string {
	return "管理当前群的普通群友（非伙伴与协作者）每日画图上限（仅伙伴可在群聊中使用，伙伴与协作者本人均不受次数限制）。支持查看今日状态、设置每日次数上限（默认 10 次）、设为 0 关闭普通群友画图、设为 -1 取消限制。"
}

func (t *GroupImageQuotaTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"status", "set"},
				"description": "status=查看当前群今日画图配额与使用情况；set=修改当前群的普通群友每日画图上限",
			},
			"daily_limit": map[string]any{
				"type":        "integer",
				"description": "仅 action=set 时需要：正整数表示每日全群普通群友总上限（默认10），0 表示禁止普通群友在群内画图，-1 表示不限制次数（伙伴与协作者始终豁免）",
			},
		},
		"required": []string{"action"},
	}
}

type groupImageQuotaArgs struct {
	Action     string `json:"action"`
	DailyLimit *int   `json:"daily_limit,omitempty"`
}

func formatGroupImageQuotaStatus(status groupchat.ImageQuotaStatus) string {
	switch {
	case status.Disabled:
		return fmt.Sprintf("当前群普通群友画图已关闭（每日上限 0，日期 %s；伙伴与协作者不受此限制）。", status.Date)
	case status.Unlimited:
		return fmt.Sprintf("当前群普通群友画图不限次数（日期 %s；伙伴与协作者亦豁免）。", status.Date)
	default:
		return fmt.Sprintf("当前群普通群友每日画图上限为 %d 次，今日（%s）已用 %d 次，剩余 %d 次（伙伴与协作者不计入也不受限）。",
			status.Limit, status.Date, status.Used, status.Remaining)
	}
}

func (t *GroupImageQuotaTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	if t.store == nil {
		return nil, fmt.Errorf("group image quota store not configured")
	}
	event := entity.SensoryEventFromContext(ctx)
	if !entity.ToolAllowed(t.Name(), event) {
		return &core.ToolResult{Text: "仅伙伴可在群聊内管理本群画图配额。"}, nil
	}

	var args groupImageQuotaArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, fmt.Errorf("invalid manage_group_image_quota args: %w", err)
	}

	switch strings.ToLower(strings.TrimSpace(args.Action)) {
	case "status":
		status, err := t.store.Get(ctx, event.Platform, event.ChannelID, time.Now())
		if err != nil {
			return &core.ToolResult{Text: fmt.Sprintf("查询本群画图配额失败：%v", err)}, nil
		}
		return &core.ToolResult{Text: formatGroupImageQuotaStatus(status)}, nil
	case "set":
		if args.DailyLimit == nil {
			return nil, fmt.Errorf("daily_limit is required when action is set")
		}
		status, err := t.store.SetLimit(ctx, event.Platform, event.ChannelID, *args.DailyLimit, time.Now())
		if err != nil {
			return &core.ToolResult{Text: fmt.Sprintf("设置本群画图配额失败：%v", err)}, nil
		}
		return &core.ToolResult{Text: "已更新本群画图配额。" + formatGroupImageQuotaStatus(status)}, nil
	default:
		return nil, fmt.Errorf("unsupported action %q", args.Action)
	}
}
