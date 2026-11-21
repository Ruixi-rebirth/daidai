package llm

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"daidai/internal/core"
)

func providerModelID(provider core.LLMProvider) string {
	if configured, ok := provider.(interface{ GetModel() string }); ok {
		return configured.GetModel()
	}
	if configured, ok := provider.(interface{ GetModelAndEffort() (string, string) }); ok {
		model, _ := configured.GetModelAndEffort()
		return model
	}
	return "未提供模型 ID"
}

// ChannelModelUsage identifies the model actually configured on a provider,
// including a fallback model that differs from the preferred route.
func (r *MindRouter) ChannelModelUsage(channel string) (string, string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	provider := r.providers[channel]
	if provider == nil {
		return "", ""
	}
	if strings.EqualFold(channel, r.active.Channel) {
		return r.active.Model, "首选聊天"
	}
	if strings.EqualFold(channel, r.fallbackChannel) {
		return providerModelID(provider), "备用 / 访客聊天"
	}
	return providerModelID(provider), "通道配置"
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
