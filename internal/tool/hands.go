package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/entity"
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
	AGYCommand      string
	Router          *llm.MindRouter
	Delegates       *entity.Delegates
	Immune          *entity.ImmuneSystem
	Workspaces      *entity.WorkspaceManager
	EnableNixSearch bool
	OCIConfig       *config.OCIConfig
	OCIClient       *OCIClient
}

// NewHandsWithConfig 按配置挂载原子双手
func NewHandsWithConfig(cfg HandsConfig) *HandsRegistry {
	h := &HandsRegistry{
		tools: make(map[string]core.Tool),
	}
	h.Register(NewRenderTool())
	h.Register(NewSysInfoTool())
	h.Register(NewMemoryTool(nil))
	h.Register(NewWebSearchToolWithCommand(cfg.Router, cfg.AGYCommand))
	h.Register(NewReadLinkTool())
	h.Register(NewScreenshotTool())
	h.Register(NewSingboxLinksTool(cfg.Immune))
	if cfg.Workspaces != nil {
		h.Register(NewWorkspaceTool(cfg.Workspaces, cfg.Immune, cfg.Delegates))
	}
	if cfg.EnableNixSearch {
		h.Register(NewNixSearchTool())
	}
	if cfg.Router != nil {
		h.Register(NewModelSwitchTool(cfg.Router, cfg.Immune))
	}
	if cfg.Delegates != nil {
		h.Register(NewDelegateToolWithWorkspace(cfg.Delegates, cfg.Immune, cfg.Workspaces))
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
		h.Register(NewOCIIPTool(cfg.OCIClient, cfg.Immune))
	} else if cfg.OCIConfig != nil && cfg.OCIConfig.Enabled {
		if ociClient, err := NewOCIClient(context.Background(), *cfg.OCIConfig); err == nil {
			h.Register(NewOCIBillTool(ociClient, cfg.OCIConfig.Timezone))
			h.Register(NewOCIIPTool(ociClient, cfg.Immune))
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

func (h *HandsRegistry) Execute(ctx context.Context, name string, rawArgs json.RawMessage) (*core.ToolResult, error) {
	if ctx == nil {
		return nil, fmt.Errorf("hands: tool execution requires a context")
	}
	event := entity.SensoryEventFromContext(ctx)
	privilegedRole := event != nil && (event.Role == entity.RolePartner || event.Role == entity.RoleCollaborator)
	if !privilegedRole && !entity.HasSystemToolAccess(ctx) && !entity.GuestToolAllowed(name) {
		if event == nil {
			return nil, fmt.Errorf("权限不足：缺少用户身份上下文，拒绝调用工具 %q", name)
		}
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
