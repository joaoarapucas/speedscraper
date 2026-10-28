package monitor

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/joaoarapucas/speedscraper/internal/api"
	"github.com/joaoarapucas/speedscraper/internal/jobs"
	"github.com/joaoarapucas/speedscraper/internal/notify"
	"github.com/joaoarapucas/speedscraper/internal/scheduler"
	"github.com/joaoarapucas/speedscraper/internal/store"
)

type Config struct {
	Interval       time.Duration // how often checks run
	StallAfter     time.Duration // no finished job for this long is critical
	FeedStaleAfter time.Duration // feed job not run for this long is critical
	ThrottleWindow time.Duration
	ThrottleLimit  int64 // 420s within ThrottleWindow that trigger a warning
	ErrorWindow    time.Duration
	ErrorRate      float64 // failed request ratio within ErrorWindow that triggers a warning
	MinFreeBytes   uint64  // free disk space below this triggers a warning (0 disables)
	DigestHour     int     // local hour of the daily summary (-1 disables)
	FeedsEnabled   bool
}

func DefaultConfig() Config {
	return Config{
		Interval:       time.Minute,
		StallAfter:     30 * time.Minute,
		FeedStaleAfter: time.Hour,
		ThrottleWindow: 10 * time.Minute,
		ThrottleLimit:  20,
		ErrorWindow:    15 * time.Minute,
		ErrorRate:      0.10,
		MinFreeBytes:   5 << 30,
		DigestHour:     8,
		FeedsEnabled:   true,
	}
}

type Monitor struct {
	DB      *store.DB
	Q       *scheduler.Queue
	API     *api.Client
	Pool    *scheduler.Pool
	Alerter *notify.Alerter
	Log     *slog.Logger
	Cfg     Config

	last    api.Stats
	samples []sample
}

type sample struct {
	at time.Time
	api.Stats
}

// Run performs the checks every Interval until ctx is done, then flushes
// the request counters one last time.
func (m *Monitor) Run(ctx context.Context) {
	t := time.NewTicker(m.Cfg.Interval)
	defer t.Stop()
	m.Tick(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			m.flushStats(context.WithoutCancel(ctx), time.Now())
			return
		case now := <-t.C:
			m.Tick(ctx, now)
		}
	}
}

// Tick runs every check once.
func (m *Monitor) Tick(ctx context.Context, now time.Time) {
	m.flushStats(ctx, now)
	checks := []func(context.Context, time.Time) error{
		m.checkPhases, m.checkStall, m.checkFeed, m.checkAPI, m.checkDisk, m.checkDigest,
	}
	for _, c := range checks {
		if err := c(ctx, now); err != nil && ctx.Err() == nil {
			m.Log.Error("monitor check failed", "err", err)
			m.OnStoreError(ctx, err)
		}
	}
}

func (m *Monitor) flushStats(ctx context.Context, now time.Time) {
	cur := m.API.Stats()
	d := api.Stats{Requests: cur.Requests - m.last.Requests, Throttled: cur.Throttled - m.last.Throttled, Errors: cur.Errors - m.last.Errors}
	m.last = cur
	m.samples = append(m.samples, sample{now, cur})
	keep := max(m.Cfg.ThrottleWindow, m.Cfg.ErrorWindow) + m.Cfg.Interval
	for len(m.samples) > 1 && now.Sub(m.samples[0].at) > keep {
		m.samples = m.samples[1:]
	}
	if d.Requests == 0 && d.Throttled == 0 && d.Errors == 0 {
		return
	}
	if err := m.DB.RecordRequests(ctx, now, d.Requests, d.Throttled, d.Errors); err != nil {
		m.Log.Error("recording request stats failed", "err", err)
	}
}

