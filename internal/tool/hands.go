package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"

	"daidai/internal/briefing"
	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/engine/vision"
	"daidai/internal/entity"
	"daidai/internal/groupchat"
	"daidai/internal/media"
	"daidai/internal/provider/llm"
)

// HandsRegistry 原生双手实现 (实现 entity.Hands，保持确定性工具顺序以稳定大模型 Prompt Cache)
type HandsRegistry struct {
	mu    sync.RWMutex
	tools map[string]core.Tool
	order []string
}

// HandsConfig 双手挂载配置
type HandsConfig struct {
	AGYCommand       string
	Router           *llm.MindRouter
	Delegates        *entity.Delegates
	Immune           *entity.ImmuneSystem
	Workspaces       *entity.WorkspaceManager
	AttachmentInbox  *media.Inbox
	Database         *db.Manager
	OCIConfig        *config.OCIConfig
	OCIClient        *OCIClient
	CloudflareConfig *config.CloudflareConfig
	Briefing         *briefing.Service
	BriefingOwners   map[string]string
	GroupExpressions *groupchat.Store
	Vision           *vision.Engine
}

// NewHandsWithConfig 按配置挂载原子双手
func NewHandsWithConfig(cfg HandsConfig) *HandsRegistry {
	h := &HandsRegistry{
		tools: make(map[string]core.Tool),
	}
	h.Register(NewRenderTool())
	if cfg.GroupExpressions != nil {
		h.Register(NewGroupExpressionTool(cfg.GroupExpressions))
	}
	if cfg.Briefing != nil {
		h.Register(NewBriefingTool(cfg.Briefing, cfg.BriefingOwners, false))
		h.Register(NewBriefingTool(cfg.Briefing, cfg.BriefingOwners, true))
	}
	h.Register(NewSysInfoTool())
	h.Register(NewMemoryTool(cfg.Database, cfg.Router))
	if cfg.Database != nil && cfg.Immune != nil {
		h.Register(NewAccessTool(cfg.Database, cfg.Immune))
	}
	h.Register(NewChatSummaryTool(cfg.Database, cfg.Router))
	h.Register(NewWebSearchToolWithCommand(cfg.Router, cfg.AGYCommand))
	h.Register(NewReadLinkToolWithCommand(cfg.Router, cfg.AGYCommand))
	h.Register(NewScreenshotTool())
	if cfg.Vision != nil {
		h.Register(NewImageAnalysisTool(cfg.Vision))
	}
	h.Register(NewSingboxLinksTool(cfg.Immune))
	if cfg.Workspaces != nil {
		h.Register(NewWorkspaceTool(cfg.Workspaces, cfg.Immune, cfg.Delegates))
	}
	h.Register(NewNixSearchTool())
	if cfg.Router != nil {
		h.Register(NewModelSwitchTool(cfg.Router, cfg.Immune))
	}
	if cfg.Delegates != nil {
		h.Register(NewDelegateToolWithInbox(cfg.Delegates, cfg.Immune, cfg.Workspaces, cfg.AttachmentInbox))
		h.Register(NewFaceToolWithCommand(cfg.Delegates, cfg.AGYCommand, filepath.Join("data", "assets", "faces"), cfg.Immune))
		outDir := filepath.Join("data", "workspace", "sandbox")
		if cfg.Workspaces != nil {
			outDir = cfg.Workspaces.SandboxDir()
		}
		h.Register(NewImageToolWithCommand(cfg.Delegates, cfg.AGYCommand, outDir, cfg.Immune))
	}
	if cfg.OCIClient != nil {
		timezone := ""
		if cfg.OCIConfig != nil {
			timezone = cfg.OCIConfig.Timezone
		}
		h.Register(NewOCIBillTool(cfg.OCIClient, timezone))
		h.Register(NewOCIIPToolWithCloudflare(cfg.OCIClient, cfg.Immune, cfg.CloudflareConfig))
	} else if cfg.OCIConfig != nil && cfg.OCIConfig.Enabled {
		if ociClient, err := NewOCIClient(context.Background(), *cfg.OCIConfig); err == nil {
			h.Register(NewOCIBillTool(ociClient, cfg.OCIConfig.Timezone))
			h.Register(NewOCIIPToolWithCloudflare(ociClient, cfg.Immune, cfg.CloudflareConfig))
		} else {
			slog.Warn("初始化 OCI 客户端失败，跳过 OCI 相关工具注册", "err", err)
		}
	}
	return h
}

func (h *HandsRegistry) Register(t core.Tool) {
	if t == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	name := t.Name()
	if _, exists := h.tools[name]; !exists {
		h.order = append(h.order, name)
	}
	h.tools[name] = t
}

func (h *HandsRegistry) Get(name string) (core.Tool, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	t, ok := h.tools[name]
	return t, ok
}

func (h *HandsRegistry) Definitions() []core.ToolDefinition {
	h.mu.RLock()
	defer h.mu.RUnlock()

	defs := make([]core.ToolDefinition, 0, len(h.order))
	for _, name := range h.order {
		if t, ok := h.tools[name]; ok {
			defs = append(defs, core.ToolDefinition{
				Type: "function",
				Function: core.ToolFunctionDetail{
					Name:        t.Name(),
					Description: t.Description(),
					Parameters:  t.Parameters(),
				},
			})
		}
	}
	return defs
}

func (h *HandsRegistry) Execute(ctx context.Context, name string, rawArgs json.RawMessage) (result *core.ToolResult, execErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = nil
			execErr = fmt.Errorf("tool %q failed internally", name)
			slog.Error("hands: 工具执行发生 panic", "name", name, "panic", recovered)
		}
	}()
	if ctx == nil {
		return nil, fmt.Errorf("hands: tool execution requires a context")
	}
	event := entity.SensoryEventFromContext(ctx)
	privilegedRole := event != nil && (event.Role == entity.RolePartner || event.Role == entity.RoleCollaborator)
	systemAccess := entity.HasSystemToolAccess(ctx, name)
	if event == nil && !systemAccess {
		return nil, fmt.Errorf("权限不足：缺少用户身份上下文，拒绝调用工具 %q", name)
	}
	if event != nil && !systemAccess && !entity.ToolAllowedInConversation(name, event) {
		return nil, fmt.Errorf("权限不足：当前会话不允许调用工具 %q", name)
	}
	if !privilegedRole && !systemAccess && !entity.GuestToolAllowed(name) {
		return nil, fmt.Errorf("权限不足：访客不能调用工具 %q", name)
	}

	h.mu.RLock()
	t, ok := h.tools[name]
	h.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("hands: tool %q not found", name)
	}

	return t.Execute(ctx, rawArgs)
}
