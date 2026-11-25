package llm

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"daidai/internal/core"
)

// ModelConfigurable 允许心智路由器动态调整其运行模型的驱动接口
type ModelConfigurable interface {
	SetModel(model string)
}

// EffortConfigurable 允许心智路由器动态调整其思考强度 (推理深度) 的驱动接口
type EffortConfigurable interface {
	SetEffort(effort string)
}

// ModelDiscoverer 支持动态自发现可用模型池的驱动接口 (零硬编码)
type ModelDiscoverer interface {
	DiscoverModels(ctx context.Context) []ModelInfo
}

// ModelDiscoveryWithError lets providers distinguish an empty model pool from
// a transient discovery failure while retaining ModelDiscoverer compatibility.
type ModelDiscoveryWithError interface {
	DiscoverModelsWithError(ctx context.Context) ([]ModelInfo, error)
}

type ModelDiscoveryStatus struct {
	LastAttempt time.Time
	LastSuccess time.Time
	Failed      bool
}

// UsageDiscoverer 支持查询通道配额与实时用量信息的驱动接口
type UsageDiscoverer interface {
	GetUsage(ctx context.Context) (string, error)
}

// ModelInfo 模型定义元数据 (由各通道运行时动态探测或注册生成)
type ModelInfo struct {
	ID               string   `json:"id"`
	Channel          string   `json:"channel"` // 通道标识 (例如 "API", "AGY")
	Name             string   `json:"name"`
	Description      string   `json:"description,omitempty"`
	SupportsEffort   bool     `json:"supports_effort"`
	SupportedEfforts []string `json:"supported_efforts,omitempty"`
	DefaultEffort    string   `json:"default_effort,omitempty"`
}

