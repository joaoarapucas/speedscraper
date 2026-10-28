package store

import (
	"context"
	"database/sql"
	"time"
)

// AlertLastSent returns when an alert kind was last sent (zero if never).
func (db *DB) AlertLastSent(ctx context.Context, kind string) (time.Time, error) {
	var s string
	err := db.W.QueryRowContext(ctx, "SELECT last_sent_at FROM alerts_sent WHERE kind = ?", kind).Scan(&s)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(TimeFormat, s)
}

func (db *DB) MarkAlertSent(ctx context.Context, kind string) error {
	_, err := db.W.ExecContext(ctx, `INSERT INTO alerts_sent (kind, last_sent_at) VALUES (?, ?)
		ON CONFLICT (kind) DO UPDATE SET last_sent_at = excluded.last_sent_at`, kind, db.NowString())
	return err
}

// Phase is a step of the initial load.
type Phase struct {
	Name       string
	StartedAt  string
	FinishedAt string
}

func (db *DB) Phases(ctx context.Context) (map[string]Phase, error) {
	rows, err := db.W.QueryContext(ctx, "SELECT name, COALESCE(started_at, ''), COALESCE(finished_at, '') FROM phases")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Phase{}
	for rows.Next() {
		var p Phase
		if err := rows.Scan(&p.Name, &p.StartedAt, &p.FinishedAt); err != nil {
			return nil, err
		}
		out[p.Name] = p
	}
	return out, rows.Err()
}

// StartPhase records the start of a phase at the given time (keeps the first).
func (db *DB) StartPhase(ctx context.Context, name, at string) error {
	_, err := db.W.ExecContext(ctx, `INSERT INTO phases (name, started_at) VALUES (?, ?)
		ON CONFLICT (name) DO NOTHING`, name, at)
	return err
}

func (db *DB) FinishPhase(ctx context.Context, name string) error {
	_, err := db.W.ExecContext(ctx, "UPDATE phases SET finished_at = ? WHERE name = ? AND finished_at IS NULL", db.NowString(), name)
	return err
}

// RecordRequests adds request counters to the bucket of the given minute.
func (db *DB) RecordRequests(ctx context.Context, minute time.Time, requests, throttled, errs int64) error {
	_, err := db.W.ExecContext(ctx, `INSERT INTO request_stats (minute, requests, throttled, errors) VALUES (?, ?, ?, ?)
		ON CONFLICT (minute) DO UPDATE SET requests = requests + excluded.requests,
			throttled = throttled + excluded.throttled, errors = errors + excluded.errors`,
		FormatTime(minute.Truncate(time.Minute)), requests, throttled, errs)
	return err
}

// RequestTotals sums request counters since t.
func (db *DB) RequestTotals(ctx context.Context, since time.Time) (requests, throttled, errs int64, err error) {
	err = db.R.QueryRowContext(ctx, `SELECT COALESCE(SUM(requests), 0), COALESCE(SUM(throttled), 0), COALESCE(SUM(errors), 0)
		FROM request_stats WHERE minute >= ?`, FormatTime(since)).Scan(&requests, &throttled, &errs)
	return
}

// Count runs a single-value COUNT style query on the reader pool.
func (db *DB) Count(ctx context.Context, q string, args ...any) (int64, error) {
	var n int64
	err := db.R.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}
