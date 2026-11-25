package groupchat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"daidai/internal/db"
)

const (
	// DefaultDailyImageLimit is the default per-group daily non-partner image limit.
	DefaultDailyImageLimit = 10

	imageQuotaStatePrefix = "group_image_quota:"
)

// ImageQuotaState persists per-group daily drawing limit and usage in runtime_state.
type ImageQuotaState struct {
	DailyLimit *int   `json:"daily_limit,omitempty"`
	Date       string `json:"date,omitempty"`
	Used       int    `json:"used,omitempty"`
}

// ImageQuotaStatus represents resolved quota settings and today's usage for a group.
type ImageQuotaStatus struct {
	Limit     int
	Used      int
	Remaining int
	Date      string
	Unlimited bool
	Disabled  bool
}

// ImageQuotaStore manages per-group daily drawing quotas backed by SQLite runtime_state.
type ImageQuotaStore struct {
	Database *db.Manager
}

func imageQuotaStateKey(group string) string {
	return imageQuotaStatePrefix + strings.TrimSpace(group)
}

func validateGroupTarget(platform, group string) (string, string, error) {
	platform = strings.ToLower(strings.TrimSpace(platform))
	group = strings.TrimSpace(group)
	if platform != "qq" && platform != "telegram" {
		return "", "", fmt.Errorf("unsupported platform %q", platform)
	}
	if group == "" {
		return "", "", fmt.Errorf("group channel id is required")
	}
	return platform, group, nil
}

func localDayString(now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	return now.In(time.Local).Format("2006-01-02")
}

func (s ImageQuotaState) effectiveLimit() int {
	if s.DailyLimit == nil {
		return DefaultDailyImageLimit
	}
	if *s.DailyLimit < 0 {
		return -1
	}
	return *s.DailyLimit
}

func (s ImageQuotaState) normalizeForDay(day string) ImageQuotaState {
	if s.Date != day {
		s.Date = day
		s.Used = 0
	}
	if s.Used < 0 {
		s.Used = 0
	}
	return s
}

func (s ImageQuotaState) status() ImageQuotaStatus {
	limit := s.effectiveLimit()
	st := ImageQuotaStatus{
		Limit:     limit,
		Used:      s.Used,
		Date:      s.Date,
		Unlimited: limit < 0,
		Disabled:  limit == 0,
	}
	if limit > 0 {
		rem := limit - s.Used
		if rem < 0 {
			rem = 0
		}
		st.Remaining = rem
	}
	return st
}

func loadImageQuotaStateTx(ctx context.Context, tx *sql.Tx, key string) (ImageQuotaState, error) {
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT value FROM runtime_state WHERE state_key = ?", key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || strings.TrimSpace(raw) == "" {
		return ImageQuotaState{}, nil
	}
	if err != nil {
		return ImageQuotaState{}, err
	}
	var state ImageQuotaState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return ImageQuotaState{}, fmt.Errorf("decode image quota state: %w", err)
	}
	return state, nil
}

func saveImageQuotaStateTx(ctx context.Context, tx *sql.Tx, key string, state ImageQuotaState, now time.Time) error {
	payload, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode image quota state: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO runtime_state (state_key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(state_key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, string(payload), now.UnixMilli())
	return err
}

// Get returns the current group's image quota status for the local calendar day.
func (s *ImageQuotaStore) Get(ctx context.Context, platform, group string, now time.Time) (ImageQuotaStatus, error) {
	if s == nil || s.Database == nil {
		return ImageQuotaStatus{}, fmt.Errorf("database manager not configured")
	}
	platform, group, err := validateGroupTarget(platform, group)
	if err != nil {
		return ImageQuotaStatus{}, err
	}
	raw, err := s.Database.GetRuntimeState(ctx, platform, imageQuotaStateKey(group))
	if err != nil {
		return ImageQuotaStatus{}, err
	}
	var state ImageQuotaState
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			return ImageQuotaStatus{}, fmt.Errorf("decode image quota state: %w", err)
		}
	}
	state = state.normalizeForDay(localDayString(now))
	return state.status(), nil
}

