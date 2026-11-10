package entity

import (
	"context"
	"encoding/json"
	"time"

	"daidai/internal/core"
)

// Channel 全双工物理通信通道接口 (收发合一，彻底隔离物理协议)
type Channel interface {
	Name() string
	Start(ctx context.Context, onSensory func(event *SensoryEvent)) error
	Transmit(ctx context.Context, u *Utterance) error
	Stop(ctx context.Context) error
}

// Hands 原生原子双手系统接口
type Hands interface {
	Register(t core.Tool)
	Definitions() []core.ToolDefinition
	Execute(ctx context.Context, name string, rawArgs json.RawMessage) (*core.ToolResult, error)
}

// Worker DSH 隔离沙箱工作者接口 (异构自驱执行单元)
type Worker interface {
	Name() string
	Description() string
	ExecuteTicket(ctx context.Context, t *Ticket) (string, error)
}

// Memory 记忆与经验中枢接口 (零前缀规范)
type Memory interface {
	WorkingContext(ctx context.Context, event *SensoryEvent, limit int) []core.ChatMessage
	RecallEpisodic(ctx context.Context, event *SensoryEvent) string
	Commit(ctx context.Context, event *SensoryEvent, replyText string)
}

// Cognition 双轨认知大脑中枢接口
type Cognition interface {
	Think(ctx context.Context, event *SensoryEvent) (*Utterance, error)
	MindStatus() string
	ModelList() string
	ModelUsage() string
}

// AGYAuthStatusInfo AGY 认证凭据摘要
type AGYAuthStatusInfo struct {
	IsLoggedIn bool
	Email      string
	Expiry     time.Time
}

// AGYAuthenticator AGY 命令行 Google OAuth 交互式认证管理接口
type AGYAuthenticator interface {
	GetStatus() AGYAuthStatusInfo
	StartLogin(ctx context.Context) (string, error)
	SubmitCode(ctx context.Context, code string) (string, error)
	Cancel()
}
