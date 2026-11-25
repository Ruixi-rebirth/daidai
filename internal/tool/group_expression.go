package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/groupchat"
)

type GroupExpressionTool struct{ store *groupchat.Store }

func NewGroupExpressionTool(store *groupchat.Store) *GroupExpressionTool {
	return &GroupExpressionTool{store: store}
}

func (*GroupExpressionTool) Name() string { return "manage_group_expressions" }
func (*GroupExpressionTool) Description() string {
	return fmt.Sprintf("管理%s的碎碎念和做梦开关。两项默认关闭、独立开关；用户明确说本群开启/关闭碎碎念或做梦时调用set，仅改提到的字段；说两项都开启或关闭时同时传两个字段。status查看本群状态。碎碎念是结合近期话题的短感想，做梦是带想象语境的小故事；不会立即触发发言，也没有固定时刻广播。", config.BotName())
}
func (*GroupExpressionTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"action":  map[string]any{"type": "string", "enum": []string{"status", "set"}},
		"chatter": map[string]any{"type": "boolean", "description": "本群碎碎念开关；未提及时省略"},
		"dream":   map[string]any{"type": "boolean", "description": "本群梦境联想开关；未提及时省略"},
	}, "required": []string{"action"}}
}
func (t *GroupExpressionTool) Execute(ctx context.Context, raw json.RawMessage) (*core.ToolResult, error) {
	event := entity.SensoryEventFromContext(ctx)
	if !entity.ToolAllowed(t.Name(), event) {
		return &core.ToolResult{Text: "碎碎念和做梦仅由核心搭档在当前群里管理。"}, nil
	}
	var args struct {
		Action string `json:"action"`
		groupchat.Patch
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid group expression arguments")
	}
	switch args.Action {
	case "set":
		if err := t.store.Update(ctx, event.Platform, event.ChannelID, args.Patch); err != nil {
			return nil, err
		}
	case "status":
	default:
		return nil, fmt.Errorf("unknown group expression action")
	}
	state, err := t.store.Get(ctx, event.Platform, event.ChannelID)
	if err != nil {
		return nil, err
	}
	label := func(enabled bool) string {
		if enabled {
			return "开启"
		}
		return "关闭"
	}
	return &core.ToolResult{Text: fmt.Sprintf("本群碎碎念：%s；梦境联想：%s。会结合本群最近的话题择机说一点，安静时保持安静；普通插话照常。", label(state.Chatter), label(state.Dream))}, nil
}
