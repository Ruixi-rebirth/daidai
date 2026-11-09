package core

import (
	"context"
	"encoding/json"
)

// ToolResult 工具执行返回的结果
type ToolResult struct {
	Text      string // 给 LLM 二次整理或提炼的文本结果
	ImageData []byte // 直接交付给用户的渲染图片数据（如排版卡片 PNG）
}

// Tool 统一的原子功能工具接口
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]any // 符合 JSON Schema 规范的参数定义
	Execute(ctx context.Context, args json.RawMessage) (*ToolResult, error)
}
