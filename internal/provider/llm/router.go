package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
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
	fallbackAvailable := r.providers[fallbackChannel] != nil
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
	sb.WriteString("🧠 当前模型路由\n")
	fmt.Fprintf(&sb, "🤖 激活模型：%s\n", modelName)
	fmt.Fprintf(&sb, "🔌 通道：%s\n", curr.Channel)
	if curr.Effort != "" {
		fmt.Fprintf(&sb, "💭 思考强度：%s\n", curr.Effort)
	}
	if fallbackAvailable && !strings.EqualFold(curr.Channel, fallbackChannel) {
		fmt.Fprintf(&sb, "🔁 首选通道调用失败时，可能将本轮内容交由 %s 通道处理；这不会改变首选设置。\n", fallbackChannel)
	}
	return strings.TrimSpace(sb.String())
}

// ListModels 列出所有已探测/注册的可用模型定义
func (r *MindRouter) ListModels() []ModelInfo {
	return r.ListModelsContext(context.Background())
}

// CachedModels returns the current discovery snapshot without refreshing it.
// Tool schemas use this method so prompt construction never starts an
// unbounded network or CLI discovery call.
func (r *MindRouter) CachedModels() []ModelInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneModelInfos(r.models)
}

func (r *MindRouter) ModelDiscoveryStatus(channel string) (ModelDiscoveryStatus, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	status, ok := r.discoveryStatus[strings.ToUpper(strings.TrimSpace(channel))]
	return status, ok
}

// ListModelsContext 列出模型定义，并在需要时使用调用方的上下文刷新模型池。
func (r *MindRouter) ListModelsContext(ctx context.Context) []ModelInfo {
	if ctx == nil {
		return r.CachedModels()
	}
	r.mu.RLock()
	needRefresh := len(r.models) == 0
	r.mu.RUnlock()

	if needRefresh {
		r.RefreshModels(ctx)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneModelInfos(r.models)
}

func cloneModelInfos(models []ModelInfo) []ModelInfo {
	cloned := make([]ModelInfo, len(models))
	for i, model := range models {
		cloned[i] = model
		cloned[i].SupportedEfforts = append([]string(nil), model.SupportedEfforts...)
	}
	return cloned
}

// normalizeModelInfos removes duplicate IDs within one provider while merging
// metadata such as effort options that may arrive in separate discovery rows.
func normalizeModelInfos(models []ModelInfo, channel string) []ModelInfo {
	result := make([]ModelInfo, 0, len(models))
	indexes := make(map[string]int, len(models))
	for _, model := range models {
		model.ID = strings.TrimSpace(model.ID)
		model.Name = strings.TrimSpace(model.Name)
		if model.ID == "" {
			continue
		}
		model.Channel = channel
		if model.Name == "" {
			model.Name = model.ID
		}
		key := strings.ToLower(model.ID)
		index, exists := indexes[key]
		if !exists {
			model.SupportedEfforts = uniqueEfforts(model.SupportedEfforts)
			result = append(result, model)
			indexes[key] = len(result) - 1
			continue
		}

		merged := &result[index]
		merged.SupportsEffort = merged.SupportsEffort || model.SupportsEffort
		merged.SupportedEfforts = uniqueEfforts(append(merged.SupportedEfforts, model.SupportedEfforts...))
		if len(merged.SupportedEfforts) > 0 {
			merged.SupportsEffort = true
		}
		if merged.DefaultEffort == "" {
			merged.DefaultEffort = model.DefaultEffort
		}
		if merged.Description == "" {
			merged.Description = model.Description
		}
		if (merged.Name == merged.ID || merged.Name == "") && model.Name != "" {
			merged.Name = model.Name
		}
	}
	return result
}

func uniqueEfforts(efforts []string) []string {
	result := make([]string, 0, len(efforts))
	seen := make(map[string]struct{}, len(efforts))
	for _, effort := range efforts {
		effort = strings.TrimSpace(effort)
		if effort == "" {
			continue
		}
		key := strings.ToLower(effort)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, effort)
	}
	return result
}

