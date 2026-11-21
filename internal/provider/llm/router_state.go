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
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
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
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err == nil && isEmptyChatResponse(resp) {
			err = fmt.Errorf("兜底通道 %q 返回空响应", r.fallbackChannel)
		}
		if err != nil {
			return nil, fmt.Errorf("当前激活通道 %q 未挂载，且兜底通道执行失败: %w", channel, err)
		}
		return resp, nil
	}

	resp, err := activeProvider.ChatWithTools(ctx, messages, tools, opts)
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
	var unsafeErr *core.UncertainExecutionError
	if errors.As(err, &unsafeErr) {
		return nil, fmt.Errorf("%s 通道执行结果不确定；为避免重复操作，已停止自动兜底: %w", channel, err)
	}

	if fallbackProvider != nil && !strings.EqualFold(channel, r.fallbackChannel) {
		slog.Warn("当前心智通道执行遇到异常，启动弹性降级机制至兜底通道",
			"channel", channel, "model", model, "fallbackChannel", r.fallbackChannel, "err", err)

		fbResp, fbErr := fallbackProvider.ChatWithTools(ctx, messages, tools, opts)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
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
	return resp == nil || (len(resp.ToolCalls) == 0 && strings.TrimSpace(resp.Content) == "" && !core.OutputLimitReached(resp.FinishReason))
}
