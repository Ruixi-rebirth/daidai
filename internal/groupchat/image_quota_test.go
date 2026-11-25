package groupchat

import (
	"context"
	"testing"
	"time"

	"daidai/internal/db"
)

func TestImageQuotaStore_Lifecycle(t *testing.T) {
	ctx := context.Background()
	dbMgr := db.NewManager(t.TempDir())
	defer func() { _ = dbMgr.Close() }()

	store := &ImageQuotaStore{Database: dbMgr}
	day1 := time.Date(2026, 11, 25, 10, 0, 0, 0, time.Local)
	day2 := time.Date(2026, 11, 26, 9, 0, 0, 0, time.Local)

	// Default limit is 10.
	status, err := store.Get(ctx, "qq", "123456", day1)
	if err != nil {
		t.Fatalf("Get default failed: %v", err)
	}
	if status.Limit != DefaultDailyImageLimit || status.Used != 0 || status.Remaining != DefaultDailyImageLimit {
		t.Fatalf("unexpected default status: %+v", status)
	}

	// Set cap to 2 and consume twice, third blocked.
	status, err = store.SetLimit(ctx, "qq", "123456", 2, day1)
	if err != nil {
		t.Fatalf("SetLimit(2) failed: %v", err)
	}
	if status.Limit != 2 || status.Remaining != 2 {
		t.Fatalf("unexpected status after SetLimit(2): %+v", status)
	}

	allowed, status, err := store.TryConsume(ctx, "qq", "123456", day1)
	if err != nil || !allowed || status.Used != 1 || status.Remaining != 1 {
		t.Fatalf("first consume unexpected: allowed=%v status=%+v err=%v", allowed, status, err)
	}
	allowed, status, err = store.TryConsume(ctx, "qq", "123456", day1)
	if err != nil || !allowed || status.Used != 2 || status.Remaining != 0 {
		t.Fatalf("second consume unexpected: allowed=%v status=%+v err=%v", allowed, status, err)
	}
	allowed, status, err = store.TryConsume(ctx, "qq", "123456", day1)
	if err != nil || allowed || status.Used != 2 || status.Remaining != 0 {
		t.Fatalf("third consume should be rejected: allowed=%v status=%+v err=%v", allowed, status, err)
	}

	// Refund frees one slot on the same date.
	if err := store.Refund(ctx, "qq", "123456", status.Date); err != nil {
		t.Fatalf("Refund failed: %v", err)
	}
	status, err = store.Get(ctx, "qq", "123456", day1)
	if err != nil || status.Used != 1 || status.Remaining != 1 {
		t.Fatalf("after refund status unexpected: status=%+v err=%v", status, err)
	}

	// Next day resets usage while preserving limit.
	status, err = store.Get(ctx, "qq", "123456", day2)
	if err != nil {
		t.Fatalf("Get day2 failed: %v", err)
	}
	if status.Limit != 2 || status.Used != 0 || status.Remaining != 2 {
		t.Fatalf("day2 should reset used count: %+v", status)
	}

	// 0 disables non-partner drawing.
	status, err = store.SetLimit(ctx, "qq", "123456", 0, day2)
	if err != nil || !status.Disabled || status.Limit != 0 {
		t.Fatalf("SetLimit(0) unexpected: status=%+v err=%v", status, err)
	}
	allowed, _, err = store.TryConsume(ctx, "qq", "123456", day2)
	if err != nil || allowed {
		t.Fatalf("disabled group should reject non-partner consume: allowed=%v err=%v", allowed, err)
	}

	// Negative normalizes to -1 (unlimited).
	status, err = store.SetLimit(ctx, "qq", "123456", -5, day2)
	if err != nil || !status.Unlimited || status.Limit != -1 {
		t.Fatalf("SetLimit(-5) unexpected: status=%+v err=%v", status, err)
	}
	allowed, _, err = store.TryConsume(ctx, "qq", "123456", day2)
	if err != nil || !allowed {
		t.Fatalf("unlimited group should allow consume: allowed=%v err=%v", allowed, err)
	}
}