func appendUniqueModels(models, candidates []ModelInfo, channel string) []ModelInfo {
	for _, candidate := range candidates {
		found := false
		for _, existing := range models {
			if strings.EqualFold(existing.Channel, channel) && strings.EqualFold(existing.ID, candidate.ID) {
				found = true
				break
			}
		}
		if !found {
			models = append(models, candidate)
		}
	}
	return models
}

func hasModelsForChannel(models []ModelInfo, channel string) bool {
	for _, model := range models {
		if strings.EqualFold(model.Channel, channel) {
			return true
		}
	}
	return false
}

// RefreshModels 重新触发所有已挂载通道的模型探测并刷新模型池
func (r *MindRouter) RefreshModels(ctx context.Context) {
	if ctx == nil || ctx.Err() != nil {
		return
	}
	r.mu.Lock()
	r.refreshVersion++
	refreshVersion := r.refreshVersion
	providerVersion := r.providerVersion
	type registeredProvider struct {
		channel  string
		provider core.LLMProvider
	}
	providers := make([]registeredProvider, 0, len(r.providers))
	for chKey, prov := range r.providers {
		providers = append(providers, registeredProvider{channel: chKey, provider: prov})
	}
	r.mu.Unlock()
	sort.Slice(providers, func(i, j int) bool {
		return providers[i].channel < providers[j].channel
	})

	type discoveryResult struct {
		channel string
		models  []ModelInfo
		failed  bool
		replace bool
	}
	results := make(chan discoveryResult, len(providers))
	discoveryCount := 0
	for _, entry := range providers {
		if ctx.Err() != nil {
			return
		}
		disc, ok := entry.provider.(ModelDiscoverer)
		discWithError, reportsError := entry.provider.(ModelDiscoveryWithError)
		if !ok && !reportsError {
			continue
		}
		discoveryCount++
		go func(channel string, discoverer ModelDiscoverer, discovererWithError ModelDiscoveryWithError, reportsError bool) {
			var models []ModelInfo
			var err error
			replace := false
			failed := false
			if reportsError {
				models, err = discovererWithError.DiscoverModelsWithError(ctx)
				failed = err != nil
				if err != nil {
					slog.Warn("model router: 模型池探测失败，保留已有清单", "channel", channel, "err", err)
				}
			} else {
				models = discoverer.DiscoverModels(ctx)
			}
			models = normalizeModelInfos(models, channel)
			if len(models) == 0 {
				failed = true
				slog.Warn("model router: 模型池探测未返回可用模型，保留已有清单", "channel", channel)
			} else {
				replace = err == nil
			}
			results <- discoveryResult{channel: channel, models: models, failed: failed, replace: replace}
		}(entry.channel, disc, discWithError, reportsError)
	}
	discovered := make(map[string]discoveryResult, discoveryCount)
	for received := 0; received < discoveryCount; {
		select {
		case result := <-results:
			received++
			discovered[result.channel] = result
		case <-ctx.Done():
			return
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Channel registration invalidates the entire snapshot, but successful model
	// discovery is merged per channel so one provider's failure cannot discard
	// another provider's useful result from an older overlapping refresh.
	if providerVersion != r.providerVersion {
		return
	}
	if r.appliedRefreshVersion == nil {
		r.appliedRefreshVersion = make(map[string]uint64)
	}
	for channel := range discovered {
		if refreshVersion < r.appliedRefreshVersion[channel] {
			delete(discovered, channel)
		}
	}
	if len(discovered) == 0 {
		return
	}
	refreshedModels := make(map[string][]ModelInfo, len(discovered))
	for channel, result := range discovered {
		status := r.discoveryStatus[channel]
		status.LastAttempt = time.Now()
		status.Failed = result.failed
		if !result.failed {
			status.LastSuccess = status.LastAttempt
		} else if !hasModelsForChannel(r.models, channel) {
			r.models = appendUniqueModels(r.models, result.models, channel)
		}
		r.discoveryStatus[channel] = status
		r.appliedRefreshVersion[channel] = refreshVersion
		if result.replace {
			refreshedModels[channel] = result.models
		}
	}
	// Replace only channels whose discovery succeeded. This preserves explicit
	// model pools for non-discovering providers and cached entries on failures.
	var updatedModels []ModelInfo
	for _, existing := range r.models {
		if _, refreshed := refreshedModels[existing.Channel]; !refreshed {
			updatedModels = append(updatedModels, existing)
		}
	}
	for _, entry := range providers {
		updatedModels = append(updatedModels, refreshedModels[entry.channel]...)
	}
	r.models = updatedModels
	if (r.active.Channel == "" || r.active.Model == "") && len(r.models) > 0 {
		selected := -1
		for index, model := range r.models {
			if strings.EqualFold(model.Channel, r.fallbackChannel) {
				selected = index
				break
			}
		}
		if selected < 0 {
			selected = 0
		}
		model := r.models[selected]
		r.active = ActiveMindState{
			Channel:   model.Channel,
			Model:     model.ID,
			Effort:    model.DefaultEffort,
			UpdatedAt: time.Now().Format(time.RFC3339),
		}
		r.saveActiveStateLocked(model.Channel)
		r.syncProviderLocked(model.Channel, model.ID, model.DefaultEffort)
	}
	if activeModels := refreshedModels[r.active.Channel]; len(activeModels) > 0 {
		var matched *ModelInfo
		for i := range activeModels {
			if strings.EqualFold(activeModels[i].ID, r.active.Model) {
				matched = &activeModels[i]
				break
			}
		}
		previous := r.active
		if matched == nil {
			matched = &activeModels[0]
			r.active.Model = matched.ID
			r.active.Effort = matched.DefaultEffort
		} else if !matched.SupportsEffort {
			r.active.Effort = ""
		} else if r.active.Effort != "" && !isEffortAllowed(*matched, r.active.Effort) {
			r.active.Effort = matched.DefaultEffort
		}
		if r.active.Model != previous.Model || r.active.Effort != previous.Effort {
			r.active.UpdatedAt = time.Now().Format(time.RFC3339)
			r.saveActiveStateLocked(r.active.Channel)
			r.syncProviderLocked(r.active.Channel, r.active.Model, r.active.Effort)
		}
	}
}

// Channels 列出当前已挂载的所有通道标识
func (r *MindRouter) Channels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	channels := make([]string, 0, len(r.providers))
	for ch := range r.providers {
		channels = append(channels, ch)
	}
	sort.Strings(channels)
	return channels
}

// QueryChannelUsage 查询指定通道或当前激活通道的配额与实时用量
func (r *MindRouter) QueryChannelUsage(ctx context.Context, channel string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	chKey := strings.ToUpper(strings.TrimSpace(channel))
	if chKey == "" {
		curr := r.CurrentMind()
		chKey = curr.Channel
	}

	r.mu.RLock()
	prov, exists := r.providers[chKey]
	r.mu.RUnlock()

	if !exists {
		return "", fmt.Errorf("未找到通道 %s", chKey)
	}

	if ud, ok := prov.(UsageDiscoverer); ok {
		return ud.GetUsage(ctx)
	}

	return "", fmt.Errorf("通道 %s 未提供动态用量探测接口", chKey)
}

// ResolveModelAndEffort 在已探测的模型池中智能解析模型名与思考强度
func (r *MindRouter) ResolveModelAndEffort(rawModel, fallbackModel, fallbackEffort string) (string, string) {
	if strings.TrimSpace(rawModel) == "" {
		return fallbackModel, fallbackEffort
	}

	r.mu.RLock()
	cleanTarget, extractedEffort := r.splitKnownModelAndEffort(rawModel)
	candidates := r.findCandidatesLocked(cleanTarget, "")
	r.mu.RUnlock()

	if len(candidates) == 1 {
		m := candidates[0]
		if !m.SupportsEffort {
			return m.ID, ""
		}
		if isEffortAllowed(m, extractedEffort) {
			return m.ID, extractedEffort
		}
		if isEffortAllowed(m, fallbackEffort) {
			return m.ID, fallbackEffort
		}
		if m.DefaultEffort != "" {
			return m.ID, m.DefaultEffort
		}
		if len(m.SupportedEfforts) > 0 {
			return m.ID, m.SupportedEfforts[0]
		}
		return m.ID, ""
	}

	if extractedEffort != "" {
		return cleanTarget, extractedEffort
	}
	return cleanTarget, fallbackEffort
}

// ResolveModelAndEffortForChannel resolves a task model only against the
// requested execution channel, preventing a model from another provider from
// being passed to a CLI that cannot run it.
func (r *MindRouter) ResolveModelAndEffortForChannel(rawModel, fallbackModel, fallbackEffort, channel string) (string, string, error) {
	if strings.TrimSpace(rawModel) == "" {
		return fallbackModel, fallbackEffort, nil
	}

	channel = strings.ToUpper(strings.TrimSpace(channel))
	if channel == "" {
		return "", "", fmt.Errorf("model execution channel cannot be empty")
	}
	r.mu.RLock()
	cleanTarget, requestedEffort := r.splitKnownModelAndEffort(rawModel)
	candidates := r.findCandidatesLocked(cleanTarget, channel)
	allCandidates := candidates
	if len(candidates) == 0 {
		allCandidates = r.findCandidatesLocked(cleanTarget, "")
	}
	r.mu.RUnlock()

	if len(candidates) > 1 {
		return "", "", fmt.Errorf("模型 %q 在 %s 通道中匹配到多个候选项，请指定完整模型 ID", cleanTarget, channel)
	}
	if len(candidates) == 0 {
		if len(allCandidates) > 0 {
			return "", "", fmt.Errorf("模型 %q 不属于 %s 通道，不能用于该执行器", cleanTarget, channel)
		}
		if requestedEffort != "" {
			return cleanTarget, requestedEffort, nil
		}
		return cleanTarget, fallbackEffort, nil
	}

	model := candidates[0]
	if !model.SupportsEffort {
		if requestedEffort != "" {
			return "", "", fmt.Errorf("模型 %s 不支持思考强度调节", model.ID)
		}
		return model.ID, "", nil
	}

	effort := model.DefaultEffort
	if requestedEffort != "" {
		if !isEffortAllowed(model, requestedEffort) {
			return "", "", fmt.Errorf("模型 %s 不支持思考强度 %q，支持的档位为: [%s]", model.ID, requestedEffort, strings.Join(model.SupportedEfforts, ", "))
		}
		effort = requestedEffort
	} else if isEffortAllowed(model, fallbackEffort) {
		effort = fallbackEffort
	} else if effort == "" && len(model.SupportedEfforts) > 0 {
		effort = model.SupportedEfforts[0]
	}
	return model.ID, effort, nil
}

func isEffortAllowed(m ModelInfo, effort string) bool {
	if !m.SupportsEffort {
		return false
	}
	if len(m.SupportedEfforts) == 0 {
		return true
	}
	for _, e := range m.SupportedEfforts {
		if strings.EqualFold(e, effort) {
			return true
		}
	}
	return false
}

// SwitchMind 动态切换心智模型或调整思考强度
func (r *MindRouter) SwitchMind(req SwitchRequest) (*SwitchResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	targetRaw := strings.TrimSpace(req.Target)
	channelReq := strings.ToUpper(strings.TrimSpace(req.Channel))
	effortReq := strings.ToLower(strings.TrimSpace(req.Effort))

	// 若未指定目标模型，仅要求调整思考强度
	if targetRaw == "" && effortReq != "" {
		if channelReq != "" && !strings.EqualFold(channelReq, r.active.Channel) {
			return &SwitchResult{
				Status:  "error",
				Message: fmt.Sprintf("当前激活通道为 %s；若要切换到 %s 并调整思考强度，请同时指定该通道的目标模型。", r.active.Channel, channelReq),
			}, nil
		}
		activeProvider := r.providers[r.active.Channel]
		if _, ok := activeProvider.(EffortConfigurable); !ok {
			return &SwitchResult{
				Status:  "error",
				Message: fmt.Sprintf("当前激活的是 %s 通道 (%s)，该驱动不支持动态调整思考强度。", r.active.Channel, r.active.Model),
			}, nil
		}

		activeModelFound := false
		for _, m := range r.models {
			if strings.EqualFold(m.Channel, r.active.Channel) && strings.EqualFold(m.ID, r.active.Model) {
				activeModelFound = true
				if !m.SupportsEffort {
					return &SwitchResult{
						Status:  "error",
						Message: fmt.Sprintf("当前模型 %s 不支持思考强度调节。", m.ID),
					}, nil
				}
				if !isEffortAllowed(m, effortReq) {
					return &SwitchResult{
						Status:  "error",
						Message: fmt.Sprintf("模型 %s 不支持思考强度 %q，支持的档位为: [%s]", m.ID, effortReq, strings.Join(m.SupportedEfforts, ", ")),
					}, nil
				}
				break
			}
		}
		if !activeModelFound {
			return &SwitchResult{
				Status:  "error",
				Message: fmt.Sprintf("当前模型 %s 不在已探测的可用模型池中，无法验证思考强度。", r.active.Model),
			}, nil
		}

		previous := r.active
		r.active.Effort = effortReq
		r.active.UpdatedAt = time.Now().Format(time.RFC3339)
		if err := r.saveState(); err != nil {
			r.active = previous
			return &SwitchResult{Status: "error", Message: fmt.Sprintf("保存模型设置失败，未应用更改: %v", err)}, nil
		}
		r.syncProviderLocked(r.active.Channel, r.active.Model, r.active.Effort)

		return &SwitchResult{
			Status:  "success",
			Channel: r.active.Channel,
			Model:   r.active.Model,
			Effort:  r.active.Effort,
			Message: fmt.Sprintf("已将当前 %s 通道模型 (%s) 的思考强度调整为: %s", r.active.Channel, r.active.Model, effortReq),
		}, nil
	}

	if targetRaw == "" {
		return &SwitchResult{
			Status:  "error",
			Message: "未指定目标模型名称",
		}, nil
	}

	cleanTarget, extractedEffort := r.splitKnownModelAndEffort(targetRaw)
	if effortReq == "" && extractedEffort != "" {
		effortReq = extractedEffort
	}

	candidates := r.findCandidatesLocked(cleanTarget, channelReq)
	if len(candidates) == 0 {
		return &SwitchResult{
			Status:  "error",
			Message: fmt.Sprintf("未找到与 %q 匹配的模型。可调用 list 查看当前动态探测到的全部可用心智。", targetRaw),
		}, nil
	}

	if len(candidates) > 1 {
		channelsSet := make(map[string]struct{})
		for _, c := range candidates {
			channelsSet[c.Channel] = struct{}{}
		}
		var channelNames []string
		for ch := range channelsSet {
			channelNames = append(channelNames, ch+" 通道")
		}
		sort.Strings(channelNames)

		return &SwitchResult{
			Status:     "ambiguous",
			Candidates: cloneModelInfos(candidates),
			Message: fmt.Sprintf("在 %s 均检测到匹配的候选心智（共 %d 个选项），请指定完整的模型 ID 或通道。",
				strings.Join(channelNames, "、"), len(candidates)),
		}, nil
	}

	targetModel := candidates[0]

	finalEffort := ""
	if targetModel.SupportsEffort {
		if effortReq != "" {
			if !isEffortAllowed(targetModel, effortReq) {
				return &SwitchResult{
					Status:  "error",
					Message: fmt.Sprintf("模型 %s 不支持思考强度 %q，支持的档位为: [%s]", targetModel.ID, effortReq, strings.Join(targetModel.SupportedEfforts, ", ")),
				}, nil
			}
			finalEffort = effortReq
		} else if targetModel.DefaultEffort != "" {
			finalEffort = targetModel.DefaultEffort
		} else if len(targetModel.SupportedEfforts) > 0 {
			finalEffort = targetModel.SupportedEfforts[0]
		}
	}

	previous := r.active
	r.active = ActiveMindState{
		Channel:   targetModel.Channel,
		Model:     targetModel.ID,
		Effort:    finalEffort,
		UpdatedAt: time.Now().Format(time.RFC3339),
	}

	if err := r.saveState(); err != nil {
		r.active = previous
		return &SwitchResult{Status: "error", Message: fmt.Sprintf("保存模型设置失败，未应用更改: %v", err)}, nil
	}
	r.syncProviderLocked(targetModel.Channel, targetModel.ID, finalEffort)

	return &SwitchResult{
		Status:  "success",
		Channel: targetModel.Channel,
		Model:   targetModel.ID,
		Name:    targetModel.Name,
		Effort:  finalEffort,
		Message: fmt.Sprintf("心智已成功热切换至 %s 通道模型 %s (%s)%s",
			targetModel.Channel, targetModel.Name, targetModel.ID,
			formatEffortNotice(finalEffort)),
	}, nil
}

func formatEffortNotice(effort string) string {
	if effort == "" {
		return ""
	}
	return fmt.Sprintf("，思考强度: %s", effort)
}

func (r *MindRouter) findCandidatesLocked(query string, channelFilter string) []ModelInfo {
	q := strings.ToLower(strings.TrimSpace(query))
	var pool []ModelInfo
	for _, m := range r.models {
		if channelFilter != "" && !strings.EqualFold(m.Channel, channelFilter) {
			continue
		}
		pool = append(pool, m)
	}

	// 1. 精确匹配 ID 或 Name
	var exactMatches []ModelInfo
	for _, m := range pool {
		if strings.EqualFold(m.ID, q) || strings.EqualFold(m.Name, q) {
			exactMatches = append(exactMatches, m)
		}
	}
	if len(exactMatches) > 0 {
		return exactMatches
	}

	// 2. 子串匹配 ID 或 Name (如 "flash" -> "gemini-3.8-flash", "opus" -> "claude-opus-5-5")
	var subMatches []ModelInfo
	for _, m := range pool {
		if strings.Contains(strings.ToLower(m.ID), q) || strings.Contains(strings.ToLower(m.Name), q) {
			subMatches = append(subMatches, m)
		}
	}
	return subMatches
}

// splitKnownModelAndEffort gives a discovered full model ID precedence over a
// trailing effort suffix, since valid provider IDs can themselves end in
// strings such as "-high" or "-low".
func (r *MindRouter) splitKnownModelAndEffort(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	for _, model := range r.models {
		if strings.EqualFold(model.ID, raw) {
			return raw, ""
		}
	}
	return SplitModelAndEffort(raw)
}

// SplitModelAndEffort 拆分模型名称末尾可能附带的标准思考强度后缀 (如 "gemini-4-argon-high" -> "gemini-4-argon", "high")
func SplitModelAndEffort(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	lower := strings.ToLower(raw)

	for _, eff := range []string{"xhigh", "medium", "high", "low", "max"} {
		suffix := "-" + eff
		if strings.HasSuffix(lower, suffix) {
			return strings.TrimSuffix(raw[:len(raw)-len(suffix)], "-"), eff
		}
		spaceSuffix := " " + eff
		if strings.HasSuffix(lower, spaceSuffix) {
			return strings.TrimSpace(raw[:len(raw)-len(spaceSuffix)]), eff
		}
	}
	return raw, ""
}

func (r *MindRouter) syncProviderLocked(channel, model, effort string) {
	p := r.providers[channel]
	if p == nil {
		return
	}
	if modc, ok := p.(ModelConfigurable); ok && model != "" {
		modc.SetModel(model)
	}
	if effc, ok := p.(EffortConfigurable); ok {
		effc.SetEffort(effort)
	}
}

func (r *MindRouter) loadState() error {
	data, err := os.ReadFile(r.statePath)
	if err != nil {
		return err
	}
	var st ActiveMindState
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}
	if st.Channel == "" || st.Model == "" {
		return fmt.Errorf("invalid state format")
	}
	r.active = st
	return nil
}

