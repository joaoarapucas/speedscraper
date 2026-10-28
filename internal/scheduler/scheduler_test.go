package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/joaoarapucas/speedscraper/internal/store"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newQueue(t *testing.T) (*Queue, *store.DB, *clock) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	db.Now = c.now
	return NewQueue(db), db, c
}

func state(t *testing.T, db *store.DB, kind, key string) string {
	t.Helper()
	var s string
	if err := db.W.QueryRow("SELECT state FROM jobs WHERE kind = ? AND key = ?", kind, key).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestClaimOrderAndReadiness(t *testing.T) {
	ctx := context.Background()
	q, _, c := newQueue(t)
	now := c.now()
	must(t, q.Enqueue(ctx, "k", "low", 1, now))
	must(t, q.Enqueue(ctx, "k", "high", 10, now))
	must(t, q.Enqueue(ctx, "k", "future", 100, now.Add(time.Hour)))

	for _, want := range []string{"high", "low"} {
		j, err := q.Claim(ctx)
		if err != nil || j == nil || j.Key != want {
			t.Fatalf("want %s, got %+v %v", want, j, err)
		}
	}
	if j, _ := q.Claim(ctx); j != nil {
		t.Fatalf("future job claimed early: %+v", j)
	}
	next, _ := q.NextReady(ctx)
	if !next.Equal(now.Add(time.Hour)) {
		t.Fatalf("NextReady = %v", next)
	}
	c.add(time.Hour)
	if j, _ := q.Claim(ctx); j == nil || j.Key != "future" {
		t.Fatalf("future job not claimed when due: %+v", j)
	}
}

func TestEnqueueMergesAndReruns(t *testing.T) {
	ctx := context.Background()
	q, db, c := newQueue(t)
	now := c.now()
	must(t, q.Enqueue(ctx, "k", "a", 1, now.Add(time.Hour)))
	must(t, q.Enqueue(ctx, "k", "a", 5, now.Add(2*time.Hour)))
	var prio float64
	var nb string
	must(t, db.W.QueryRow("SELECT priority, not_before FROM jobs WHERE key='a'").Scan(&prio, &nb))
	if prio != 5 || nb != store.FormatTime(now.Add(time.Hour)) {
		t.Fatalf("merge: prio=%v not_before=%s", prio, nb)
	}

	must(t, q.EnqueueOnce(ctx, "k", "b", 1, now))
	j, _ := q.Claim(ctx)
	if j == nil || j.Key != "b" {
		t.Fatalf("got %+v", j)
	}
	// Enqueued again while running: must run once more after completion.
	must(t, q.Enqueue(ctx, "k", "b", 1, now))
	must(t, q.Complete(ctx, j))
	if s := state(t, db, "k", "b"); s != Scheduled {
		t.Fatalf("rerun not honored, state %s", s)
	}
	j, _ = q.Claim(ctx)
	must(t, q.Complete(ctx, j))
	if s := state(t, db, "k", "b"); s != Done {
		t.Fatalf("state %s", s)
	}
	// EnqueueOnce never revives finished jobs; Enqueue does.
	must(t, q.EnqueueOnce(ctx, "k", "b", 1, now))
	if s := state(t, db, "k", "b"); s != Done {
		t.Fatalf("EnqueueOnce revived job: %s", s)
	}
	must(t, q.Enqueue(ctx, "k", "b", 1, now))
	if s := state(t, db, "k", "b"); s != Scheduled {
		t.Fatalf("Enqueue did not re-arm: %s", s)
	}
}

func TestRetryAndResetRunning(t *testing.T) {
	ctx := context.Background()
	q, db, c := newQueue(t)
	must(t, q.Enqueue(ctx, "k", "x", 1, c.now()))
	j, _ := q.Claim(ctx)
	final, err := q.Retry(ctx, j, errors.New("boom"), time.Minute, 2)
	if err != nil || final {
		t.Fatalf("final=%v err=%v", final, err)
	}
	c.add(time.Minute)
	j, _ = q.Claim(ctx)
	if j == nil || j.Attempts != 2 {
		t.Fatalf("got %+v", j)
	}
	if final, _ := q.Retry(ctx, j, errors.New("boom"), time.Minute, 2); !final {
		t.Fatal("should be final at max attempts")
	}
	if s := state(t, db, "k", "x"); s != Failed {
		t.Fatalf("state %s", s)
	}

	must(t, q.Enqueue(ctx, "k", "y", 1, c.now()))
	if j, _ := q.Claim(ctx); j == nil {
		t.Fatal("no job")
	}
	if n, err := q.ResetRunning(ctx); err != nil || n != 1 {
		t.Fatalf("reset %d %v", n, err)
	}
	if j, _ := q.Claim(ctx); j == nil || j.Key != "y" {
		t.Fatal("crashed job not recovered")
	}
}

func TestPoolRunsRescheduleGoneAndShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	q, db, c := newQueue(t)
	db.Now = time.Now
	_ = c
	now := time.Now()
	must(t, q.Enqueue(ctx, "ok", "1", 1, now))
	must(t, q.Enqueue(ctx, "recurring", "1", 1, now))
	must(t, q.Enqueue(ctx, "gone", "1", 1, now))
	must(t, q.Enqueue(ctx, "panics", "1", 1, now))

	var mu sync.Mutex
	ran := map[string]int{}
	var errs int
	p := &Pool{
		Queue:   q,
		Workers: 3,
		Grace:   time.Second,
		Handlers: map[string]Handler{
			"ok": func(ctx context.Context, j *Job) (Outcome, error) {
				mu.Lock()
				ran["ok"]++
				mu.Unlock()
				return Outcome{}, nil
			},
			"recurring": func(ctx context.Context, j *Job) (Outcome, error) {
				mu.Lock()
				ran["recurring"]++
				mu.Unlock()
				return Outcome{Next: time.Now().Add(time.Hour), NextPriority: 7}, nil
			},
			"gone": func(ctx context.Context, j *Job) (Outcome, error) {
				return Outcome{}, ErrGone
			},
			"panics": func(ctx context.Context, j *Job) (Outcome, error) {
				panic("kaboom")
			},
		},
		OnError: func(j *Job, err error, final bool) { mu.Lock(); errs++; mu.Unlock() },
	}
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if state(t, db, "ok", "1") == Done && state(t, db, "gone", "1") == Gone &&
			state(t, db, "recurring", "1") == Scheduled && state(t, db, "panics", "1") == Scheduled {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pool did not stop")
	}
	mu.Lock()
	defer mu.Unlock()
	if ran["ok"] != 1 || ran["recurring"] != 1 || errs != 1 {
		t.Fatalf("ran=%v errs=%d", ran, errs)
	}
	var prio float64
	must(t, db.W.QueryRow("SELECT priority FROM jobs WHERE kind='recurring'").Scan(&prio))
	if prio != 7 {
		t.Fatalf("reschedule priority %v", prio)
	}
	if p.Completed() != 3 { // ok, recurring and gone
		t.Fatalf("completed %d", p.Completed())
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
