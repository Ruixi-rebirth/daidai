package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/engine/render"
	"daidai/internal/entity"
)

type RenderTool struct{}

func NewRenderTool() *RenderTool {
	return &RenderTool{}
}

func (t *RenderTool) Name() string {
	return "render_card"
}

func (t *RenderTool) Description() string {
	return "将 Markdown 文档、代码或表格排版渲染为卡片长图。"
}

func (t *RenderTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content": map[string]any{
				"type":        "string",
				"description": "需要排版渲染的 Markdown 文本内容，可包含各级标题、清单、代码块、引用等",
			},
		},
		"required": []string{"content"},
	}
}

type renderArgs struct {
	Content string `json:"content"`
}

func (t *RenderTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	var args renderArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, fmt.Errorf("invalid render_card args: %w", err)
	}

	content := strings.TrimSpace(args.Content)
	if event := entity.SensoryEventFromContext(ctx); !event.IsPartnerPrivate() {
		content = core.RedactSensitiveIPs(content)
	}
	if content == "" {
		return nil, fmt.Errorf("render content cannot be empty")
	}

	slog.Info("render_tool: 开始渲染卡片", "contentLen", len(content))

	renderCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	pngBytes, err := render.RenderCard(renderCtx, content)
	if err != nil {
		slog.Error("render_tool: 卡片渲染失败", "err", err)
		return nil, fmt.Errorf("render card failed: %w", err)
	}

	slog.Info("render_tool: 卡片图片渲染成功", "bytes", len(pngBytes))

	return &core.ToolResult{
		Text:      "卡片图片已成功渲染生成",
		ImageData: pngBytes,
	}, nil
}
