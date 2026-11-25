package llm

import (
	"fmt"
	"strings"
)

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

// SplitModelAndEffort 拆分模型名称末尾可能附带的标准思考强度后缀 (如 "gemini-4-argon-high" 或 "Gemini 4 Hydrogen (High)" -> base, "high")
func SplitModelAndEffort(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	lower := strings.ToLower(raw)

	for _, eff := range []string{"xhigh", "medium", "high", "low", "max"} {
		for _, sep := range []string{"-", " "} {
			suffix := sep + eff
			if strings.HasSuffix(lower, suffix) {
				base := strings.TrimSpace(raw[:len(raw)-len(suffix)])
				return strings.TrimSuffix(base, "-"), eff
			}
		}
		parenSuffix := "(" + eff + ")"
		if strings.HasSuffix(lower, parenSuffix) {
			return strings.TrimSpace(raw[:len(raw)-len(parenSuffix)]), eff
		}
	}
	return raw, ""
}
