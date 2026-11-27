package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/media"
)

// UserAvatarTool fetches and directly sends a conversation member's original avatar image
// without invoking image generation or consuming drawing quota.
type UserAvatarTool struct {
	sources *media.Sources
	avatars *AvatarResolver
}

// NewUserAvatarTool constructs the direct user avatar sending tool.
func NewUserAvatarTool(sources *media.Sources, avatars *AvatarResolver) *UserAvatarTool {
	return &UserAvatarTool{
		sources: sources,
		avatars: avatars,
	}
}

func (t *UserAvatarTool) Name() string {
	return "get_user_avatar"
}

func (t *UserAvatarTool) Description() string {
	return "直接获取并发送指定用户（或当前发言人）的原始头像原图。仅当用户明确要求“把某人头像发出来/看看原图”时调用；若要评价头像内容请用 analyze_images，若要改图或二创请用 generate_image。"
}

func (t *UserAvatarTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target_user": map[string]any{
				"type":        "string",
				"description": "目标用户：支持 \"self\"（当前发言人）、\"他\"/\"她\"（引用消息作者）、群友昵称（如 \"月\"）或用户 ID；留空默认当前发言人",
			},
		},
		"additionalProperties": false,
	}
}

// DefinitionForEvent only exposes get_user_avatar on platforms with a registered avatar media source.
func (t *UserAvatarTool) DefinitionForEvent(event *entity.SensoryEvent) (core.ToolDefinition, bool) {
	if event == nil || t.avatars == nil || !t.avatars.SupportsPlatform(event.Platform) {
		return core.ToolDefinition{}, false
	}
	return core.ToolDefinition{
		Type: "function",
		Function: core.ToolFunctionDetail{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		},
	}, true
}

func (t *UserAvatarTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	event := entity.SensoryEventFromContext(ctx)
	if event == nil {
		return nil, fmt.Errorf("缺少当前会话上下文")
	}
	if t.avatars == nil || t.sources == nil {
		return &core.ToolResult{Text: "当前未启用用户头像读取能力。"}, nil
	}

	var args struct {
		TargetUser string `json:"target_user,omitempty"`
	}
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, fmt.Errorf("invalid get_user_avatar args: %w", err)
		}
	}

	displayName, avatarRef, err := t.avatars.Resolve(ctx, event, args.TargetUser)
	if err != nil {
		return &core.ToolResult{Text: err.Error()}, nil
	}

	raw, err := media.FetchImage(ctx, t.sources, avatarRef, 15*time.Second)
	if err != nil {
		return &core.ToolResult{Text: fmt.Sprintf("获取「%s」的头像失败：%v", displayName, err)}, nil
	}
	normalized, _, err := normalizeImageData(ctx, raw)
	if err != nil {
		return &core.ToolResult{Text: fmt.Sprintf("解析「%s」的头像图片失败：%v", displayName, err)}, nil
	}

	return &core.ToolResult{
		ImageData:         normalized,
		ImageOnly:         true,
		FixedPresentation: true,
	}, nil
}
