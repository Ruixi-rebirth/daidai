package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	Router          *llm.MindRouter
	Delegates       *entity.Delegates
	Immune          *entity.ImmuneSystem
	Workspaces      *entity.WorkspaceManager
	EnableNixSearch bool
	OCIConfig       *config.OCIConfig
}

// NewHandsWithConfig 按配置挂载原子双手
func NewHandsWithConfig(cfg HandsConfig) *HandsRegistry {
	h := &HandsRegistry{
		tools: make(map[string]core.Tool),
	}
	h.Register(NewRenderTool())
	h.Register(NewSysInfoTool())
	h.Register(NewMemoryTool(nil))
	h.Register(NewWebSearchTool(cfg.Router))
	h.Register(NewReadLinkTool())
	h.Register(NewScreenshotTool())
	if cfg.Workspaces != nil {
		h.Register(NewWorkspaceTool(cfg.Workspaces, cfg.Immune))
	}
	if cfg.EnableNixSearch {
		h.Register(NewNixSearchTool())
	}
	if cfg.Router != nil {
		h.Register(NewModelSwitchTool(cfg.Router, cfg.Immune))
	}
	if cfg.Delegates != nil {
		h.Register(NewDelegateToolWithWorkspace(cfg.Delegates, cfg.Immune, cfg.Workspaces))
		h.Register(NewFaceTool(cfg.Delegates))
		h.Register(NewImageTool(cfg.Delegates))
	}
	if cfg.OCIConfig != nil && cfg.OCIConfig.Enabled {
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
	h.mu.RLock()
	t, ok := h.tools[name]
	h.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("hands: tool %q not found", name)
	}

	return t.Execute(ctx, rawArgs)
}
