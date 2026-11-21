package briefing

import (
	"context"
	"daidai/internal/db"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const schema = `
CREATE TABLE IF NOT EXISTS brief_editions(
 id TEXT PRIMARY KEY, status TEXT NOT NULL CHECK(status IN ('generating','ready','empty','failed')),
 content_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(content_json)), attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts>=0),
 updated_at INTEGER NOT NULL, error_kind TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS brief_subscriptions(
 platform TEXT NOT NULL CHECK(platform IN ('qq','telegram')), recipient_id TEXT NOT NULL CHECK(length(recipient_id)>0),
 is_group INTEGER NOT NULL CHECK(is_group IN (0,1)),
 owner_key TEXT NOT NULL,enabled INTEGER NOT NULL DEFAULT 0 CHECK(enabled IN (0,1)),last_viewed TEXT NOT NULL DEFAULT '',paused_day TEXT NOT NULL DEFAULT '',
 updated_at INTEGER NOT NULL,PRIMARY KEY(platform,recipient_id,is_group));
CREATE TABLE IF NOT EXISTS brief_deliveries(
 edition_id TEXT NOT NULL REFERENCES brief_editions(id),platform TEXT NOT NULL,recipient_id TEXT NOT NULL,is_group INTEGER NOT NULL CHECK(is_group IN (0,1)),
 status TEXT NOT NULL CHECK(status IN ('pending','sending','sent','retryable','partial','uncertain','expired','rejected')),
 attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts>=0),next_attempt INTEGER NOT NULL DEFAULT 0,
 delivered_text TEXT NOT NULL DEFAULT '',remaining_text TEXT NOT NULL DEFAULT '',error_kind TEXT NOT NULL DEFAULT '',
 notified INTEGER NOT NULL DEFAULT 0 CHECK(notified IN (0,1)),updated_at INTEGER NOT NULL,
 PRIMARY KEY(edition_id,platform,recipient_id,is_group),FOREIGN KEY(platform,recipient_id,is_group) REFERENCES brief_subscriptions(platform,recipient_id,is_group));
CREATE INDEX IF NOT EXISTS brief_due ON brief_deliveries(status,next_attempt);
`

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	conn, err := db.OpenSQLite(path, schema)
	if err != nil {
		return nil, err
	}
	s := &Store{conn}
	if _, err = conn.Exec(`UPDATE brief_editions SET status='failed',error_kind='interrupted' WHERE status='generating'`); err == nil {
		_, err = conn.Exec(`UPDATE brief_deliveries SET status='uncertain',error_kind='interrupted' WHERE status='sending'`)
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Edition(ctx context.Context, id string) (*Edition, string, error) {
	var body, status string
	err := s.db.QueryRowContext(ctx, `SELECT content_json,status FROM brief_editions WHERE id=?`, id).Scan(&body, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	if status != "ready" && status != "empty" {
		return nil, status, nil
	}
	var e Edition
	if err = json.Unmarshal([]byte(body), &e); err != nil {
		return nil, status, err
	}
	return &e, status, nil
}
func (s *Store) Begin(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO brief_editions(id,status,attempts,updated_at) VALUES(?,'generating',1,?) ON CONFLICT(id) DO UPDATE SET status='generating',attempts=attempts+1,updated_at=excluded.updated_at,error_kind='' WHERE status='failed'`, id, now.UnixMilli())
	return err
}
func (s *Store) Save(ctx context.Context, e *Edition) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(b) > 256<<10 {
		return fmt.Errorf("edition exceeds 256 KiB")
	}
	status := "ready"
	if len(e.Items) == 0 {
		status = "empty"
	}
	r, err := s.db.ExecContext(ctx, `UPDATE brief_editions SET status=?,content_json=?,updated_at=?,error_kind='' WHERE id=? AND status='generating'`, status, string(b), time.Now().UnixMilli(), e.ID)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		err = fmt.Errorf("edition is not generating")
	}
	return err
}
func (s *Store) Fail(ctx context.Context, id, kind string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE brief_editions SET status='failed',error_kind=?,updated_at=? WHERE id=? AND status='generating'`, kind, time.Now().UnixMilli(), id)
	return err
}
func (s *Store) CanRetry(ctx context.Context, id string, now time.Time) (bool, error) {
	var attempts int
	var updated int64
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT attempts,updated_at,status FROM brief_editions WHERE id=?`, id).Scan(&attempts, &updated, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return status == "failed" && attempts < 3 && now.Sub(time.UnixMilli(updated)) >= 5*time.Minute, nil
}
func (s *Store) RecentIdentities(ctx context.Context, before time.Time) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT content_json FROM brief_editions WHERE id>=? AND id<? AND status IN ('ready','empty')`, before.AddDate(0, 0, -7).Format("2006-01-02"), before.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	for rows.Next() {
		var b string
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		var e Edition
		if err = json.Unmarshal([]byte(b), &e); err != nil {
			return nil, err
		}
		for _, i := range e.Items {
			seen[i.ID] = true
			seen[identity(i)] = true
		}
	}
	return seen, rows.Err()
}
func (s *Store) Touch(ctx context.Context, sub Subscription, enable *bool) error {
	v := false
	if enable != nil {
		v = *enable
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO brief_subscriptions(platform,recipient_id,is_group,owner_key,enabled,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(platform,recipient_id,is_group) DO UPDATE SET owner_key=excluded.owner_key,enabled=CASE WHEN ? THEN excluded.enabled ELSE enabled END,paused_day=CASE WHEN ? THEN '' ELSE paused_day END,updated_at=excluded.updated_at`, sub.Platform, sub.Recipient, sub.IsGroup, sub.Owner, v, time.Now().UnixMilli(), enable != nil, enable != nil && v)
	return err
}
func (s *Store) Subscription(ctx context.Context, p, r string, group bool) (*Subscription, error) {
	sub := &Subscription{Platform: p, Recipient: r, IsGroup: group}
	err := s.db.QueryRowContext(ctx, `SELECT owner_key,enabled,last_viewed,paused_day FROM brief_subscriptions WHERE platform=? AND recipient_id=? AND is_group=?`, p, r, group).Scan(&sub.Owner, &sub.Enabled, &sub.LastViewed, &sub.PausedDay)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return sub, err
}
func (s *Store) Subscriptions(ctx context.Context) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT platform,recipient_id,is_group,owner_key,enabled,last_viewed,paused_day FROM brief_subscriptions ORDER BY platform,is_group,recipient_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Subscription
	for rows.Next() {
		var sub Subscription
		if err = rows.Scan(&sub.Platform, &sub.Recipient, &sub.IsGroup, &sub.Owner, &sub.Enabled, &sub.LastViewed, &sub.PausedDay); err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}
func (s *Store) Viewed(ctx context.Context, p, r string, group bool, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE brief_subscriptions SET last_viewed=?,updated_at=? WHERE platform=? AND recipient_id=? AND is_group=?`, id, time.Now().UnixMilli(), p, r, group)
	return err
}
func (s *Store) Delivery(ctx context.Context, id, p, r string, group bool) (*Delivery, error) {
	d := &Delivery{Edition: id, Platform: p, Recipient: r, IsGroup: group}
	var next int64
	err := s.db.QueryRowContext(ctx, `SELECT status,attempts,next_attempt,delivered_text,remaining_text,error_kind,notified FROM brief_deliveries WHERE edition_id=? AND platform=? AND recipient_id=? AND is_group=?`, id, p, r, group).Scan(&d.Status, &d.Attempts, &next, &d.Delivered, &d.Remaining, &d.ErrorKind, &d.Notified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	d.Next = time.UnixMilli(next)
	return d, err
}
func (s *Store) SaveDelivery(ctx context.Context, d *Delivery) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO brief_deliveries(edition_id,platform,recipient_id,is_group,status,attempts,next_attempt,delivered_text,remaining_text,error_kind,notified,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(edition_id,platform,recipient_id,is_group) DO UPDATE SET status=excluded.status,attempts=excluded.attempts,next_attempt=excluded.next_attempt,delivered_text=excluded.delivered_text,remaining_text=excluded.remaining_text,error_kind=excluded.error_kind,notified=excluded.notified,updated_at=excluded.updated_at`, d.Edition, d.Platform, d.Recipient, d.IsGroup, d.Status, d.Attempts, d.Next.UnixMilli(), d.Delivered, d.Remaining, d.ErrorKind, d.Notified, time.Now().UnixMilli())
	return err
}

type deliveryFailure struct {
	editionID string
	status    string
	errorKind string
	attempts  int
}

func (s *Store) LatestRemainingEdition(ctx context.Context, sub Subscription) (string, error) {
	var editionID string
	err := s.db.QueryRowContext(ctx, `SELECT edition_id FROM brief_deliveries
		WHERE platform=? AND recipient_id=? AND is_group=? AND status NOT IN ('sent','sending')
		AND remaining_text<>'' ORDER BY edition_id DESC LIMIT 1`, sub.Platform, sub.Recipient, sub.IsGroup).Scan(&editionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return editionID, err
}

func (s *Store) latestUnnotifiedFailure(ctx context.Context, p, r string, group bool) (*deliveryFailure, error) {
	var failure deliveryFailure
	err := s.db.QueryRowContext(ctx, `SELECT edition_id,status,error_kind,attempts FROM brief_deliveries WHERE platform=? AND recipient_id=? AND is_group=? AND status IN ('partial','uncertain','expired','rejected') AND notified=0 ORDER BY edition_id DESC LIMIT 1`, p, r, group).
		Scan(&failure.editionID, &failure.status, &failure.errorKind, &failure.attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &failure, err
}
func (s *Store) markFailureNotified(ctx context.Context, failure *deliveryFailure, p, r string, group bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE brief_deliveries SET notified=1 WHERE edition_id=? AND platform=? AND recipient_id=? AND is_group=? AND attempts=? AND status=? AND error_kind=?`,
		failure.editionID, p, r, group, failure.attempts, failure.status, failure.errorKind)
	return err
}

func (s *Store) Expire(ctx context.Context, day string, windowClosed bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE brief_deliveries SET status='expired',next_attempt=0 WHERE status IN ('pending','retryable') AND (edition_id<? OR (edition_id=? AND ?))`, day, day, windowClosed)
	return err
}

func (s *Store) Pause(ctx context.Context, p, r string, group bool, day string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE brief_subscriptions SET paused_day=? WHERE platform=? AND recipient_id=? AND is_group=?`, day, p, r, group)
	return err
}
