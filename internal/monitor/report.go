// Package monitor tracks scraping progress, detects problems and produces
// the status report used by the CLI and the daily digest.
package monitor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/joaoarapucas/speedscraper/internal/scheduler"
	"github.com/joaoarapucas/speedscraper/internal/store"
)

// Phase describes one step of the initial load.
type Phase struct {
	Name  string
	Title string
	// Remaining counts the items still to process. Items whose job failed
	// for good or whose target is gone do not block the phase.
	Remaining string
}

var Phases = []Phase{
	{"1-metadados", "metadados dos jogos", `SELECT COUNT(*) FROM games g WHERE g.gone = 0 AND g.meta_fetched_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.kind = 'game_meta' AND j.key = g.id AND j.state IN ('failed', 'gone'))`},
	{"2-runs", "runs de todos os jogos", `SELECT COUNT(*) FROM games g WHERE g.gone = 0 AND g.runs_crawled_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.kind IN ('game_meta', 'runs_crawl') AND j.key = g.id AND j.state IN ('failed', 'gone'))`},
	{"3-leaderboards", "leaderboards oficiais", `SELECT COUNT(*) FROM boards b WHERE b.gone = 0 AND b.first_fetched_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.kind = 'board' AND j.key = b.game_id || '|' || b.category_id || '|' || b.level_id
			AND j.state IN ('failed', 'gone'))`},
}

// Report renders a human readable progress report.
func Report(ctx context.Context, db *store.DB, q *scheduler.Queue) (string, error) {
	var b strings.Builder
	now := db.Now()
	fmt.Fprintf(&b, "speedscraper - %s\n", now.Local().Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(&b, "banco: %s\n\n", db.Path)

	phases, err := db.Phases(ctx)
	if err != nil {
		return "", err
	}
	b.WriteString("Fases da carga inicial\n")
	for _, p := range Phases {
		rem, err := db.Count(ctx, p.Remaining)
		if err != nil {
			return "", err
		}
		st := phases[p.Name]
		status := "aguardando"
		switch {
		case st.FinishedAt != "":
			status = "concluida em " + st.FinishedAt
		case st.StartedAt != "":
			status = fmt.Sprintf("em andamento desde %s, faltam %d", st.StartedAt, rem)
		}
		fmt.Fprintf(&b, "  %-16s %-26s %s\n", p.Name, p.Title, status)
	}

	b.WriteString("\nDados\n")
	counts := []struct{ label, q string }{
		{"jogos", "SELECT COUNT(*) FROM games WHERE gone = 0"},
		{"jogos com metadados", "SELECT COUNT(*) FROM games WHERE gone = 0 AND meta_fetched_at IS NOT NULL"},
		{"jogos com runs", "SELECT COUNT(*) FROM games WHERE gone = 0 AND runs_crawled_at IS NOT NULL"},
		{"jogos truncados (>cap)", "SELECT COUNT(*) FROM games WHERE runs_truncated = 1"},
		{"categorias", "SELECT COUNT(*) FROM categories WHERE removed_at IS NULL"},
		{"runs", "SELECT COUNT(*) FROM runs WHERE deleted_at IS NULL"},
		{"  verificadas", "SELECT COUNT(*) FROM runs WHERE deleted_at IS NULL AND status = 'verified'"},
		{"  pendentes", "SELECT COUNT(*) FROM runs WHERE deleted_at IS NULL AND status = 'new'"},
		{"  rejeitadas", "SELECT COUNT(*) FROM runs WHERE deleted_at IS NULL AND status = 'rejected'"},
		{"usuarios", "SELECT COUNT(*) FROM users"},
		{"boards", "SELECT COUNT(*) FROM boards WHERE gone = 0"},
		{"boards ja buscadas", "SELECT COUNT(*) FROM boards WHERE gone = 0 AND first_fetched_at IS NOT NULL"},
		{"entradas abertas", "SELECT COUNT(*) FROM board_runs WHERE valid_to IS NULL"},
		{"entradas historicas", "SELECT COUNT(*) FROM board_runs WHERE valid_to IS NOT NULL"},
		{"boards com place divergente", "SELECT COUNT(*) FROM boards WHERE rank_mismatch > 0"},
	}
	for _, c := range counts {
		n, err := db.Count(ctx, c.q)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "  %-28s %d\n", c.label, n)
	}

	b.WriteString("\nRequisicoes\n")
	var perMin float64
	for _, w := range []struct {
		label string
		d     time.Duration
	}{{"ultimos 15min", 15 * time.Minute}, {"ultima hora", time.Hour}, {"ultimas 24h", 24 * time.Hour}} {
		req, thr, errs, err := db.RequestTotals(ctx, now.Add(-w.d))
		if err != nil {
			return "", err
		}
		if perMin == 0 {
			perMin = float64(req) / w.d.Minutes() // the projection uses the recent pace
		}
		fmt.Fprintf(&b, "  %-14s %d req (%.1f/min), %d com 420, %d erros\n", w.label, req, float64(req)/w.d.Minutes(), thr, errs)
	}

	jobs, err := q.Counts(ctx)
	if err != nil {
		return "", err
	}
	b.WriteString("\nFila de jobs (por estado)\n")
	kinds := make([]string, 0, len(jobs))
	for k := range jobs {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		var parts []string
		for _, s := range []string{scheduler.Pending, scheduler.Scheduled, scheduler.Running, scheduler.Done, scheduler.Failed, scheduler.Gone} {
			if n := jobs[k][s]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s=%d", s, n))
			}
		}
		fmt.Fprintf(&b, "  %-16s %s\n", k, strings.Join(parts, " "))
	}

	if eta := projection(jobs, perMin); eta != "" {
		b.WriteString("\n" + eta + "\n")
	}
	return b.String(), nil
}

// projection estimates how long the queued initial-load work takes at the
// request rate of the last hour. Each meta and board job is one request;
// run crawls are counted as one request (most games fit in one page).
func projection(jobs map[string]map[string]int64, perMin float64) string {
	if perMin < 1 {
		return ""
	}
	ready := func(kind string) int64 { return jobs[kind][scheduler.Pending] }
	work := ready("game_meta") + ready("runs_crawl") + ready("board")
	if work == 0 {
		return ""
	}
	d := time.Duration(float64(work)/perMin) * time.Minute
	return fmt.Sprintf("Projecao: %d jobs prontos da carga inicial, ~%s no ritmo atual", work, d.Round(time.Minute))
}
