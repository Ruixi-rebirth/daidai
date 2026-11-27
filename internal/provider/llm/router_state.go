package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"daidai/internal/core"
)

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
	if st.Public != nil && st.Public.Channel != "" && st.Public.Model != "" {
		eff, _ := capEffortValue(st.Public.Effort)
		r.public = MindSlotState{
			Channel:   st.Public.Channel,
			Model:     st.Public.Model,
			Effort:    eff,
			UpdatedAt: st.Public.UpdatedAt,
		}
		r.publicPinned = true
	}
	st.Public = nil
	r.active = st
	return nil
}

func (r *MindRouter) saveState() error {
	dir := filepath.Dir(r.statePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	persisted := r.active
	if r.publicPinned && r.public.Channel != "" && r.public.Model != "" {
		pubCopy := r.public
		persisted.Public = &pubCopy
	} else {
		persisted.Public = nil
	}
	data, err := json.MarshalIndent(persisted, "", "  ")
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
	usePublic := opts != nil && opts.ReadOnly
	type fallbackCandidate struct {
		channel  string
		provider core.LLMProvider
	}
	r.mu.RLock()
	slot := MindSlotState{
		Channel: r.active.Channel,
		Model:   r.active.Model,
		Effort:  r.active.Effort,
	}
	if usePublic && r.public.Channel != "" && r.public.Model != "" {
		slot = r.public
	}
	channel := slot.Channel
	model := slot.Model
	activeProvider := r.providers[channel]
	fallbacks := make([]fallbackCandidate, 0, len(r.fallbackChannels))
	for _, ch := range r.fallbackChannels {
		if strings.EqualFold(channel, ch) {
			continue
		}
		if prov := r.providers[ch]; prov != nil {
			fallbacks = append(fallbacks, fallbackCandidate{channel: ch, provider: prov})
		}
	}
	r.mu.RUnlock()

	fallbackOpts := stripRouteModelOpts(opts)
	primaryOpts := withRouteModelOpts(opts, slot.Model, slot.Effort)

	runFallbacks := func(reason string) (*core.ChatResponse, string, error) {
		var lastErr error
		var lastCh string
		var cooled []fallbackCandidate
		for _, cand := range fallbacks {
			candModel := providerModelID(cand.provider)
			if rem := r.modelCooldownRemaining(cand.channel, candModel); rem > 0 {
				cooled = append(cooled, cand)
				continue
			}
			resp, err := cand.provider.ChatWithTools(ctx, messages, tools, fallbackOpts)
			if ctx.Err() != nil {
				return nil, cand.channel, ctx.Err()
			}
			if err == nil && isEmptyChatResponse(resp) {
				err = fmt.Errorf("兜底通道 %q 返回空响应", cand.channel)
			}
			if err == nil {
				return resp, cand.channel, nil
			}
			if cd := ParseProviderCooldown(err); cd > 0 {
				r.setModelCooldown(cand.channel, candModel, cd)
			}
			slog.Warn("兜底心智通道执行失败", "reason", reason, "channel", cand.channel, "model", candModel, "err", err)
			lastErr = err
			lastCh = cand.channel
		}
		// If every fallback was in cooldown and none succeeded, try the final cooled candidate as last resort.
		if lastErr == nil && len(cooled) > 0 {
			cand := cooled[len(cooled)-1]
			resp, err := cand.provider.ChatWithTools(ctx, messages, tools, fallbackOpts)
			if ctx.Err() != nil {
				return nil, cand.channel, ctx.Err()
			}
			if err == nil && isEmptyChatResponse(resp) {
				err = fmt.Errorf("兜底通道 %q 返回空响应", cand.channel)
			}
			if err == nil {
				return resp, cand.channel, nil
			}
			lastErr = err
			lastCh = cand.channel
		}
		return nil, lastCh, lastErr
	}

	// 高频机械分类/门控请求优先直连低延迟兜底链（例如免费 GLM → DeepSeek），避免为布尔判定拉起 CLI 子进程。
	if opts != nil && opts.FastRoute && len(fallbacks) > 0 {
		resp, _, err := runFallbacks("fast_route")
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err == nil && !isEmptyChatResponse(resp) {
			return resp, nil
		}
	}
	if activeProvider == nil {
		if len(fallbacks) == 0 {
			return nil, fmt.Errorf("当前激活的心智通道 %q 未挂载任何可用驱动", channel)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		slog.Warn("当前激活的心智通道未挂载，降级至兜底链", "channel", channel)
		resp, _, err := runFallbacks("unmounted_primary")
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			return nil, fmt.Errorf("当前激活通道 %q 未挂载，且兜底链执行失败: %w", channel, err)
		}
		return resp, nil
	}

	if rem := r.modelCooldownRemaining(channel, model); rem > 0 && len(fallbacks) > 0 {
		slog.Info("心智模型处于限流/容量冷却窗口，直接路由至兜底链",
			"channel", channel, "model", model, "remaining", rem.Round(time.Second))
		fbResp, fbCh, fbErr := runFallbacks("primary_cooldown")
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if fbErr == nil {
			return fbResp, nil
		}
		return nil, fmt.Errorf("%s (%s) 处于冷却窗口且兜底通道 %q 执行失败: %w", channel, model, fbCh, fbErr)
	}

	resp, err := activeProvider.ChatWithTools(ctx, messages, tools, primaryOpts)
	if ctx.Err() != nil {
		var unsafeErr *core.UncertainExecutionError
		if errors.As(err, &unsafeErr) {
			return nil, fmt.Errorf("%w: %w", ctx.Err(), err)
		}
		return nil, ctx.Err()
	}
	if err == nil && isEmptyChatResponse(resp) {
		err = fmt.Errorf("%s 通道返回空响应", channel)
	}
	if err == nil {
		return resp, nil
	}
	if cd := ParseProviderCooldown(err); cd > 0 {
		r.setModelCooldown(channel, model, cd)
	}
	var unsafeErr *core.UncertainExecutionError
	if errors.As(err, &unsafeErr) {
		return nil, fmt.Errorf("%s 通道执行结果不确定；为避免重复操作，已停止自动兜底: %w", channel, err)
	}

	if len(fallbacks) > 0 {
		slog.Warn("当前心智通道执行遇到异常，启动弹性降级机制至兜底链",
			"channel", channel, "model", model, "err", err)
		fbResp, _, fbErr := runFallbacks("primary_error")
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if fbErr == nil {
			return fbResp, nil
		}
	}

	return nil, fmt.Errorf("%s 通道心智驱动执行失败: %w", channel, err)
}

func withRouteModelOpts(opts *core.ChatOptions, model, effort string) *core.ChatOptions {
	if strings.TrimSpace(model) == "" && strings.TrimSpace(effort) == "" {
		return opts
	}
	if opts == nil {
		return &core.ChatOptions{
			Temperature: 0.7,
			Model:       model,
			Effort:      effort,
		}
	}
	copied := *opts
	copied.Model = model
	copied.Effort = effort
	return &copied
}

func stripRouteModelOpts(opts *core.ChatOptions) *core.ChatOptions {
	if opts == nil || (opts.Model == "" && opts.Effort == "") {
		return opts
	}
	copied := *opts
	copied.Model = ""
	copied.Effort = ""
	return &copied
}

func isEmptyChatResponse(resp *core.ChatResponse) bool {
	return resp == nil || (len(resp.ToolCalls) == 0 && strings.TrimSpace(resp.Content) == "" && !core.OutputLimitReached(resp.FinishReason))
}
