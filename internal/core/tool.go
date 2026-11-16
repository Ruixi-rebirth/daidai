package core

import (
	"context"
	"encoding/json"
)

// ToolResult 工具执行返回的结果
type ToolResult struct {
	Text             string   // 给 LLM 二次整理或提炼的文本结果
	FallbackText     string   // 图片无法发送时给用户的文字版本；不进入模型上下文
	ImageData        []byte   // 直接交付给用户的渲染图片数据（如排版卡片 PNG）
	Images           [][]byte // 一次工具调用需要交付的多张图片
	ImageOnly        bool     // 已准备视图交付时不附加模型正文，Caption 仍照常发送
	ImageSources     []string // 与 Images 对齐的逐图语义正文
	Caption          string   // 确定交付的索引等文字，不交给模型改写
	InlineImages     bool
	OnDelivered      func(context.Context) // 仅在通道确认交付后更新展示上下文
	OnDeliveryFailed func(context.Context, error)
}

// Tool 统一的原子功能工具接口
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]any // 符合 JSON Schema 规范的参数定义
	Execute(ctx context.Context, args json.RawMessage) (*ToolResult, error)
}