// delta returns the counter growth over the window ending now.
func (m *Monitor) delta(now time.Time, window time.Duration) api.Stats {
	if len(m.samples) == 0 {
		return api.Stats{}
	}
	cur := m.samples[len(m.samples)-1]
	base := m.samples[0]
	for _, s := range m.samples {
		if now.Sub(s.at) <= window {
			break
		}
		base = s
	}
	return api.Stats{Requests: cur.Requests - base.Requests, Throttled: cur.Throttled - base.Throttled, Errors: cur.Errors - base.Errors}
}

func (m *Monitor) checkPhases(ctx context.Context, now time.Time) error {
	listedAt, _, err := m.DB.Checkpoint(ctx, jobs.CheckpointGamesListed)
	if err != nil || listedAt == "" {
		return err
	}
	state, err := m.DB.Phases(ctx)
	if err != nil {
		return err
	}
	for i, p := range Phases {
		st := state[p.Name]
		if st.FinishedAt != "" {
			continue
		}
		if st.StartedAt == "" {
			// The first phase starts with the first game listing; the
			// others start when the previous one finishes.
			st.StartedAt = store.FormatTime(now)
			if i == 0 {
				st.StartedAt = listedAt
			}
			if err := m.DB.StartPhase(ctx, p.Name, st.StartedAt); err != nil {
				return err
			}
			m.Log.Info("phase started", "phase", p.Name)
		}
		remaining, err := m.DB.Count(ctx, p.Remaining)
		if err != nil {
			return err
		}
		if remaining > 0 {
			return nil // later phases wait for this one
		}
		if err := m.DB.FinishPhase(ctx, p.Name); err != nil {
			return err
		}
		report, err := Report(ctx, m.DB, m.Q)
		if err != nil {
			report = "relatorio indisponivel: " + err.Error()
		}
		subject := fmt.Sprintf("Fase %s concluida", p.Name)
		body := fmt.Sprintf("A fase %q (%s) terminou.\nInicio: %s\nFim: %s\n", p.Name, p.Title, st.StartedAt, store.FormatTime(now))
		if i+1 < len(Phases) {
			next := Phases[i+1]
			if err := m.DB.StartPhase(ctx, next.Name, store.FormatTime(now)); err != nil {
				return err
			}
			subject += fmt.Sprintf(", iniciando %s", next.Name)
			body += fmt.Sprintf("Proxima fase: %q (%s).\n", next.Name, next.Title)
		} else {
			subject = "Carga inicial completa: regime permanente"
			body += "Todas as fases terminaram. A partir de agora so o feed, as boards alteradas e a reconciliacao periodica consomem a cota.\n"
		}
		m.Log.Info("phase finished", "phase", p.Name)
		m.Alerter.Alert(ctx, "phase:"+p.Name, notify.Info, subject, body+"\n"+report)
	}
	return nil
}

func (m *Monitor) checkStall(ctx context.Context, now time.Time) error {
	last := m.Pool.LastProgress()
	if last.IsZero() {
		return nil
	}
	idle := now.Sub(last)
	if idle < m.Cfg.StallAfter {
		return nil
	}
	m.Alerter.Alert(ctx, "stall", notify.Critical, "Scraper sem progresso",
		fmt.Sprintf("Nenhum job foi concluido nos ultimos %s.\nVerifique rede, a API (status 420/5xx) e o journal: journalctl -u speedscraper -n 200", idle.Round(time.Minute)))
	return nil
}

func (m *Monitor) checkFeed(ctx context.Context, now time.Time) error {
	if !m.Cfg.FeedsEnabled {
		return nil
	}
	var updated, state string
	err := m.DB.R.QueryRowContext(ctx, "SELECT updated_at, state FROM jobs WHERE kind = ? AND key = ''", jobs.KindFeedVerified).Scan(&updated, &state)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	t, err := time.Parse(store.TimeFormat, updated)
	if err != nil {
		return err
	}
	if state == scheduler.Failed || now.Sub(t) > m.Cfg.FeedStaleAfter {
		m.Alerter.Alert(ctx, "feed_stale", notify.Critical, "Feed de runs verificadas parado",
			fmt.Sprintf("O job %s nao roda desde %s (estado: %s). Leaderboards alteradas nao estao sendo detectadas.", jobs.KindFeedVerified, updated, state))
	}
	return nil
}

