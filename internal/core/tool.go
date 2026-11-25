package core

import (
	"context"
	"encoding/json"
	"strings"
)

// ToolResult 工具执行返回的结果
type ToolResult struct {
	Text              string   // 给 LLM 二次整理或提炼的文本结果
	DirectReply       bool     // 单次工具调用即可用 Text 结束本轮，无需模型再改写
	FallbackText      string   // 图片无法发送时给用户的文字版本；不进入模型上下文
	ImageData         []byte   // 直接交付给用户的渲染图片数据（如排版卡片 PNG）
	Images            [][]byte // 一次工具调用需要交付的多张图片
	VideoData         []byte   // 直接交付给用户的视频数据（标准化 MP4）
	Videos            [][]byte // 一次工具调用需要交付的多段视频
	ImageOnly         bool     // 已准备视图交付时不附加模型正文，Caption 仍照常发送
	ImageSources      []string // 与 Images 对齐的逐图语义正文
	MediaHistory      string   // 仅在媒体确认送达后写入聊天历史的说明
	PageProgress      *PageReadProgress
	Caption           string // 确定交付的索引等文字，不交给模型改写
	InlineImages      bool
	FixedPresentation bool                  // 声明该结果已确定展示形式（如代码、凭据等纯文本），跳过自动转长图排版
	OnDelivered       func(context.Context) // 仅在通道确认交付后更新展示上下文
	OnDeliveryFailed  func(context.Context, error)
}

type PageReadProgress struct {
	URL    string
	Status string
}

// InlineAsset is a rendered image placed at a specific point in a Markdown reply.
type InlineAsset struct {
	Placeholder  string
	Reference    string
	Alt          string
	Fallback     string
	SemanticText string
	Data         []byte
}

// InlineAssetText replaces local image references with text for quotes or fallback.
func InlineAssetText(markdown string, assets []InlineAsset, semantic bool) string {
	for _, asset := range assets {
		replacement := asset.Fallback
		if semantic && asset.SemanticText != "" {
			replacement += "\n" + asset.SemanticText
		}
		markdown = strings.ReplaceAll(markdown, asset.Placeholder, replacement)
	}
	return markdown
}

// Tool 统一的原子功能工具接口
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]any // 符合 JSON Schema 规范的参数定义
	Execute(ctx context.Context, args json.RawMessage) (*ToolResult, error)
}
