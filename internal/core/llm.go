package core

import (
	"context"
	"strings"
)

// Role 角色
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolDefinition 声明给 LLM 的工具元数据 (符合 OpenAI JSON Schema 标准)
type ToolDefinition struct {
	Type     string             `json:"type"` // 固定为 "function"
	Function ToolFunctionDetail `json:"function"`
}

// ToolFunctionDetail 工具函数的具体信息
type ToolFunctionDetail struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ToolCall LLM 返回的工具调用请求
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction 工具函数调用实参
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatMessage 单条对话消息
type ChatMessage struct {
	RecoveryContent  string            `json:"-"` // Tool evidence retained only when saving a draft.
	GroupReplyTarget string            `json:"-"` // Group continuation judgement metadata.
	PageProgress     *PageReadProgress `json:"-"` // Used to report progress if synthesis cannot finish.
	Role             Role
	Content          string
	ToolCalls        []ToolCall // 当 Role 为 assistant 时，可能包含工具调用指令
	ToolCallID       string     // 当 Role 为 tool 时，对应 ToolCall.ID
	Name             string     // 工具名称
}

// ChatOptions 模型生成参数
type ChatOptions struct {
	// Temperature is an explicit value whenever ChatOptions is non-nil; zero is valid.
	Temperature   float32
	MaxTokens     int
	ReadOnly      bool // 供路由层与提供方启用内核沙箱与只读受限执行路径
	ReasoningOnly bool // 请求提供方为本轮使用只读推理模式（禁止工具调用与文件变更）
	FastRoute     bool // 高频轻量分类/门控请求，优先走低延迟 API 兜底通道
}

// ChatResponse 模型响应，支持纯文本与工具调用
type ChatResponse struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
}

// OutputLimitReached recognizes provider finish reasons for truncated output.
func OutputLimitReached(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length", "max_tokens", "max_output_tokens":
		return true
	default:
		return false
	}
}

// LLMProvider 大模型驱动接口 (统一 OpenAI / DeepSeek / Gemini / 本地模型)
type LLMProvider interface {
	Name() string
	Chat(ctx context.Context, messages []ChatMessage, opts *ChatOptions) (string, error)
	ChatWithTools(ctx context.Context, messages []ChatMessage, tools []ToolDefinition, opts *ChatOptions) (*ChatResponse, error)
}

// EmbeddingProvider 稠密语义向量编码接口 (支持 BAAI/bge-m3 等标准 /embeddings 端点)
type EmbeddingProvider interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// IdentifiedEmbeddingProvider returns a stable, non-secret identity for the
// exact model configuration used to create a vector.
type IdentifiedEmbeddingProvider interface {
	EmbeddingIdentity() string
	EmbedIdentified(ctx context.Context, text string) ([]float32, string, error)
}
