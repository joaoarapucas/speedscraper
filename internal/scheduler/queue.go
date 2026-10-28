// Package scheduler implements a persistent priority job queue on SQLite and
// the worker pool that drains it.
package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/joaoarapucas/speedscraper/internal/store"
)

// Job states. Scheduled jobs wait for not_before and are promoted to pending
// when due, so every pending job is ready to run.
const (
	Scheduled = "scheduled"
	Pending   = "pending"
	Running   = "running"
	Done      = "done"
	Failed    = "failed"
	Gone      = "gone"
)

type Job struct {
	ID       int64
	Kind     string
	Key      string
	Priority float64
	Attempts int
}

type Queue struct {
	db *store.DB
}

func NewQueue(db *store.DB) *Queue { return &Queue{db: db} }

// Enqueue schedules (kind, key). A waiting job keeps the higher priority and
// the earlier start; a running job is flagged to run again once it finishes;
// a finished job is re-armed.
func (q *Queue) Enqueue(ctx context.Context, kind, key string, priority float64, notBefore time.Time) error {
	now := q.db.NowString()
	_, err := q.db.W.ExecContext(ctx, `INSERT INTO jobs (kind, key, priority, not_before, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'scheduled', ?, ?)
		ON CONFLICT (kind, key) DO UPDATE SET
			priority   = CASE WHEN jobs.state IN ('scheduled', 'pending') THEN MAX(jobs.priority, excluded.priority) ELSE excluded.priority END,
			not_before = CASE WHEN jobs.state IN ('scheduled', 'pending') THEN MIN(jobs.not_before, excluded.not_before) ELSE excluded.not_before END,
			rerun      = CASE WHEN jobs.state = 'running' THEN 1 ELSE jobs.rerun END,
			attempts   = CASE WHEN jobs.state IN ('done', 'failed', 'gone') THEN 0 ELSE jobs.attempts END,
			state      = CASE WHEN jobs.state = 'running' THEN 'running' ELSE 'scheduled' END,
			updated_at = excluded.updated_at`,
		kind, key, priority, store.FormatTime(notBefore), now, now)
	return err
}

// EnqueueOnce adds (kind, key) only if it was never queued before.
func (q *Queue) EnqueueOnce(ctx context.Context, kind, key string, priority float64, notBefore time.Time) error {
	now := q.db.NowString()
	_, err := q.db.W.ExecContext(ctx, `INSERT OR IGNORE INTO jobs (kind, key, priority, not_before, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'scheduled', ?, ?)`, kind, key, priority, store.FormatTime(notBefore), now, now)
	return err
}

