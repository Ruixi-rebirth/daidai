package core

import "context"

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
	Role       Role
	Content    string
	ToolCalls  []ToolCall // 当 Role 为 assistant 时，可能包含工具调用指令
	ToolCallID string     // 当 Role 为 tool 时，对应 ToolCall.ID
	Name       string     // 工具名称
}

// ChatOptions 模型生成参数
type ChatOptions struct {
	// Temperature is an explicit value whenever ChatOptions is non-nil; zero is valid.
	Temperature float32
	MaxTokens   int
	ReadOnly    bool // 标记受限访客请求，供路由层与提供方选择受限执行路径；不单独构成权限边界
}

// ChatResponse 模型响应，支持纯文本与工具调用
type ChatResponse struct {
	Content   string
	ToolCalls []ToolCall
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
