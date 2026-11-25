package entity

import (
	"context"
	"daidai/internal/core"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

type ChannelRegistry struct {
	mu          sync.RWMutex
	lifecycleMu sync.Mutex
	channels    map[string]Channel
}

type channelStartError struct {
	startErr    error
	rollbackErr error
}

func (e *channelStartError) Error() string {
	if e.rollbackErr == nil {
		return e.startErr.Error()
	}
	return fmt.Sprintf("%v; channel startup rollback incomplete: %v", e.startErr, e.rollbackErr)
}

func (e *channelStartError) Unwrap() error { return e.startErr }

func (e *channelStartError) RollbackFailed() bool { return e.rollbackErr != nil }

func NewChannelRegistry() *ChannelRegistry {
	return &ChannelRegistry{
		channels: make(map[string]Channel),
	}
}

func (r *ChannelRegistry) Register(ch Channel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channels[ch.Name()] = ch
}

func (r *ChannelRegistry) Get(name string) (Channel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ch, ok := r.channels[name]
	return ch, ok
}

func (r *ChannelRegistry) FormatStatus() string {
	r.mu.RLock()
	names := make([]string, 0, len(r.channels))
	channels := make(map[string]Channel, len(r.channels))
	for name, channel := range r.channels {
		names = append(names, name)
		channels[name] = channel
	}
	r.mu.RUnlock()
	sort.Strings(names)
	if len(names) == 0 {
		return "### 消息通道\n- 未挂载任何消息通道"
	}
	var lines []string
	for _, name := range names {
		if formatter, ok := channels[name].(interface{ FormatChannelStatus() string }); ok {
			lines = append(lines, formatter.FormatChannelStatus())
			continue
		}
		state := "连接状态未知"
		if probe, ok := channels[name].(interface{ Ready() bool }); ok {
			if probe.Ready() {
				state = "已连接"
			} else {
				state = "未连接"
			}
		}
		lines = append(lines, fmt.Sprintf("- %s：%s", name, state))
	}
	return "### 消息通道\n" + strings.Join(lines, "\n")
}

func (r *ChannelRegistry) StartAll(ctx context.Context, onSensory func(event *SensoryEvent)) error {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}

	r.mu.RLock()
	names := make([]string, 0, len(r.channels))
	for name := range r.channels {
		names = append(names, name)
	}
	sort.Strings(names)
	channels := make([]Channel, len(names))
	for i, name := range names {
		channels[i] = r.channels[name]
	}
	r.mu.RUnlock()

	started := make([]Channel, 0, len(names))
	rollback := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var rollbackErrs []error
		for i := len(started) - 1; i >= 0; i-- {
			if stopErr := started[i].Stop(cleanupCtx); stopErr != nil {
				slog.Warn("channel: 启动失败后的回滚停止未完全成功", "name", started[i].Name(), "err", stopErr)
				rollbackErrs = append(rollbackErrs, fmt.Errorf("stop channel %s: %w", started[i].Name(), stopErr))
			}
		}
		return errors.Join(rollbackErrs...)
	}
	for i, name := range names {
		if err := ctx.Err(); err != nil {
			return &channelStartError{startErr: fmt.Errorf("channel startup canceled: %w", err), rollbackErr: rollback()}
		}
		ch := channels[i]
		slog.Info("channel: 正在启动物理通道", "name", name)
		started = append(started, ch)
		if err := ch.Start(ctx, onSensory); err != nil {
			return &channelStartError{startErr: fmt.Errorf("failed to start channel %s: %w", name, err), rollbackErr: rollback()}
		}
	}
	if err := ctx.Err(); err != nil {
		return &channelStartError{startErr: fmt.Errorf("channel startup canceled: %w", err), rollbackErr: rollback()}
	}
	return nil
}

func (r *ChannelRegistry) StopAll(ctx context.Context) error {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}

	r.mu.RLock()
	var firstErr error
	names := make([]string, 0, len(r.channels))
	for name := range r.channels {
		names = append(names, name)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	channels := make([]Channel, len(names))
	for i, name := range names {
		channels[i] = r.channels[name]
	}
	r.mu.RUnlock()
	for i := range names {
		ch := channels[i]
		if err := ch.Stop(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *ChannelRegistry) Transmit(ctx context.Context, u *Utterance) error {
	if u == nil {
		return nil
	}
	if u.Platform == "" {
		return fmt.Errorf("empty platform in utterance")
	}

	if u.IsGroup {
		u.Text = core.RedactSensitiveIPs(u.Text)
		u.FallbackText = core.RedactSensitiveIPs(u.FallbackText)
		for i := range u.ImageSources {
			u.ImageSources[i] = core.RedactSensitiveIPs(u.ImageSources[i])
		}
	}

	ch, ok := r.Get(u.Platform)
	if !ok {
		return fmt.Errorf("channel %q not mounted", u.Platform)
	}

	if err := ch.Transmit(ctx, u); err != nil {
		if u.OnDeliveryFailed != nil {
			commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			u.OnDeliveryFailed(commit, err)
		}
		return err
	}
	if u.OnDelivered != nil {
		commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		u.OnDelivered(commitCtx)
	}
	return nil
}

// SuperAgent 呆呆超级数字生命体核心聚合根 (仅持有实际调度的核心职责)
