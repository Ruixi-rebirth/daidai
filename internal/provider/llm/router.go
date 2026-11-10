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
	mu              sync.RWMutex
	statePath       string
	fallbackChannel string
	active          ActiveMindState
	providers       map[string]core.LLMProvider
	models          []ModelInfo
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
	if len(models) == 0 {
		if disc, ok := reg.Provider.(ModelDiscoverer); ok {
			models = disc.DiscoverModels(context.Background())
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.providers[channelKey] = reg.Provider

	for _, m := range models {
		m.Channel = channelKey
		if m.Name == "" {
			m.Name = m.ID
		}
		r.models = append(r.models, m)
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
			_ = r.saveState()
		}
	}

	if r.active.Channel == channelKey {
		if len(models) > 0 {
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
				_ = r.saveState()
			} else if !matched.SupportsEffort && r.active.Effort != "" {
				r.active.Effort = ""
				_ = r.saveState()
			} else if matched.SupportsEffort && r.active.Effort != "" && !isEffortAllowed(*matched, r.active.Effort) {
				r.active.Effort = matched.DefaultEffort
				_ = r.saveState()
			}
		}
		r.syncProviderLocked(channelKey, r.active.Model, r.active.Effort)
	}
}

// CurrentMind 获取当前激活的心智与思考强度
func (r *MindRouter) CurrentMind() ActiveMindState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.active
}

// FormatCurrentStatus 格式化当前激活的心智运行状态摘要
func (r *MindRouter) FormatCurrentStatus() string {
	r.mu.RLock()
	curr := r.active
	models := r.models
	r.mu.RUnlock()

	if curr.Model == "" && curr.Channel == "" {
		return ""
	}

	modelName := curr.Model
	for _, m := range models {
		if m.ID == curr.Model && (curr.Channel == "" || strings.EqualFold(m.Channel, curr.Channel)) {
			modelName = m.Name
			break
		}
	}

	var sb strings.Builder
	sb.WriteString("【当前心智运行状态】\n")
	sb.WriteString(fmt.Sprintf("- 激活模型：%s (`%s`)\n", modelName, curr.Model))
	sb.WriteString(fmt.Sprintf("- 通信通道：%s 通道\n", curr.Channel))
	if curr.Effort != "" {
		sb.WriteString(fmt.Sprintf("- 思考强度：%s\n", curr.Effort))
	}
	return strings.TrimSpace(sb.String())
}

// ListModels 列出所有已探测/注册的可用模型定义
func (r *MindRouter) ListModels() []ModelInfo {
	r.mu.RLock()
	needRefresh := len(r.models) == 0
	r.mu.RUnlock()

	if needRefresh {
		r.RefreshModels(context.Background())
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]ModelInfo, len(r.models))
	copy(result, r.models)
	return result
}

// RefreshModels 重新触发所有已挂载通道的模型探测并刷新模型池
func (r *MindRouter) RefreshModels(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var newModels []ModelInfo
	for chKey, prov := range r.providers {
		if disc, ok := prov.(ModelDiscoverer); ok {
			probed := disc.DiscoverModels(ctx)
			for _, m := range probed {
				m.Channel = chKey
				if m.Name == "" {
					m.Name = m.ID
				}
				newModels = append(newModels, m)
			}
		}
	}
	if len(newModels) > 0 {
		r.models = newModels
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

	cleanTarget, extractedEffort := SplitModelAndEffort(rawModel)

	r.mu.RLock()
	candidates := r.findCandidatesLocked(cleanTarget, "")
	r.mu.RUnlock()

	if len(candidates) == 1 {
		m := candidates[0]
		if !m.SupportsEffort {
			return m.ID, ""
		}
		if extractedEffort != "" {
			return m.ID, extractedEffort
		}
		if fallbackEffort != "" {
			return m.ID, fallbackEffort
		}
		return m.ID, m.DefaultEffort
	}

	if extractedEffort != "" {
		return cleanTarget, extractedEffort
	}
	return cleanTarget, fallbackEffort
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
		activeProvider := r.providers[r.active.Channel]
		if _, ok := activeProvider.(EffortConfigurable); !ok {
			return &SwitchResult{
				Status:  "error",
				Message: fmt.Sprintf("当前激活的是 %s 通道 (%s)，该驱动不支持动态调整思考强度。", r.active.Channel, r.active.Model),
			}, nil
		}

		for _, m := range r.models {
			if strings.EqualFold(m.Channel, r.active.Channel) && m.ID == r.active.Model {
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

		r.active.Effort = effortReq
		r.active.UpdatedAt = time.Now().Format(time.RFC3339)
		_ = r.saveState()
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

	cleanTarget, extractedEffort := SplitModelAndEffort(targetRaw)
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
			Candidates: candidates,
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

	r.active = ActiveMindState{
		Channel:   targetModel.Channel,
		Model:     targetModel.ID,
		Effort:    finalEffort,
		UpdatedAt: time.Now().Format(time.RFC3339),
	}

	_ = r.saveState()
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
	tmpFile := fmt.Sprintf("%s.tmp.%d", r.statePath, time.Now().UnixNano())
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpFile, r.statePath)
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
	return resp.Content, nil
}

// ChatWithTools 带工具调用的开放心智调度
func (r *MindRouter) ChatWithTools(
	ctx context.Context,
	messages []core.ChatMessage,
	tools []core.ToolDefinition,
	opts *core.ChatOptions,
) (*core.ChatResponse, error) {
	r.mu.RLock()
	channel := r.active.Channel
	model := r.active.Model
	activeProvider := r.providers[channel]
	fallbackProvider := r.providers[r.fallbackChannel]
	r.mu.RUnlock()

	if activeProvider == nil {
		return nil, fmt.Errorf("当前激活的心智通道 %q 未挂载任何可用驱动", channel)
	}

	// 普通访客 (ReadOnly / RoleGuest) 严禁直接接入 AGY 全能智能体通道，强制路由至受控 API 通道配合只读工具处理
	if opts != nil && opts.ReadOnly && fallbackProvider != nil && !strings.EqualFold(channel, r.fallbackChannel) {
		return fallbackProvider.ChatWithTools(ctx, messages, tools, opts)
	}

	resp, err := activeProvider.ChatWithTools(ctx, messages, tools, opts)
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
		if fbErr == nil {
			return fbResp, nil
		}
	}

	return nil, fmt.Errorf("%s 通道心智驱动执行失败: %w", channel, err)
}