// SetLimit configures a group's daily non-partner image limit.
// limit > 0 sets the daily cap, 0 disables non-partner drawing, and < 0 sets unlimited (-1).
func (s *ImageQuotaStore) SetLimit(ctx context.Context, platform, group string, limit int, now time.Time) (ImageQuotaStatus, error) {
	if s == nil || s.Database == nil {
		return ImageQuotaStatus{}, fmt.Errorf("database manager not configured")
	}
	platform, group, err := validateGroupTarget(platform, group)
	if err != nil {
		return ImageQuotaStatus{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	conn, err := s.Database.GetConn(platform)
	if err != nil {
		return ImageQuotaStatus{}, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return ImageQuotaStatus{}, err
	}
	defer func() { _ = tx.Rollback() }()

	key := imageQuotaStateKey(group)
	state, err := loadImageQuotaStateTx(ctx, tx, key)
	if err != nil {
		return ImageQuotaStatus{}, err
	}
	state = state.normalizeForDay(localDayString(now))
	normalized := limit
	if normalized < 0 {
		normalized = -1
	}
	state.DailyLimit = &normalized
	if err := saveImageQuotaStateTx(ctx, tx, key, state, now); err != nil {
		return ImageQuotaStatus{}, err
	}
	if err := tx.Commit(); err != nil {
		return ImageQuotaStatus{}, err
	}
	return state.status(), nil
}

// TryConsume atomically reserves one drawing slot for a non-partner user in a group.
// When allowed is true and status.Limit > 0, callers should Refund if generation fails.
func (s *ImageQuotaStore) TryConsume(ctx context.Context, platform, group string, now time.Time) (bool, ImageQuotaStatus, error) {
	if s == nil || s.Database == nil {
		return false, ImageQuotaStatus{}, fmt.Errorf("database manager not configured")
	}
	platform, group, err := validateGroupTarget(platform, group)
	if err != nil {
		return false, ImageQuotaStatus{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	conn, err := s.Database.GetConn(platform)
	if err != nil {
		return false, ImageQuotaStatus{}, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, ImageQuotaStatus{}, err
	}
	defer func() { _ = tx.Rollback() }()

	key := imageQuotaStateKey(group)
	state, err := loadImageQuotaStateTx(ctx, tx, key)
	if err != nil {
		return false, ImageQuotaStatus{}, err
	}
	state = state.normalizeForDay(localDayString(now))

	limit := state.effectiveLimit()
	if limit == 0 {
		return false, state.status(), nil
	}
	if limit < 0 {
		return true, state.status(), nil
	}
	if state.Used >= limit {
		return false, state.status(), nil
	}
	state.Used++
	if err := saveImageQuotaStateTx(ctx, tx, key, state, now); err != nil {
		return false, ImageQuotaStatus{}, err
	}
	if err := tx.Commit(); err != nil {
		return false, ImageQuotaStatus{}, err
	}
	return true, state.status(), nil
}

// Refund returns one consumed quota slot for the given calendar day when generation fails.
func (s *ImageQuotaStore) Refund(ctx context.Context, platform, group, date string) error {
	if s == nil || s.Database == nil {
		return fmt.Errorf("database manager not configured")
	}
	platform, group, err := validateGroupTarget(platform, group)
	if err != nil {
		return err
	}
	date = strings.TrimSpace(date)
	if date == "" {
		return nil
	}
	conn, err := s.Database.GetConn(platform)
	if err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	key := imageQuotaStateKey(group)
	state, err := loadImageQuotaStateTx(ctx, tx, key)
	if err != nil {
		return err
	}
	if state.Date != date || state.Used <= 0 {
		return nil
	}
	state.Used--
	if err := saveImageQuotaStateTx(ctx, tx, key, state, time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}
