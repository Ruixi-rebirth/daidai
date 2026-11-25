package llm

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

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

func normalizeMindScope(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", MindScopePartner, "private":
		return MindScopePartner, nil
	case MindScopePublic, "group", "shared":
		return MindScopePublic, nil
	case MindScopeBoth, "all", "global":
		return MindScopeBoth, nil
	default:
		return "", fmt.Errorf("未知模型切换范围 %q，可选值为 partner（搭档私聊）、public（公用模型）或 both（双轨同步）", raw)
	}
}

// SwitchMind 动态切换心智模型或调整思考强度（支持按 partner / public / both 轨道独立控制）
func (r *MindRouter) SwitchMind(req SwitchRequest) (*SwitchResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	scope, scopeErr := normalizeMindScope(req.Scope)
	if scopeErr != nil {
		return &SwitchResult{Status: "error", Message: scopeErr.Error()}, nil
	}

	targetRaw := strings.TrimSpace(req.Target)
	channelReq := strings.ToUpper(strings.TrimSpace(req.Channel))
	effortReq := strings.ToLower(strings.TrimSpace(req.Effort))

	// 若未指定目标模型，仅要求调整思考强度
	if targetRaw == "" && effortReq != "" {
		return r.switchEffortOnlyLocked(scope, channelReq, effortReq)
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

	nowStr := time.Now().Format(time.RFC3339)
	prevActive := r.active
	prevPublic := r.public
	prevPinned := r.publicPinned

	switch scope {
	case MindScopePartner:
		r.active = ActiveMindState{
			Channel:   targetModel.Channel,
			Model:     targetModel.ID,
			Effort:    finalEffort,
			UpdatedAt: nowStr,
		}
		if !r.publicPinned || r.public.Channel == "" || r.public.Model == "" {
			r.public = r.selectDefaultPublicMindLocked()
		}
		if err := r.saveState(); err != nil {
			r.active = prevActive
			r.public = prevPublic
			r.publicPinned = prevPinned
			return &SwitchResult{Status: "error", Message: fmt.Sprintf("保存模型设置失败，未应用更改: %v", err)}, nil
		}
		r.syncProviderLocked(targetModel.Channel, targetModel.ID, finalEffort)
		pubHint := ""
		if r.public.Model != "" && (!strings.EqualFold(r.public.Channel, targetModel.Channel) || !strings.EqualFold(r.public.Model, targetModel.ID)) {
			pubHint = fmt.Sprintf("（公用模型保持 %s · %s 不变）", r.public.Channel, r.public.Model)
		}
		return &SwitchResult{
			Status:  "success",
			Scope:   MindScopePartner,
			Channel: targetModel.Channel,
			Model:   targetModel.ID,
			Name:    targetModel.Name,
			Effort:  finalEffort,
			Message: fmt.Sprintf("搭档私聊心智已成功热切换至 %s 通道模型 %s (%s)%s%s",
				targetModel.Channel, targetModel.Name, targetModel.ID,
				formatEffortNotice(finalEffort), pubHint),
		}, nil

	case MindScopePublic:
		pubEffort, capped := capPublicEffortForModel(targetModel, finalEffort)
		r.public = MindSlotState{
			Channel:   targetModel.Channel,
			Model:     targetModel.ID,
			Effort:    pubEffort,
			UpdatedAt: nowStr,
		}
		r.publicPinned = true
		if err := r.saveState(); err != nil {
			r.public = prevPublic
			r.publicPinned = prevPinned
			return &SwitchResult{Status: "error", Message: fmt.Sprintf("保存公用模型设置失败，未应用更改: %v", err)}, nil
		}
		capNote := ""
		if capped {
			capNote = "（公用模型思考强度已按上限自动封顶为 medium）"
		}
		privHint := ""
		if r.active.Model != "" {
			privHint = fmt.Sprintf("（搭档私聊模型保持 %s · %s 不变）", r.active.Channel, r.active.Model)
		}
		return &SwitchResult{
			Status:  "success",
			Scope:   MindScopePublic,
			Channel: targetModel.Channel,
			Model:   targetModel.ID,
			Name:    targetModel.Name,
			Effort:  pubEffort,
			Message: fmt.Sprintf("公用模型已成功热切换至 %s 通道模型 %s (%s)%s%s%s",
				targetModel.Channel, targetModel.Name, targetModel.ID,
				formatEffortNotice(pubEffort), capNote, privHint),
		}, nil

	default: // MindScopeBoth
		pubEffort, capped := capPublicEffortForModel(targetModel, finalEffort)
		r.active = ActiveMindState{
			Channel:   targetModel.Channel,
			Model:     targetModel.ID,
			Effort:    finalEffort,
			UpdatedAt: nowStr,
		}
		r.public = MindSlotState{
			Channel:   targetModel.Channel,
			Model:     targetModel.ID,
			Effort:    pubEffort,
			UpdatedAt: nowStr,
		}
		r.publicPinned = true
		if err := r.saveState(); err != nil {
			r.active = prevActive
			r.public = prevPublic
			r.publicPinned = prevPinned
			return &SwitchResult{Status: "error", Message: fmt.Sprintf("保存全局模型设置失败，未应用更改: %v", err)}, nil
		}
		r.syncProviderLocked(targetModel.Channel, targetModel.ID, finalEffort)
		capNote := ""
		if capped {
			capNote = fmt.Sprintf("（公用轨思考强度按上限设为 %s）", pubEffort)
		}
		return &SwitchResult{
			Status:  "success",
			Scope:   MindScopeBoth,
			Channel: targetModel.Channel,
			Model:   targetModel.ID,
			Name:    targetModel.Name,
			Effort:  finalEffort,
			Message: fmt.Sprintf("搭档私聊与公用模型已同步热切换至 %s 通道模型 %s (%s)%s%s",
				targetModel.Channel, targetModel.Name, targetModel.ID,
				formatEffortNotice(finalEffort), capNote),
		}, nil
	}
}

func (r *MindRouter) switchEffortOnlyLocked(scope, channelReq, effortReq string) (*SwitchResult, error) {
	slotChannel := r.active.Channel
	slotModel := r.active.Model
	if scope == MindScopePublic && r.public.Channel != "" && r.public.Model != "" {
		slotChannel = r.public.Channel
		slotModel = r.public.Model
	}
	if channelReq != "" && !strings.EqualFold(channelReq, slotChannel) {
		return &SwitchResult{
			Status:  "error",
			Message: fmt.Sprintf("当前目标轨道通道为 %s；若要切换到 %s 并调整思考强度，请同时指定该通道的目标模型。", slotChannel, channelReq),
		}, nil
	}
	activeProvider := r.providers[slotChannel]
	if _, ok := activeProvider.(EffortConfigurable); !ok {
		return &SwitchResult{
			Status:  "error",
			Message: fmt.Sprintf("当前目标轨道为 %s 通道 (%s)，该驱动不支持动态调整思考强度。", slotChannel, slotModel),
		}, nil
	}

	var matchedModel *ModelInfo
	for i := range r.models {
		if strings.EqualFold(r.models[i].Channel, slotChannel) && strings.EqualFold(r.models[i].ID, slotModel) {
			matchedModel = &r.models[i]
			break
		}
	}
	if matchedModel == nil {
		return &SwitchResult{
			Status:  "error",
			Message: fmt.Sprintf("当前模型 %s 不在已探测的可用模型池中，无法验证思考强度。", slotModel),
		}, nil
	}
	if !matchedModel.SupportsEffort {
		return &SwitchResult{
			Status:  "error",
			Message: fmt.Sprintf("当前模型 %s 不支持思考强度调节。", matchedModel.ID),
		}, nil
	}
	if !isEffortAllowed(*matchedModel, effortReq) {
		return &SwitchResult{
			Status:  "error",
			Message: fmt.Sprintf("模型 %s 不支持思考强度 %q，支持的档位为: [%s]", matchedModel.ID, effortReq, strings.Join(matchedModel.SupportedEfforts, ", ")),
		}, nil
	}

	nowStr := time.Now().Format(time.RFC3339)
	prevActive := r.active
	prevPublic := r.public
	prevPinned := r.publicPinned

	switch scope {
	case MindScopePublic:
		pubEffort, capped := capPublicEffortForModel(*matchedModel, effortReq)
		r.public.Channel = slotChannel
		r.public.Model = slotModel
		r.public.Effort = pubEffort
		r.public.UpdatedAt = nowStr
		r.publicPinned = true
		if err := r.saveState(); err != nil {
			r.public = prevPublic
			r.publicPinned = prevPinned
			return &SwitchResult{Status: "error", Message: fmt.Sprintf("保存公用模型设置失败，未应用更改: %v", err)}, nil
		}
		capNote := ""
		if capped {
			capNote = "（公用轨上限为 medium）"
		}
		return &SwitchResult{
			Status:  "success",
			Scope:   MindScopePublic,
			Channel: slotChannel,
			Model:   slotModel,
			Effort:  pubEffort,
			Message: fmt.Sprintf("已将公用模型 %s 通道 (%s) 的思考强度调整为: %s%s", slotChannel, slotModel, pubEffort, capNote),
		}, nil

	case MindScopeBoth:
		r.active.Effort = effortReq
		r.active.UpdatedAt = nowStr
		if r.public.Channel != "" && r.public.Model != "" {
			for i := range r.models {
				if strings.EqualFold(r.models[i].Channel, r.public.Channel) && strings.EqualFold(r.models[i].ID, r.public.Model) && r.models[i].SupportsEffort {
					eff, _ := capPublicEffortForModel(r.models[i], effortReq)
					r.public.Effort = eff
					r.public.UpdatedAt = nowStr
					r.publicPinned = true
					break
				}
			}
		}
		if err := r.saveState(); err != nil {
			r.active = prevActive
			r.public = prevPublic
			r.publicPinned = prevPinned
			return &SwitchResult{Status: "error", Message: fmt.Sprintf("保存模型设置失败，未应用更改: %v", err)}, nil
		}
		r.syncProviderLocked(r.active.Channel, r.active.Model, r.active.Effort)
		return &SwitchResult{
			Status:  "success",
			Scope:   MindScopeBoth,
			Channel: r.active.Channel,
			Model:   r.active.Model,
			Effort:  r.active.Effort,
			Message: fmt.Sprintf("已同步调整双轨思考强度（搭档私聊: %s，公用轨: %s）", r.active.Effort, r.public.Effort),
		}, nil

	default:
		r.active.Effort = effortReq
		r.active.UpdatedAt = nowStr
		if err := r.saveState(); err != nil {
			r.active = prevActive
			return &SwitchResult{Status: "error", Message: fmt.Sprintf("保存模型设置失败，未应用更改: %v", err)}, nil
		}
		r.syncProviderLocked(r.active.Channel, r.active.Model, r.active.Effort)
		return &SwitchResult{
			Status:  "success",
			Scope:   MindScopePartner,
			Channel: r.active.Channel,
			Model:   r.active.Model,
			Effort:  r.active.Effort,
			Message: fmt.Sprintf("已将搭档私聊 %s 通道模型 (%s) 的思考强度调整为: %s", r.active.Channel, r.active.Model, effortReq),
		}, nil
	}
}

func formatEffortNotice(effort string) string {
	if effort == "" {
		return ""
	}
	return fmt.Sprintf("，思考强度: %s", effort)
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