func (m *Monitor) checkAPI(ctx context.Context, now time.Time) error {
	if d := m.delta(now, m.Cfg.ThrottleWindow); d.Throttled >= m.Cfg.ThrottleLimit {
		m.Alerter.Alert(ctx, "throttled", notify.Warning, "Muitas respostas 420 da API",
			fmt.Sprintf("%d respostas 420 nos ultimos %s. O scraper pausa sozinho, mas considere reduzir --rpm.", d.Throttled, m.Cfg.ThrottleWindow))
	}
	if d := m.delta(now, m.Cfg.ErrorWindow); d.Requests >= 30 && float64(d.Errors)/float64(d.Requests) > m.Cfg.ErrorRate {
		m.Alerter.Alert(ctx, "api_errors", notify.Warning, "Taxa de erro alta na API",
			fmt.Sprintf("%d de %d requisicoes falharam nos ultimos %s.", d.Errors, d.Requests, m.Cfg.ErrorWindow))
	}
	return nil
}

func (m *Monitor) checkDisk(ctx context.Context, now time.Time) error {
	if m.Cfg.MinFreeBytes == 0 {
		return nil
	}
	free, err := freeBytes(filepath.Dir(m.DB.Path))
	if err != nil {
		return err
	}
	if free < m.Cfg.MinFreeBytes {
		m.Alerter.Alert(ctx, "disk", notify.Warning, "Pouco espaco em disco",
			fmt.Sprintf("Restam %.1f GiB livres em %s.", float64(free)/(1<<30), filepath.Dir(m.DB.Path)))
	}
	return nil
}

func (m *Monitor) checkDigest(ctx context.Context, now time.Time) error {
	if m.Cfg.DigestHour < 0 || now.Local().Hour() != m.Cfg.DigestHour {
		return nil
	}
	last, err := m.DB.AlertLastSent(ctx, "digest")
	if err != nil {
		return err
	}
	if !last.IsZero() && now.Sub(last) < 20*time.Hour {
		return nil
	}
	report, err := Report(ctx, m.DB, m.Q)
	if err != nil {
		return err
	}
	m.Alerter.Alert(ctx, "digest", notify.Info, "Resumo diario", report)
	return nil
}

// OnJobError is wired to the pool: storage errors are critical, jobs that
// failed for good are reported (deduplicated per kind).
func (m *Monitor) OnJobError(j *scheduler.Job, err error, final bool) {
	ctx := context.Background()
	if isStorageError(err) {
		m.OnStoreError(ctx, err)
		return
	}
	if final {
		m.Alerter.Alert(ctx, "job_failed:"+j.Kind, notify.Warning, "Job falhou definitivamente: "+j.Kind,
			fmt.Sprintf("kind=%s key=%q tentativas=%d\nerro: %v\n\nOutros jobs do mesmo tipo que falharem na proxima hora nao geram novo e-mail; veja 'speedscraper status'.",
				j.Kind, j.Key, j.Attempts, err))
	}
}

func (m *Monitor) OnStoreError(ctx context.Context, err error) {
	if !isStorageError(err) {
		return
	}
	m.Alerter.Alert(ctx, "storage", notify.Critical, "Erro no banco SQLite",
		fmt.Sprintf("O banco %s retornou um erro grave:\n%v\n\nVerifique espaco em disco e integridade (sqlite3 %s 'PRAGMA integrity_check').", m.DB.Path, err, m.DB.Path))
}

func isStorageError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, marker := range []string{"database or disk is full", "sqlite_full", "disk i/o error", "malformed", "sqlite_corrupt",
		"not a database", "readonly database", "no space left", "sqlite_ioerr", "sqlite_cantopen"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}
