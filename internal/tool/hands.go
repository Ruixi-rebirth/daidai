package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"daidai/internal/briefing"
	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/engine/vision"
	"daidai/internal/entity"
	"daidai/internal/groupchat"
	"daidai/internal/media"
	"daidai/internal/oci"
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
	OCIClient        *oci.OCIClient
	CloudflareConfig *config.CloudflareConfig
	Briefing         *briefing.Service
	BriefingOwners   map[string]string
	GroupExpressions *groupchat.Store
	Vision           *vision.Engine
	Embedder         core.EmbeddingProvider
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
	var imageQuotaStore *groupchat.ImageQuotaStore
	if cfg.Database != nil {
		imageQuotaStore = &groupchat.ImageQuotaStore{Database: cfg.Database}
		h.Register(NewGroupImageQuotaTool(imageQuotaStore))
	}
	if cfg.Briefing != nil {
		h.Register(NewBriefingTool(cfg.Briefing, cfg.BriefingOwners, false))
		h.Register(NewBriefingTool(cfg.Briefing, cfg.BriefingOwners, true))
	}
	h.Register(NewSysInfoTool())
	memTool := NewMemoryTool(cfg.Database, cfg.Router)
	memTool.SetEmbedder(cfg.Embedder)
	h.Register(memTool)
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
	h.Register(oci.NewSingboxLinksTool(cfg.Immune))
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
		imgTool := NewImageToolWithCommand(cfg.Delegates, cfg.AGYCommand, outDir, cfg.Immune)
		imgTool.SetImageQuotaStore(imageQuotaStore)
		h.Register(imgTool)
		h.Register(NewVideoToolWithCommand(cfg.Delegates, cfg.AGYCommand, outDir, cfg.Immune))
	}
	if cfg.OCIClient != nil {
		timezone := ""
		if cfg.OCIConfig != nil {
			timezone = cfg.OCIConfig.Timezone
		}
		h.Register(oci.NewOCIBillTool(cfg.OCIClient, timezone))
		h.Register(oci.NewOCIIPToolWithCloudflare(cfg.OCIClient, cfg.Immune, cfg.CloudflareConfig))
	} else if cfg.OCIConfig != nil && cfg.OCIConfig.Enabled {
		if ociClient, err := oci.NewOCIClient(context.Background(), *cfg.OCIConfig); err == nil {
			h.Register(oci.NewOCIBillTool(ociClient, cfg.OCIConfig.Timezone))
			h.Register(oci.NewOCIIPToolWithCloudflare(ociClient, cfg.Immune, cfg.CloudflareConfig))
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
	systemAccess := entity.HasSystemToolAccess(ctx, name)
	if !systemAccess && !entity.ToolAllowed(name, event) {
		if event == nil {
			return nil, fmt.Errorf("权限不足：缺少用户身份上下文，拒绝调用工具 %q", name)
		}
		return nil, fmt.Errorf("权限不足：当前会话或角色不允许调用工具 %q", name)
	}

	h.mu.RLock()
	t, ok := h.tools[name]
	h.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("hands: tool %q not found", name)
	}
	if !systemAccess && event != nil {
		markOperationalToolTurn(event, name, rawArgs)
	}

	return t.Execute(ctx, rawArgs)
}

func markOperationalToolTurn(event *entity.SensoryEvent, name string, rawArgs json.RawMessage) {
	switch name {
	case "switch_workspace", "get_system_status", "singbox_links",
		"manage_group_expressions", "manage_group_image_quota", "manage_geek_briefing":
		event.SkipFactExtraction = true
		event.SkipVectorIndex = true
		event.EphemeralControlTurn = true
	case "switch_model":
		event.SkipFactExtraction = true
		event.SkipVectorIndex = true
		var probe struct {
			Action string `json:"action"`
			Model  string `json:"model"`
			Effort string `json:"effort"`
		}
		_ = json.Unmarshal(rawArgs, &probe)
		act := strings.ToLower(strings.TrimSpace(probe.Action))
		if act == "switch" || act == "current" || act == "usage" ||
			(act == "" && (strings.TrimSpace(probe.Model) != "" || strings.TrimSpace(probe.Effort) != "")) {
			event.EphemeralControlTurn = true
		}
	case "manage_access", "oci_ip", "oci_bill", "delegate_ticket", "manage_memory", "summarize_chat", "get_geek_briefing":
		event.SkipFactExtraction = true
		event.SkipVectorIndex = true
	}
}