// ActiveMindState 持久化心智状态
type ActiveMindState struct {
	Channel   string `json:"channel"`
	Model     string `json:"model"`
	Effort    string `json:"effort,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

// ChannelRegistration 通道驱动与模型池注册描述
type ChannelRegistration struct {
	Channel  string           // 通道名称，例如 "API", "AGY"
	Provider core.LLMProvider // 实现 core.LLMProvider 的具体驱动 (若实现 ModelDiscoverer 则自动发现模型)
	Models   []ModelInfo      // 可选显式传入的模型列表；若留空则自动调用 Provider.DiscoverModels 动态探测
}

// SwitchRequest 切换请求
type SwitchRequest struct {
	Target  string // 目标模型 ID 或关键字
	Channel string // 可选指定通道类型
	Effort  string // 可选指定思考强度 (low, medium, high, xhigh, max)
}

// SwitchResult 切换结果
type SwitchResult struct {
	Status     string      `json:"status"` // "success", "ambiguous", "error"
	Channel    string      `json:"channel,omitempty"`
	Model      string      `json:"model,omitempty"`
	Name       string      `json:"name,omitempty"`
	Effort     string      `json:"effort,omitempty"`
	Message    string      `json:"message"`
	Candidates []ModelInfo `json:"candidates,omitempty"`
}

// MindRouter 异构多心智开放路由器 (实现 core.LLMProvider，支持动态模型自发现)
type MindRouter struct {
	mu                    sync.RWMutex
	statePath             string
	fallbackChannel       string
	active                ActiveMindState
	providers             map[string]core.LLMProvider
	models                []ModelInfo
	discoveryStatus       map[string]ModelDiscoveryStatus
	providerVersion       uint64
	refreshVersion        uint64
	appliedRefreshVersion map[string]uint64
}

// MindRouterConfig 路由器初始化参数
type MindRouterConfig struct {
	StatePath       string
	FallbackChannel string
}

// NewMindRouter 实例化异构多心智开放路由器
func NewMindRouter(cfg MindRouterConfig) *MindRouter {
	r := &MindRouter{
		statePath:       cfg.StatePath,
		fallbackChannel: strings.ToUpper(strings.TrimSpace(cfg.FallbackChannel)),
		providers:       make(map[string]core.LLMProvider),
		models:          make([]ModelInfo, 0),
		discoveryStatus: make(map[string]ModelDiscoveryStatus),
	}

	if r.statePath == "" {
		r.statePath = "data/state/active_mind.json"
	}
	if r.fallbackChannel == "" {
		r.fallbackChannel = "API"
	}

	if err := r.loadState(); err != nil {
		slog.Info("未发现历史心智状态，等待挂载通道后自动初始化", "err", err)
	}

	return r
}

// RegisterChannel 注册一个心智通道，若未显式传入 Models 且驱动实现了 ModelDiscoverer，则自动探测可用模型池
func (r *MindRouter) RegisterChannel(reg ChannelRegistration) {
	channelKey := strings.ToUpper(strings.TrimSpace(reg.Channel))
	if channelKey == "" || reg.Provider == nil {
		return
	}

	models := reg.Models
	discoveryAttempted := false
	discoveryFailed := false
	if len(models) == 0 {
		if disc, ok := reg.Provider.(ModelDiscoveryWithError); ok {
			discoveryAttempted = true
			var err error
			models, err = disc.DiscoverModelsWithError(context.Background())
			discoveryFailed = err != nil
			if err != nil {
				slog.Warn("model router: 初始模型探测失败", "channel", channelKey, "err", err)
			}
		} else if disc, ok := reg.Provider.(ModelDiscoverer); ok {
			discoveryAttempted = true
			models = disc.DiscoverModels(context.Background())
			discoveryFailed = len(models) == 0
			if discoveryFailed {
				slog.Warn("model router: 初始模型探测未返回可用模型，保留已有清单", "channel", channelKey)
			}
		}
	}
	models = normalizeModelInfos(models, channelKey)
	if discoveryAttempted && len(models) == 0 {
		discoveryFailed = true
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.providers[channelKey] = reg.Provider
	r.providerVersion++

	// Re-registering a channel replaces its model pool instead of accumulating
	// stale or duplicate entries from the previous provider instance.
	if discoveryAttempted {
		status := r.discoveryStatus[channelKey]
		status.LastAttempt = time.Now()
		status.Failed = discoveryFailed
		if !discoveryFailed {
			status.LastSuccess = status.LastAttempt
		}
		r.discoveryStatus[channelKey] = status
	} else {
		// An explicit model pool is authoritative; do not retain a failure
		// status from a previous discoverable provider registered on this key.
		delete(r.discoveryStatus, channelKey)
	}
	if !discoveryFailed {
		keptModels := r.models[:0]
		for _, existing := range r.models {
			if !strings.EqualFold(existing.Channel, channelKey) {
				keptModels = append(keptModels, existing)
			}
		}
		r.models = append(keptModels, models...)
	} else {
		if !hasModelsForChannel(r.models, channelKey) {
			r.models = appendUniqueModels(r.models, models, channelKey)
		}
	}

	if r.active.Channel == "" || r.active.Model == "" {
		if len(models) > 0 {
			first := models[0]
			r.active = ActiveMindState{
				Channel:   channelKey,
				Model:     first.ID,
				Effort:    first.DefaultEffort,
				UpdatedAt: time.Now().Format(time.RFC3339),
			}
			r.saveActiveStateLocked(channelKey)
		}
	}

	if r.active.Channel == channelKey {
		if !discoveryFailed && len(models) > 0 {
			var matched *ModelInfo
			for i := range models {
				if strings.EqualFold(models[i].ID, r.active.Model) {
					matched = &models[i]
					break
				}
			}
			if matched == nil {
				// 持久化的旧模型在当前探测池中已不存在，自动回退至该通道首个可用模型
				first := models[0]
				r.active.Model = first.ID
				r.active.Effort = first.DefaultEffort
				r.active.UpdatedAt = time.Now().Format(time.RFC3339)
				r.saveActiveStateLocked(channelKey)
			} else if !matched.SupportsEffort && r.active.Effort != "" {
				r.active.Effort = ""
				r.saveActiveStateLocked(channelKey)
			} else if matched.SupportsEffort && r.active.Effort != "" && !isEffortAllowed(*matched, r.active.Effort) {
				r.active.Effort = matched.DefaultEffort
				r.saveActiveStateLocked(channelKey)
			}
		}
		r.syncProviderLocked(channelKey, r.active.Model, r.active.Effort)
	}
}

// saveActiveStateLocked persists a recovered active model. Keep the usable
// in-memory state when persistence fails, and make the failure visible.
func (r *MindRouter) saveActiveStateLocked(channel string) {
	if err := r.saveState(); err != nil {
		slog.Warn("活动心智状态已在内存更新，但未能持久化", "channel", channel, "model", r.active.Model, "err", err)
	}
}

// CurrentMind 获取当前激活的心智与思考强度
func (r *MindRouter) CurrentMind() ActiveMindState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.active
}

// HasConfiguredRoute reports whether the selected model is registered and backed by a provider.
func (r *MindRouter) HasConfiguredRoute() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.active.Model != "" && r.providers[r.active.Channel] != nil
}

// FormatCurrentStatus 格式化当前激活的心智运行状态摘要
func (r *MindRouter) FormatCurrentStatus() string {
	r.mu.RLock()
	curr := r.active
	models := cloneModelInfos(r.models)
	fallbackChannel := r.fallbackChannel
	fallbackProvider := r.providers[fallbackChannel]
	r.mu.RUnlock()

	if curr.Model == "" && curr.Channel == "" {
		return ""
	}

	modelName := curr.Model
	for _, m := range models {
		if m.ID == curr.Model && (curr.Channel == "" || strings.EqualFold(m.Channel, curr.Channel)) {
			if strings.TrimSpace(m.Name) != "" {
				modelName = m.Name
			}
			break
		}
	}

	var sb strings.Builder
	sb.WriteString("### 首选聊天模型\n")
	fmt.Fprintf(&sb, "🤖 模型：%s\n", modelName)
	if modelName != curr.Model {
		fmt.Fprintf(&sb, "模型 ID：`%s`\n", curr.Model)
	}
	fmt.Fprintf(&sb, "🔌 通道：%s\n", curr.Channel)
	if curr.Effort != "" {
		fmt.Fprintf(&sb, "💭 思考强度：%s\n", curr.Effort)
	}
	if fallbackProvider != nil && !strings.EqualFold(curr.Channel, fallbackChannel) {
		fmt.Fprintf(&sb, "\n### 备用与轻量分类模型\n模型：`%s`\n通道：%s\n首选通道调用失败/配额耗尽兜底及高频分类门控时使用；非搭档私聊的 AGY 调用运行于 bwrap 内核隔离沙箱。\n", providerModelID(fallbackProvider), fallbackChannel)
	}
	sb.WriteString("\n以上为路由设置，不代表每一轮实际使用的模型。")
	return strings.TrimSpace(sb.String())
}
