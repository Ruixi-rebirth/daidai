package chat

import (
	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/engine/vision"
	"daidai/internal/entity"
	"daidai/internal/groupchat"
	"daidai/internal/media"
)

const (
	maxToolCallsPerThink   = 8
	maxToolArgumentsBytes  = 64 << 10
	maxAssistantReplyRunes = 5000
)

// Cortex 具体双轨认知大脑实现 (实现 entity.Cognition)
type Cortex struct {
	llm              core.LLMProvider
	persona          *entity.Persona
	memory           entity.Memory
	hands            entity.Hands
	vision           *vision.Engine
	workspaces       *entity.WorkspaceManager
	inbox            *media.Inbox
	groupExpressions *groupchat.Store
	drafts           *db.Manager
}

// CortexConfig 认知大脑初始化配置
type CortexConfig struct {
	LLM              core.LLMProvider
	Persona          *entity.Persona
	Memory           entity.Memory
	Hands            entity.Hands
	Vision           *vision.Engine
	Workspaces       *entity.WorkspaceManager
	Inbox            *media.Inbox
	GroupExpressions *groupchat.Store
	Drafts           *db.Manager
}

// NewCortex 按配置实例化认知大脑
func NewCortex(cfg CortexConfig) *Cortex {
	return &Cortex{
		llm:              cfg.LLM,
		persona:          cfg.Persona,
		memory:           cfg.Memory,
		hands:            cfg.Hands,
		vision:           cfg.Vision,
		workspaces:       cfg.Workspaces,
		inbox:            cfg.Inbox,
		groupExpressions: cfg.GroupExpressions,
		drafts:           cfg.Drafts,
	}
}