// Claim takes the highest-priority ready job and marks it running. It
// returns nil when nothing is ready.
func (q *Queue) Claim(ctx context.Context) (*Job, error) {
	var j Job
	err := q.db.Tx(ctx, func(tx *sql.Tx) error {
		now := q.db.NowString()
		if _, err := tx.ExecContext(ctx, "UPDATE jobs SET state = 'pending' WHERE state = 'scheduled' AND not_before <= ?", now); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT id, kind, key, priority, attempts FROM jobs
			WHERE state = 'pending' ORDER BY priority DESC, id LIMIT 1`).
			Scan(&j.ID, &j.Kind, &j.Key, &j.Priority, &j.Attempts)
		if err != nil {
			return err
		}
		j.Attempts++
		_, err = tx.ExecContext(ctx, "UPDATE jobs SET state = 'running', attempts = ?, rerun = 0, updated_at = ? WHERE id = ?",
			j.Attempts, now, j.ID)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// Complete finishes a job. A job flagged for rerun while it ran is scheduled
// again immediately.
func (q *Queue) Complete(ctx context.Context, j *Job) error {
	now := q.db.NowString()
	_, err := q.db.W.ExecContext(ctx, `UPDATE jobs SET
			state = CASE WHEN rerun = 1 THEN 'scheduled' ELSE 'done' END,
			not_before = CASE WHEN rerun = 1 THEN ? ELSE not_before END,
			rerun = 0, attempts = 0, last_error = NULL, updated_at = ?
		WHERE id = ?`, now, now, j.ID)
	return err
}

// Reschedule finishes a recurring job and arms it again at the given time
// (or right away if it was re-enqueued while running).
func (q *Queue) Reschedule(ctx context.Context, j *Job, priority float64, at time.Time) error {
	now := q.db.NowString()
	_, err := q.db.W.ExecContext(ctx, `UPDATE jobs SET state = 'scheduled',
			not_before = CASE WHEN rerun = 1 THEN ? ELSE ? END,
			priority = CASE WHEN rerun = 1 THEN MAX(priority, ?) ELSE ? END,
			rerun = 0, attempts = 0, last_error = NULL, updated_at = ?
		WHERE id = ?`, now, store.FormatTime(at), priority, priority, now, j.ID)
	return err
}

// Retry puts a failed attempt back in the queue after delay, or marks it
// failed for good once maxAttempts is reached.
func (q *Queue) Retry(ctx context.Context, j *Job, cause error, delay time.Duration, maxAttempts int) (final bool, err error) {
	final = j.Attempts >= maxAttempts
	state := Scheduled
	if final {
		state = Failed
	}
	_, err = q.db.W.ExecContext(ctx, `UPDATE jobs SET state = ?, not_before = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		state, store.FormatTime(q.db.Now().Add(delay)), cause.Error(), q.db.NowString(), j.ID)
	return final, err
}

// Release returns a job interrupted by shutdown without counting the attempt.
func (q *Queue) Release(ctx context.Context, j *Job) error {
	now := q.db.NowString()
	_, err := q.db.W.ExecContext(ctx, `UPDATE jobs SET state = 'scheduled', not_before = ?, attempts = MAX(attempts - 1, 0),
		updated_at = ? WHERE id = ?`, now, now, j.ID)
	return err
}

// MarkGone ends a job whose target no longer exists.
func (q *Queue) MarkGone(ctx context.Context, j *Job, cause error) error {
	_, err := q.db.W.ExecContext(ctx, `UPDATE jobs SET state = 'gone', rerun = 0, last_error = ?, updated_at = ? WHERE id = ?`,
		cause.Error(), q.db.NowString(), j.ID)
	return err
}

// ResetRunning returns jobs left running by a crash to the queue.
func (q *Queue) ResetRunning(ctx context.Context) (int64, error) {
	now := q.db.NowString()
	res, err := q.db.W.ExecContext(ctx, "UPDATE jobs SET state = 'scheduled', not_before = ?, updated_at = ? WHERE state = 'running'", now, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RequeueFailed gives jobs that failed for good a fresh set of attempts.
// An empty kind requeues every kind.
func (q *Queue) RequeueFailed(ctx context.Context, kind string) (int64, error) {
	now := q.db.NowString()
	res, err := q.db.W.ExecContext(ctx, `UPDATE jobs SET state = 'scheduled', not_before = ?, attempts = 0, updated_at = ?
		WHERE state = 'failed' AND (? = '' OR kind = ?)`, now, now, kind, kind)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// NextReady returns when the earliest waiting job becomes ready (zero if none).
func (q *Queue) NextReady(ctx context.Context) (time.Time, error) {
	var pending int
	if err := q.db.W.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM jobs WHERE state = 'pending')").Scan(&pending); err != nil {
		return time.Time{}, err
	}
	if pending == 1 {
		return q.db.Now(), nil
	}
	var s sql.NullString
	if err := q.db.W.QueryRowContext(ctx, "SELECT MIN(not_before) FROM jobs WHERE state = 'scheduled'").Scan(&s); err != nil {
		return time.Time{}, err
	}
	if !s.Valid {
		return time.Time{}, nil
	}
	return time.Parse(store.TimeFormat, s.String)
}

// Counts returns the number of jobs per kind and state.
func (q *Queue) Counts(ctx context.Context) (map[string]map[string]int64, error) {
	rows, err := q.db.R.QueryContext(ctx, "SELECT kind, state, COUNT(*) FROM jobs GROUP BY kind, state")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int64{}
	for rows.Next() {
		var kind, state string
		var n int64
		if err := rows.Scan(&kind, &state, &n); err != nil {
			return nil, err
		}
		if out[kind] == nil {
			out[kind] = map[string]int64{}
		}
		out[kind][state] = n
	}
	return out, rows.Err()
}