func (r *MindRouter) saveState() error {
	dir := filepath.Dir(r.statePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r.active, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(r.statePath)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0644); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, r.statePath)
}

// Name 识别当前路由器描述
func (r *MindRouter) Name() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.active.Effort != "" {
		return fmt.Sprintf("MindRouter [%s: %s, effort: %s]", r.active.Channel, r.active.Model, r.active.Effort)
	}
	return fmt.Sprintf("MindRouter [%s: %s]", r.active.Channel, r.active.Model)
}

// Chat 同步对话执行
func (r *MindRouter) Chat(ctx context.Context, messages []core.ChatMessage, opts *core.ChatOptions) (string, error) {
	resp, err := r.ChatWithTools(ctx, messages, nil, opts)
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", fmt.Errorf("心智通道返回空响应")
	}
	return resp.Content, nil
}

// ChatWithTools 带工具调用的开放心智调度
func (r *MindRouter) ChatWithTools(
	ctx context.Context,
	messages []core.ChatMessage,
	tools []core.ToolDefinition,
	opts *core.ChatOptions,
) (*core.ChatResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	channel := r.active.Channel
	model := r.active.Model
	activeProvider := r.providers[channel]
	fallbackProvider := r.providers[r.fallbackChannel]
	r.mu.RUnlock()

	// 访客请求不进入 AGY 全能智能体通道；兜底通道不可用时也必须失败关闭。
	if opts != nil && opts.ReadOnly && !strings.EqualFold(channel, r.fallbackChannel) {
		if fallbackProvider == nil {
			return nil, fmt.Errorf("访客请求无法路由：受限兜底通道 %q 未挂载", r.fallbackChannel)
		}
		resp, err := fallbackProvider.ChatWithTools(ctx, messages, tools, opts)
		if err == nil && isEmptyChatResponse(resp) {
			return nil, fmt.Errorf("受限兜底通道 %q 返回空响应", r.fallbackChannel)
		}
		return resp, err
	}
	if activeProvider == nil {
		if fallbackProvider == nil || strings.EqualFold(channel, r.fallbackChannel) {
			return nil, fmt.Errorf("当前激活的心智通道 %q 未挂载任何可用驱动", channel)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		slog.Warn("当前激活的心智通道未挂载，降级至兜底通道",
			"channel", channel, "fallbackChannel", r.fallbackChannel)
		resp, err := fallbackProvider.ChatWithTools(ctx, messages, tools, opts)
		if err == nil && isEmptyChatResponse(resp) {
			err = fmt.Errorf("兜底通道 %q 返回空响应", r.fallbackChannel)
		}
		if err != nil {
			return nil, fmt.Errorf("当前激活通道 %q 未挂载，且兜底通道执行失败: %w", channel, err)
		}
		return resp, nil
	}

	resp, err := activeProvider.ChatWithTools(ctx, messages, tools, opts)
	if err == nil && isEmptyChatResponse(resp) {
		err = fmt.Errorf("%s 通道返回空响应", channel)
	}
	if err == nil {
		return resp, nil
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if fallbackProvider != nil && !strings.EqualFold(channel, r.fallbackChannel) {
		slog.Warn("当前心智通道执行遇到异常，启动弹性降级机制至兜底通道",
			"channel", channel, "model", model, "fallbackChannel", r.fallbackChannel, "err", err)

		fbResp, fbErr := fallbackProvider.ChatWithTools(ctx, messages, tools, opts)
		if fbErr == nil && isEmptyChatResponse(fbResp) {
			fbErr = fmt.Errorf("兜底通道 %q 返回空响应", r.fallbackChannel)
		}
		if fbErr == nil {
			return fbResp, nil
		}
		slog.Warn("兜底心智通道执行失败", "channel", r.fallbackChannel, "err", fbErr)
	}

	return nil, fmt.Errorf("%s 通道心智驱动执行失败: %w", channel, err)
}

func isEmptyChatResponse(resp *core.ChatResponse) bool {
	return resp == nil || (len(resp.ToolCalls) == 0 && strings.TrimSpace(resp.Content) == "")
}
