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
