package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"daidai/internal/entity"
	"daidai/internal/kernel"
)

func (c *Channel) Start(ctx context.Context, handler func(*entity.SensoryEvent)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		return fmt.Errorf("telegram already started")
	}
	if err := c.jsonCall(ctx, "getMe", map[string]any{}, &c.bot); err != nil {
		return err
	}
	var webhook struct {
		URL string `json:"url"`
	}
	if err := c.jsonCall(ctx, "getWebhookInfo", map[string]any{}, &webhook); err != nil {
		return err
	}
	if webhook.URL != "" {
		return fmt.Errorf("telegram bot has an existing webhook; polling cannot start")
	}
	if err := c.jsonCall(ctx, "setMyCommands", map[string]any{"commands": entity.CommandMenu("telegram")}, nil); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("telegram: command menu registration failed; continue polling", "err", err)
	}
	if err := c.initQuotes(ctx); err != nil {
		return err
	}
	if c.database == nil {
		return fmt.Errorf("telegram update offset store is unavailable")
	}
	savedOffset, err := c.database.GetRuntimeState(ctx, "telegram", "update_offset")
	if err != nil {
		return fmt.Errorf("read Telegram update offset: %w", err)
	}
	offset := int64(0)
	if savedOffset != "" {
		offset, err = strconv.ParseInt(savedOffset, 10, 64)
		if err != nil || offset < 0 {
			return fmt.Errorf("invalid persisted Telegram update offset %q", savedOffset)
		}
	}
	pollCtx, cancel := context.WithCancel(ctx)
	c.queue = kernel.NewDebounceManager(kernel.DebounceConfig{MergeWindow: 2 * time.Second}, handler)
	c.cancel = cancel
	c.stopping = false
	c.done = make(chan struct{})
	go c.poll(pollCtx, handler, offset)
	return nil
}
func (c *Channel) poll(ctx context.Context, handler func(*entity.SensoryEvent), offset int64) {
	defer close(c.done)
	defer c.ready.Store(false)
	for ctx.Err() == nil {
		var updates []update
		err := c.jsonCall(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 25, "allowed_updates": []string{"message"}}, &updates)
		if err != nil {
			c.ready.Store(false)
			if ctx.Err() != nil {
				return
			}
			slog.Warn("telegram: polling failed", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		c.ready.Store(true)
		for _, u := range updates {
			if ctx.Err() != nil {
				return
			}
			if err := c.handle(ctx, u.Message, handler); err != nil {
				slog.Warn("telegram: update handling failed", "updateID", u.ID, "err", err)
				var apiErr *apiError
				if !errors.As(err, &apiErr) || (apiErr.code != 400 && apiErr.code != 403) {
					select {
					case <-ctx.Done():
						return
					case <-time.After(3 * time.Second):
					}
					break
				}
			}
			offset = u.ID + 1
			persistenceFailed := false
			for ctx.Err() == nil {
				if err := c.database.SetRuntimeState(ctx, "telegram", "update_offset", strconv.FormatInt(offset, 10)); err == nil {
					c.ready.Store(true)
					if persistenceFailed {
						slog.Info("telegram: update offset persistence recovered")
					}
					break
				} else {
					c.ready.Store(false)
					if !persistenceFailed {
						slog.Warn("telegram: offset persistence failed; polling is paused to avoid acknowledging uncheckpointed updates", "err", err)
					}
					persistenceFailed = true
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(3 * time.Second):
				}
			}
		}
	}
}

func (c *Channel) Stop(ctx context.Context) error {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.stopping = true
	for _, group := range c.groups {
		if group.timer != nil {
			group.timer.Stop()
		}
	}
	c.groups = nil
	c.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		if c.queue != nil {
			if err := c.queue.Shutdown(ctx); err != nil {
				return err
			}
		}
		c.mu.Lock()
		c.cancel = nil
		c.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
