package groupchat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"daidai/internal/db"
)

const (
	Chatter = "chatter"
	Dream   = "dream"
)

// State is scoped to one platform and group (including a Telegram topic).
// Flags and successful-send times share a small runtime_state JSON record.
type State struct {
	Chatter       bool  `json:"chatter"`
	Dream         bool  `json:"dream"`
	LastChatterMS int64 `json:"last_chatter_ms"`
	LastDreamMS   int64 `json:"last_dream_ms"`
}

type Patch struct {
	Chatter *bool `json:"chatter,omitempty"`
	Dream   *bool `json:"dream,omitempty"`
}

type Store struct{ Database *db.Manager }

func stateKey(platform, group string) (string, error) {
	if (platform != "qq" && platform != "telegram") || strings.TrimSpace(group) == "" {
		return "", fmt.Errorf("group expressions require a QQ or Telegram group")
	}
	return "group_expressions:" + group, nil
}

func (s *Store) Get(ctx context.Context, platform, group string) (State, error) {
	var state State
	key, err := stateKey(platform, group)
	if err != nil {
		return state, err
	}
	if s == nil || s.Database == nil {
		return state, fmt.Errorf("group expression store is unavailable")
	}
	value, err := s.Database.GetRuntimeState(ctx, platform, key)
	if err != nil || value == "" {
		return state, err
	}
	err = json.Unmarshal([]byte(value), &state)
	return state, err
}

func (s *Store) Update(ctx context.Context, platform, group string, patch Patch) error {
	if patch.Chatter == nil && patch.Dream == nil {
		return fmt.Errorf("specify chatter or dream")
	}
	return s.merge(ctx, platform, group, patch)
}

func (s *Store) MarkSent(ctx context.Context, platform, group, kind string, at time.Time) error {
	var field string
	switch kind {
	case Chatter:
		field = "last_chatter_ms"
	case Dream:
		field = "last_dream_ms"
	default:
		return fmt.Errorf("invalid group expression kind")
	}
	return s.merge(ctx, platform, group, map[string]int64{field: at.UnixMilli()})
}

func (s *Store) merge(ctx context.Context, platform, group string, patch any) error {
	key, err := stateKey(platform, group)
	if err != nil {
		return err
	}
	if s == nil || s.Database == nil {
		return fmt.Errorf("group expression store is unavailable")
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	conn, err := s.Database.GetConn(platform)
	if err != nil {
		return err
	}
	// Atomic JSON merge preserves concurrent flag changes and send timestamps.
	_, err = conn.ExecContext(ctx, `INSERT INTO runtime_state(state_key,value,updated_at) VALUES(?,?,?)
		ON CONFLICT(state_key) DO UPDATE SET value=json_patch(runtime_state.value,excluded.value),updated_at=excluded.updated_at`, key, string(body), time.Now().UnixMilli())
	return err
}

// Available is a rate guard, not a timer. A fresh human group message and the
// model's judgement are still required, even after a cooldown has elapsed.
func (s State) Available(at time.Time) (chatter, dream bool) {
	last := max(s.LastChatterMS, s.LastDreamMS)
	if last > 0 && at.Sub(time.UnixMilli(last)) < time.Hour {
		return false, false
	}
	return s.Chatter, s.Dream && (s.LastDreamMS == 0 || at.Sub(time.UnixMilli(s.LastDreamMS)) >= 6*time.Hour)
}
