package monitor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joaoarapucas/speedscraper/internal/api"
	"github.com/joaoarapucas/speedscraper/internal/jobs"
	"github.com/joaoarapucas/speedscraper/internal/notify"
	"github.com/joaoarapucas/speedscraper/internal/scheduler"
	"github.com/joaoarapucas/speedscraper/internal/store"
)

type outbox struct {
	mu       sync.Mutex
	subjects []string
}

func (o *outbox) Send(_ context.Context, subject, _ string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.subjects = append(o.subjects, subject)
	return nil
}

func (o *outbox) list() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.subjects...)
}

func newMonitor(t *testing.T) (*Monitor, *outbox) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	out := &outbox{}
	cfg := DefaultConfig()
	cfg.DigestHour = -1
	cfg.MinFreeBytes = 0
	q := scheduler.NewQueue(db)
	m := &Monitor{DB: db, Q: q, API: api.New(api.Options{}), Pool: &scheduler.Pool{Queue: q}, Log: log, Cfg: cfg,
		Alerter: &notify.Alerter{N: out, Store: db, Log: log, Cooldown: time.Hour, Prefix: "[t]", Retries: 1}}
	return m, out
}

func exec(t *testing.T, db *store.DB, q string) {
	t.Helper()
	if _, err := db.W.Exec(q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func TestPhaseTransitionsNotifyOnce(t *testing.T) {
	ctx := context.Background()
	m, out := newMonitor(t)
	db := m.DB
	exec(t, db, "INSERT INTO games (id, name, discovered_at) VALUES ('g1', 'G', '2026-01-01T00:00:00Z'), ('g2', 'H', '2026-01-01T00:00:00Z')")
	tick := func() { m.Tick(ctx, time.Now()); m.Alerter.Wait() }

	tick() // nothing listed yet: no phase starts
	if p, _ := db.Phases(ctx); len(p) != 0 {
		t.Fatalf("phases started before the game listing: %v", p)
	}
	if err := db.SetCheckpoint(ctx, jobs.CheckpointGamesListed, "x"); err != nil {
		t.Fatal(err)
	}
	tick()
	if len(out.list()) != 0 {
		t.Fatalf("unexpected alerts %v", out.list())
	}

	exec(t, db, "UPDATE games SET meta_fetched_at = '2026-01-01T00:00:00Z'")
	tick()
	tick()
	if s := out.list(); len(s) != 1 || !strings.Contains(s[0], "Fase 1-metadados concluida, iniciando 2-runs") {
		t.Fatalf("alerts %v", s)
	}

	// A game whose crawl failed for good does not block the phase.
	exec(t, db, "UPDATE games SET runs_crawled_at = '2026-01-01T00:00:00Z' WHERE id = 'g1'")
	exec(t, db, "INSERT INTO jobs (kind, key, priority, not_before, state, created_at, updated_at) VALUES ('runs_crawl', 'g2', 1, 'x', 'failed', 'x', 'x')")
	exec(t, db, "INSERT INTO boards (game_id, category_id, level_id, first_fetched_at) VALUES ('g1', 'c1', '', '2026-01-01T00:00:00Z')")
	tick()
	s := out.list()
	if len(s) != 3 || !strings.Contains(s[1], "2-runs concluida") || !strings.Contains(s[2], "Carga inicial completa") {
		t.Fatalf("alerts %v", s)
	}
	tick()
	if len(out.list()) != 3 {
		t.Fatal("finished phases must not notify again")
	}
}

func TestStorageErrorsAndFailedJobs(t *testing.T) {
	m, out := newMonitor(t)
	m.OnJobError(&scheduler.Job{Kind: "board", Key: "k"}, errors.New("database or disk is full (13)"), false)
	m.OnJobError(&scheduler.Job{Kind: "board", Key: "k"}, errors.New("timeout"), false) // transient: silent
	m.OnJobError(&scheduler.Job{Kind: "board", Key: "k"}, errors.New("timeout"), true)
	m.OnJobError(&scheduler.Job{Kind: "board", Key: "k2"}, errors.New("timeout"), true) // cooldown
	m.Alerter.Wait()
	s := out.list()
	if len(s) != 2 || !strings.Contains(strings.Join(s, "|"), "Erro no banco SQLite") || !strings.Contains(strings.Join(s, "|"), "Job falhou definitivamente: board") {
		t.Fatalf("alerts %v", s)
	}
}

func TestReportRenders(t *testing.T) {
	m, _ := newMonitor(t)
	r, err := Report(context.Background(), m.DB, m.Q)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Fases da carga inicial", "1-metadados", "Requisicoes", "Fila de jobs"} {
		if !strings.Contains(r, want) {
			t.Errorf("report lacks %q:\n%s", want, r)
		}
	}
}
