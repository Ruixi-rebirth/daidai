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

const (
	MindScopePartner = "partner"
	MindScopePublic  = "public"
	MindScopeBoth    = "both"
)

// MindSlotState 单条心智轨道模型配置
type MindSlotState struct {
	Channel   string `json:"channel"`
	Model     string `json:"model"`
	Effort    string `json:"effort,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// ActiveMindState 持久化心智状态（顶层字段为搭档私聊主轨，Public 为显式固定的公用/群聊轨）
type ActiveMindState struct {
	Channel   string         `json:"channel"`
	Model     string         `json:"model"`
	Effort    string         `json:"effort,omitempty"`
	UpdatedAt string         `json:"updated_at"`
	Public    *MindSlotState `json:"public,omitempty"`
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
	Scope   string // 可选作用轨道: "partner" (默认), "public", "both"
}

// SwitchResult 切换结果
type SwitchResult struct {
	Status     string      `json:"status"` // "success", "ambiguous", "error"
	Scope      string      `json:"scope,omitempty"`
	Channel    string      `json:"channel,omitempty"`
	Model      string      `json:"model,omitempty"`
	Name       string      `json:"name,omitempty"`
	Effort     string      `json:"effort,omitempty"`
	Message    string      `json:"message"`
	Candidates []ModelInfo `json:"candidates,omitempty"`
}

// MindRouter 异构多心智开放路由器 (实现 core.LLMProvider，支持搭档私聊主轨与公用沙箱轨双轨独立调度)
type MindRouter struct {
	mu                    sync.RWMutex
	statePath             string
	fallbackChannel       string
	fallbackChannels      []string
	active                ActiveMindState
	public                MindSlotState
	publicPinned          bool
	providers             map[string]core.LLMProvider
	models                []ModelInfo
	discoveryStatus       map[string]ModelDiscoveryStatus
	cooldowns             map[string]time.Time
	providerVersion       uint64
	refreshVersion        uint64
	appliedRefreshVersion map[string]uint64
}

// MindRouterConfig 路由器初始化参数
type MindRouterConfig struct {
	StatePath        string
	FallbackChannel  string
	FallbackChannels []string
}

// NewMindRouter 实例化异构多心智开放路由器
func NewMindRouter(cfg MindRouterConfig) *MindRouter {
	seen := make(map[string]struct{})
	var chain []string
	addChannel := func(ch string) {
		norm := strings.ToUpper(strings.TrimSpace(ch))
		if norm == "" {
			return
		}
		if _, exists := seen[norm]; exists {
			return
		}
		seen[norm] = struct{}{}
		chain = append(chain, norm)
	}
	for _, ch := range cfg.FallbackChannels {
		addChannel(ch)
	}
	addChannel(cfg.FallbackChannel)
	if len(chain) == 0 {
		chain = []string{"API"}
	}

	r := &MindRouter{
		statePath:        cfg.StatePath,
		fallbackChannel:  chain[0],
		fallbackChannels: chain,
		providers:        make(map[string]core.LLMProvider),
		models:           make([]ModelInfo, 0),
		discoveryStatus:  make(map[string]ModelDiscoveryStatus),
		cooldowns:        make(map[string]time.Time),
	}

	if r.statePath == "" {
		r.statePath = "data/state/active_mind.json"
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
	r.reconcilePublicMindLocked(channelKey, models, discoveryFailed)
}

func capEffortValue(effort string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "high", "xhigh", "max":
		return "medium", true
	default:
		return strings.ToLower(strings.TrimSpace(effort)), false
	}
}

func capPublicEffortForModel(m ModelInfo, requested string) (string, bool) {
	if !m.SupportsEffort {
		return "", false
	}
	eff, capped := capEffortValue(requested)
	if eff != "" && isEffortAllowed(m, eff) {
		return eff, capped
	}
	for _, candidate := range []string{"medium", "low"} {
		if isEffortAllowed(m, candidate) {
			return candidate, capped
		}
	}
	if def, _ := capEffortValue(m.DefaultEffort); def != "" && isEffortAllowed(m, def) {
		return def, capped
	}
	if len(m.SupportedEfforts) > 0 {
		first, firstCapped := capEffortValue(m.SupportedEfforts[0])
		if isEffortAllowed(m, first) {
			return first, capped || firstCapped
		}
		return m.SupportedEfforts[0], capped
	}
	return "", capped
}

func publicModelRank(id string) int {
	lower := strings.ToLower(strings.TrimSpace(id))
	switch {
	case strings.Contains(lower, "flash"):
		return 1
	case strings.Contains(lower, "argon") || strings.Contains(lower, "carbon") || strings.Contains(lower, "sonnet"):
		return 2
	case !strings.Contains(lower, "opus") && !strings.Contains(lower, "orchestrator"):
		return 3
	default:
		return 4
	}
}

func (r *MindRouter) selectDefaultPublicMindLocked() MindSlotState {
	if r.providers["AGY"] != nil {
		var best *ModelInfo
		bestRank := 99
		for i := range r.models {
			if !strings.EqualFold(r.models[i].Channel, "AGY") {
				continue
			}
			rank := publicModelRank(r.models[i].ID)
			if best == nil || rank < bestRank {
				best = &r.models[i]
				bestRank = rank
			}
		}
		if best != nil {
			eff, _ := capPublicEffortForModel(*best, best.DefaultEffort)
			return MindSlotState{
				Channel:   best.Channel,
				Model:     best.ID,
				Effort:    eff,
				UpdatedAt: time.Now().Format(time.RFC3339),
			}
		}
	}
	if r.active.Channel != "" && r.active.Model != "" {
		eff, _ := capEffortValue(r.active.Effort)
		for i := range r.models {
			if strings.EqualFold(r.models[i].Channel, r.active.Channel) && strings.EqualFold(r.models[i].ID, r.active.Model) {
				eff, _ = capPublicEffortForModel(r.models[i], r.active.Effort)
				break
			}
		}
		return MindSlotState{
			Channel:   r.active.Channel,
			Model:     r.active.Model,
			Effort:    eff,
			UpdatedAt: r.active.UpdatedAt,
		}
	}
	return MindSlotState{}
}

func (r *MindRouter) reconcilePublicMindLocked(channelKey string, channelModels []ModelInfo, discoveryFailed bool) {
	if !r.publicPinned || r.public.Channel == "" || r.public.Model == "" {
		r.public = r.selectDefaultPublicMindLocked()
		return
	}
	if !strings.EqualFold(r.public.Channel, channelKey) || discoveryFailed || len(channelModels) == 0 {
		return
	}
	var matched *ModelInfo
	for i := range channelModels {
		if strings.EqualFold(channelModels[i].ID, r.public.Model) {
			matched = &channelModels[i]
			break
		}
	}
	previous := r.public
	if matched == nil {
		r.publicPinned = false
		r.public = r.selectDefaultPublicMindLocked()
		r.saveActiveStateLocked(r.public.Channel)
		return
	}
	eff, _ := capPublicEffortForModel(*matched, r.public.Effort)
	if eff != previous.Effort {
		r.public.Effort = eff
		r.public.UpdatedAt = time.Now().Format(time.RFC3339)
		r.saveActiveStateLocked(r.public.Channel)
	}
}

// saveActiveStateLocked persists a recovered active model. Keep the usable
// in-memory state when persistence fails, and make the failure visible.
func (r *MindRouter) saveActiveStateLocked(channel string) {
	if err := r.saveState(); err != nil {
		slog.Warn("活动心智状态已在内存更新，但未能持久化", "channel", channel, "model", r.active.Model, "err", err)
	}
}

// CurrentMind 获取当前搭档私聊主轨激活的心智与思考强度
func (r *MindRouter) CurrentMind() ActiveMindState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st := r.active
	st.Public = nil
	return st
}

// PublicMind 获取当前群聊与非搭档公用轨激活的心智与思考强度
func (r *MindRouter) PublicMind() MindSlotState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.public.Channel != "" && r.public.Model != "" {
		return r.public
	}
	eff, _ := capEffortValue(r.active.Effort)
	return MindSlotState{
		Channel:   r.active.Channel,
		Model:     r.active.Model,
		Effort:    eff,
		UpdatedAt: r.active.UpdatedAt,
	}
}

// HasConfiguredRoute reports whether the selected model is registered and backed by a provider.
func (r *MindRouter) HasConfiguredRoute() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.active.Model != "" && r.providers[r.active.Channel] != nil
}

func resolveDisplayModelName(models []ModelInfo, channel, modelID string) string {
	for _, m := range models {
		if m.ID == modelID && (channel == "" || strings.EqualFold(m.Channel, channel)) {
			if strings.TrimSpace(m.Name) != "" {
				return m.Name
			}
			break
		}
	}
	return modelID
}

func cooldownKey(channel, model string) string {
	return strings.ToUpper(strings.TrimSpace(channel)) + "\x00" + strings.ToLower(strings.TrimSpace(model))
}

func (r *MindRouter) cooldownRemainingLocked(channel, model string, now time.Time) time.Duration {
	if strings.TrimSpace(channel) == "" || strings.TrimSpace(model) == "" || len(r.cooldowns) == 0 {
		return 0
	}
	until, ok := r.cooldowns[cooldownKey(channel, model)]
	if !ok {
		return 0
	}
	rem := until.Sub(now)
	if rem <= 0 {
		return 0
	}
	return rem
}

func (r *MindRouter) modelCooldownRemaining(channel, model string) time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cooldownRemainingLocked(channel, model, time.Now())
}

func (r *MindRouter) setModelCooldown(channel, model string, d time.Duration) {
	if d <= 0 || strings.TrimSpace(channel) == "" || strings.TrimSpace(model) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cooldowns == nil {
		r.cooldowns = make(map[string]time.Time)
	}
	now := time.Now()
	for k, exp := range r.cooldowns {
		if !now.Before(exp) {
			delete(r.cooldowns, k)
		}
	}
	r.cooldowns[cooldownKey(channel, model)] = now.Add(d)
}

func (r *MindRouter) clearModelCooldownLocked(channel, model string) {
	if strings.TrimSpace(channel) == "" || strings.TrimSpace(model) == "" || len(r.cooldowns) == 0 {
		return
	}
	delete(r.cooldowns, cooldownKey(channel, model))
}

// FormatCurrentStatus 格式化完整双轨心智运行状态摘要（供 /model 与搭档视图使用）
func (r *MindRouter) FormatCurrentStatus() string {
	return r.FormatStatusFor(true)
}

// FormatStatusFor 按会话范围格式化心智运行状态：搭档私聊展示双轨完整视图，其他会话仅展示公用轨状态。
func (r *MindRouter) FormatStatusFor(partnerPrivate bool) string {
	now := time.Now()
	r.mu.RLock()
	curr := r.active
	pub := r.public
	if pub.Channel == "" || pub.Model == "" {
		eff, _ := capEffortValue(curr.Effort)
		pub = MindSlotState{Channel: curr.Channel, Model: curr.Model, Effort: eff}
	}
	models := cloneModelInfos(r.models)
	var fallbackSummary []string
	for _, ch := range r.fallbackChannels {
		if strings.EqualFold(curr.Channel, ch) {
			continue
		}
		if prov := r.providers[ch]; prov != nil {
			fallbackSummary = append(fallbackSummary, fmt.Sprintf("%s (`%s`)", ch, providerModelID(prov)))
		}
	}
	currCooldown := r.cooldownRemainingLocked(curr.Channel, curr.Model, now)
	pubCooldown := r.cooldownRemainingLocked(pub.Channel, pub.Model, now)
	r.mu.RUnlock()

	if curr.Model == "" && curr.Channel == "" {
		return ""
	}

	var sb strings.Builder
	if partnerPrivate {
		modelName := resolveDisplayModelName(models, curr.Channel, curr.Model)
		sb.WriteString("### 首选聊天模型（搭档私聊）\n")
		fmt.Fprintf(&sb, "🤖 模型：%s\n", modelName)
		if modelName != curr.Model {
			fmt.Fprintf(&sb, "模型 ID：`%s`\n", curr.Model)
		}
		fmt.Fprintf(&sb, "🔌 通道：%s\n", curr.Channel)
		if curr.Effort != "" {
			fmt.Fprintf(&sb, "💭 思考强度：%s\n", curr.Effort)
		}
		if currCooldown > 0 {
			fmt.Fprintf(&sb, "⏳ 冷却状态：剩余 %s（期间自动走备用通道）\n", currCooldown.Round(time.Second))
		}

		if pub.Model != "" {
			pubName := resolveDisplayModelName(models, pub.Channel, pub.Model)
			sb.WriteString("\n### 公用模型（bwrap 沙箱）\n")
			fmt.Fprintf(&sb, "🤖 模型：%s\n", pubName)
			if pubName != pub.Model {
				fmt.Fprintf(&sb, "模型 ID：`%s`\n", pub.Model)
			}
			fmt.Fprintf(&sb, "🔌 通道：%s\n", pub.Channel)
			if pub.Effort != "" {
				fmt.Fprintf(&sb, "💭 思考强度：%s（上限 medium）\n", pub.Effort)
			}
			if pubCooldown > 0 {
				fmt.Fprintf(&sb, "⏳ 冷却状态：剩余 %s（期间自动走备用通道）\n", pubCooldown.Round(time.Second))
			}
		}

		if len(fallbackSummary) > 0 {
			fmt.Fprintf(&sb, "\n### 备用与轻量分类模型\n兜底链：%s\n首选通道调用失败/配额耗尽兜底及高频分类门控时按序使用；非搭档私聊的 AGY 调用运行于 bwrap 内核隔离沙箱。\n", strings.Join(fallbackSummary, " → "))
		}
	} else {
		pubName := resolveDisplayModelName(models, pub.Channel, pub.Model)
		sb.WriteString("### 公用模型（bwrap 沙箱）\n")
		fmt.Fprintf(&sb, "🤖 模型：%s\n", pubName)
		if pubName != pub.Model {
			fmt.Fprintf(&sb, "模型 ID：`%s`\n", pub.Model)
		}
		fmt.Fprintf(&sb, "🔌 通道：%s\n", pub.Channel)
		if pub.Effort != "" {
			fmt.Fprintf(&sb, "💭 思考强度：%s\n", pub.Effort)
		}
		if pubCooldown > 0 {
			fmt.Fprintf(&sb, "⏳ 冷却状态：剩余 %s（期间自动走备用通道）\n", pubCooldown.Round(time.Second))
		}
	}
	sb.WriteString("\n以上为路由设置，不代表每一轮实际使用的模型。")
	return strings.TrimSpace(sb.String())
}
