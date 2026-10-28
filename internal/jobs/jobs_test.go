package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/joaoarapucas/speedscraper/internal/api"
	"github.com/joaoarapucas/speedscraper/internal/scheduler"
	"github.com/joaoarapucas/speedscraper/internal/store"
)

type env struct {
	fake *fakeAPI
	db   *store.DB
	q    *scheduler.Queue
	r    *Runner
}

func newEnv(t *testing.T, onlyGames ...string) *env {
	t.Helper()
	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	fake := newUniverse(20, base)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	client := api.New(api.Options{BaseURL: srv.URL, Limiter: api.NewLimiter(600000), MaxOffset: 20, BaseBackoff: time.Millisecond})
	cfg := DefaultConfig()
	cfg.DirtyDelay = 0
	cfg.RunsPageSize = 10
	cfg.OnlyGames = onlyGames
	q := scheduler.NewQueue(db)
	r := &Runner{API: client, DB: db, Q: q, Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return &env{fake: fake, db: db, q: q, r: r}
}

// drain runs every ready job synchronously, like the pool does.
func (e *env) drain(t *testing.T) map[string]int {
	t.Helper()
	ctx := context.Background()
	h := e.r.Handlers()
	ran := map[string]int{}
	for i := 0; i < 10000; i++ {
		j, err := e.q.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if j == nil {
			return ran
		}
		out, err := h[j.Kind](ctx, j)
		switch {
		case err == nil && out.Next.IsZero():
			err = e.q.Complete(ctx, j)
		case err == nil:
			err = e.q.Reschedule(ctx, j, out.NextPriority, out.Next)
		case errors.Is(err, scheduler.ErrGone):
			err = e.q.MarkGone(ctx, j, err)
		default:
			t.Fatalf("job %s %q failed: %v", j.Kind, j.Key, err)
		}
		if err != nil {
			t.Fatal(err)
		}
		ran[j.Kind]++
	}
	t.Fatal("queue never drained")
	return nil
}

func (e *env) count(t *testing.T, q string, args ...any) int64 {
	t.Helper()
	n, err := e.db.Count(context.Background(), q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestFullPipeline(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if err := e.r.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	ran := e.drain(t)
	for _, k := range []string{KindSeedActivity, KindDiscoverGames, KindDiscoverSeries, KindFeedVerified, KindFeedSubmitted} {
		if ran[k] != 1 {
			t.Errorf("%s ran %d times", k, ran[k])
		}
	}
	if ran[KindGameMeta] != 2 || ran[KindRunsCrawl] != 2 || ran[KindBoard] != 4 {
		t.Fatalf("ran %v", ran)
	}

	checks := map[string]int64{
		"SELECT COUNT(*) FROM games WHERE meta_fetched_at IS NOT NULL AND runs_crawled_at IS NOT NULL": 2,
		"SELECT COUNT(*) FROM runs":                                      int64(len(e.fake.runs)),
		"SELECT COUNT(*) FROM runs WHERE deleted_at IS NOT NULL":         0,
		"SELECT runs_truncated FROM games WHERE id = 'g1'":               1,
		"SELECT run_count FROM games WHERE id = 'g2'":                    5,
		"SELECT COUNT(*) FROM boards WHERE first_fetched_at IS NOT NULL": 4,
		"SELECT COUNT(*) FROM boards WHERE rank_mismatch > 0":            0,
		"SELECT COUNT(*) FROM series":                                    1,
		"SELECT COUNT(*) FROM users":                                     20,
		"SELECT COUNT(*) FROM game_activity":                             2,
	}
	for q, want := range checks {
		if got := e.count(t, q); got != want {
			t.Errorf("%s = %d, want %d", q, got, want)
		}
	}

	// Every board holds exactly what the API leaderboard holds.
	wantOpen := 0
	for _, b := range [][3]string{{"g1", "c1", ""}, {"g1", "c2", "l1"}, {"g1", "c2", "l2"}, {"g2", "d1", ""}} {
		wantOpen += len(e.fake.leaderboard(e.fake.game(b[0]), b[1], b[2])["runs"].([]any))
	}
	if got := e.count(t, "SELECT COUNT(*) FROM board_runs WHERE valid_to IS NULL"); got != int64(wantOpen) {
		t.Fatalf("open memberships %d, want %d", got, wantOpen)
	}
	checkSubcatRanking(t, e)

	// Steady state: a new world record arrives through the verified feed.
	rows := e.count(t, "SELECT COUNT(*) FROM board_runs")
	lbReq := e.fake.requestCount("leaderboards")
	now := time.Now().UTC().Truncate(time.Second)
	e.fake.addRun(fakeRun{ID: "wr", Game: "g1", Cat: "c1", Status: "verified", Player: "u01", SubVal: "a", Time: 99,
		Submitted: now, Verified: now})
	if err := e.q.Enqueue(ctx, KindFeedVerified, "", PrioFeed, now); err != nil {
		t.Fatal(err)
	}
	ran = e.drain(t)
	if ran[KindFeedVerified] != 1 || ran[KindBoard] != 1 {
		t.Fatalf("steady state ran %v", ran)
	}
	if got := e.fake.requestCount("leaderboards") - lbReq; got != 1 {
		t.Fatalf("only the changed board should be refetched, got %d leaderboard requests", got)
	}
	if got := e.count(t, "SELECT COUNT(*) FROM board_runs"); got != rows+1 {
		t.Fatalf("new PB must add exactly one membership row: %d -> %d", rows, got)
	}
	if got := e.count(t, "SELECT COUNT(*) FROM board_runs WHERE valid_to IS NOT NULL"); got != 1 {
		t.Fatalf("closed memberships %d, want 1 (the old PB)", got)
	}
	if got := e.count(t, "SELECT place FROM board_ranking_current WHERE run_id = 'wr' AND subcat_key = 'v1=a'"); got != 1 {
		t.Fatalf("new WR place %d", got)
	}
	checkSubcatRanking(t, e)

	// Refetching an unchanged board writes no membership rows.
	rows = e.count(t, "SELECT COUNT(*) FROM board_runs")
	if err := e.q.Enqueue(ctx, KindBoard, "g2|d1|", PrioDirtyBoard, now); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	if got := e.count(t, "SELECT COUNT(*) FROM board_runs"); got != rows {
		t.Fatalf("unchanged board wrote rows: %d -> %d", rows, got)
	}

	// A board whose category disappears is closed and its job retired.
	e.fake.mu.Lock()
	e.fake.games[1].Cats = nil
	e.fake.mu.Unlock()
	if err := e.q.Enqueue(ctx, KindBoard, "g2|d1|", PrioDirtyBoard, now); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	if got := e.count(t, "SELECT COUNT(*) FROM boards WHERE gone = 1"); got != 1 {
		t.Fatalf("gone boards %d", got)
	}
	if got := e.count(t, "SELECT COUNT(*) FROM jobs WHERE kind = 'board' AND key = 'g2|d1|' AND state = 'gone'"); got != 1 {
		t.Fatal("board job not retired")
	}
}

// checkSubcatRanking compares the derived per-subcategory places with the
// places a filtered leaderboard would show.
func checkSubcatRanking(t *testing.T, e *env) {
	t.Helper()
	g := e.fake.game("g1")
	lb := e.fake.leaderboard(g, "c1", "")
	type row struct {
		id string
		t  float64
	}
	bySub := map[string][]row{}
	for _, entry := range lb["runs"].([]any) {
		run := entry.(map[string]any)["run"].(map[string]any)
		sub := run["values"].(map[string]string)["v1"]
		if sub == "" {
			sub = "a"
		}
		bySub["v1="+sub] = append(bySub["v1="+sub], row{run["id"].(string), run["times"].(map[string]any)["primary_t"].(float64)})
	}
	for key, rows := range bySub {
		sort.Slice(rows, func(i, j int) bool { return rows[i].t < rows[j].t })
		for i, r := range rows {
			want := i + 1
			for want > 1 && rows[want-2].t == r.t {
				want--
			}
			got := e.count(t, "SELECT place FROM board_ranking_current WHERE run_id = ? AND subcat_key = ?", r.id, key)
			if got != int64(want) {
				t.Fatalf("run %s in %s: place %d, want %d", r.id, key, got, want)
			}
		}
	}
}

func TestRestrictedMode(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, "two")
	if err := e.r.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	ran := e.drain(t)
	if ran[KindFeedVerified] != 0 || ran[KindDiscoverGames] != 0 || ran[KindRunsCrawl] != 1 || ran[KindBoard] != 1 {
		t.Fatalf("ran %v", ran)
	}
	if got := e.count(t, "SELECT COUNT(*) FROM games"); got != 1 {
		t.Fatalf("games %d", got)
	}
	if got := e.count(t, "SELECT COUNT(*) FROM board_runs WHERE valid_to IS NULL"); got != 5 {
		t.Fatalf("open memberships %d", got)
	}
	// Bootstrap again (daemon restart) must not duplicate work.
	if err := e.r.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	if ran := e.drain(t); len(ran) != 0 {
		t.Fatalf("restart re-ran finished work: %v", ran)
	}
}

func TestCrawlFlagsDeletedRuns(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, "two")
	if err := e.r.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	e.fake.mu.Lock()
	e.fake.runs = e.fake.runs[:len(e.fake.runs)-1] // last g2 run deleted on the site
	e.fake.mu.Unlock()
	time.Sleep(1100 * time.Millisecond) // timestamps have second resolution
	if err := e.q.Enqueue(ctx, KindRunsCrawl, "g2", PrioRuns, time.Now()); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	if got := e.count(t, "SELECT COUNT(*) FROM runs WHERE game_id = 'g2' AND deleted_at IS NOT NULL"); got != 1 {
		t.Fatalf("deleted runs %d", got)
	}
	if got := e.count(t, "SELECT run_count FROM games WHERE id = 'g2'"); got != 4 {
		t.Fatalf("run_count %d", got)
	}
}

// TestSplitLeaderboard covers a leaderboard the API cannot serve whole. The
// g1/c1 board has 30 verified runs and the fake fails any leaderboard request
// covering more than 10, so it must be fetched in slices:
//   - subcategory b (10 runs) is served;
//   - subcategory a (20 runs) fails and is split by platform: p2 (4 runs) is
//     served, p1 (16 runs) fails and is split by emulator: emulated (4 runs)
//     is served, real hardware (12 runs) fails and is ranked from stored runs.
func TestSplitLeaderboard(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, "one")
	e.fake.lbLimit = 10
	for i := 0; i < 30; i++ { // the 30 verified g1/c1 runs, see newUniverse
		r := &e.fake.runs[i]
		if r.SubVal == "a" && i%5 == 0 {
			r.Platform = "p2"
		}
		r.Emulated = i%4 == 1
	}
	var warns []string
	e.r.Warn = func(_ context.Context, kind, _, _ string) { warns = append(warns, kind) }

	if err := e.r.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	if ran := e.drain(t); ran[KindBoard] != 3 {
		t.Fatalf("ran %v", ran)
	}
	if got := e.count(t, "SELECT COUNT(*) FROM boards WHERE game_id = 'g1' AND category_id = 'c1' AND split = 1 AND computed_slices = 1"); got != 1 {
		t.Fatal("c1 board not stored as split with one computed slice")
	}
	if got := e.count(t, "SELECT COUNT(*) FROM boards WHERE split = 1"); got != 1 {
		t.Fatalf("split boards %d, want only c1", got)
	}
	if len(warns) != 1 || warns[0] != "board_split:g1|c1|" {
		t.Fatalf("warnings %v", warns)
	}
	checkBoardMembers(t, e, "c1")
	checkSubcatRanking(t, e)
	if got := e.count(t, "SELECT COUNT(*) FROM board_ranking_current WHERE board_id = (SELECT id FROM boards WHERE category_id = 'c1')"); got == 0 {
		t.Fatal("no current ranking")
	}

	// A known split board goes straight to its slices: subcategories a and b,
	// platforms p1 and p2, emulator true and false. Nothing changed, so
	// nothing is written.
	rows := e.count(t, "SELECT COUNT(*) FROM board_runs")
	lbReq := e.fake.requestCount("leaderboards")
	now := time.Now().UTC().Truncate(time.Second)
	if err := e.q.Enqueue(ctx, KindBoard, "g1|c1|", PrioDirtyBoard, now); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	if got := e.fake.requestCount("leaderboards") - lbReq; got != 6 {
		t.Fatalf("refetch made %d leaderboard requests, want 6", got)
	}
	if got := e.count(t, "SELECT COUNT(*) FROM board_runs"); got != rows {
		t.Fatalf("unchanged split board wrote rows: %d -> %d", rows, got)
	}

	// A new record in the slice the API cannot serve reaches the board through
	// the stored runs.
	e.fake.addRun(fakeRun{ID: "wr", Game: "g1", Cat: "c1", Status: "verified", Player: "u01", SubVal: "a", Time: 1,
		Submitted: now, Verified: now})
	if err := e.q.Enqueue(ctx, KindRunsCrawl, "g1", PrioRuns, now); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	if err := e.q.Enqueue(ctx, KindBoard, "g1|c1|", PrioDirtyBoard, now); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	if got := e.count(t, "SELECT place FROM board_ranking_current WHERE run_id = 'wr' AND subcat_key = 'v1=a'"); got != 1 {
		t.Fatalf("new WR place %d", got)
	}
	checkBoardMembers(t, e, "c1")
	checkSubcatRanking(t, e)

	// If the API serves no slice at all the board is left untouched and the
	// job fails, to be retried.
	e.fake.lbLimit = 1
	rows = e.count(t, "SELECT COUNT(*) FROM board_runs")
	h := e.r.Handlers()[KindBoard]
	if _, err := h(ctx, &scheduler.Job{Kind: KindBoard, Key: "g1|c1|"}); !errors.Is(err, errNoSlice) {
		t.Fatalf("err = %v, want errNoSlice", err)
	}
	if got := e.count(t, "SELECT COUNT(*) FROM board_runs"); got != rows {
		t.Fatal("failed split fetch wrote rows")
	}
}

// checkBoardMembers compares the open memberships of a g1 board with the
// runs of the full API leaderboard.
func checkBoardMembers(t *testing.T, e *env, cat string) {
	t.Helper()
	var want []string
	for _, entry := range e.fake.leaderboard(e.fake.game("g1"), cat, "")["runs"].([]any) {
		want = append(want, entry.(map[string]any)["run"].(map[string]any)["id"].(string))
	}
	sort.Strings(want)
	rows, err := e.db.R.Query(`SELECT run_id FROM board_runs br JOIN boards b ON b.id = br.board_id
		WHERE b.game_id = 'g1' AND b.category_id = ? AND br.valid_to IS NULL ORDER BY run_id`, cat)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("board %s members\n got %v\nwant %v", cat, got, want)
	}
}

func TestRequeueFailed(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, "two")
	if err := e.q.Enqueue(ctx, KindBoard, "g2|d1|", PrioBoard, time.Now()); err != nil {
		t.Fatal(err)
	}
	j, err := e.q.Claim(ctx)
	if err != nil || j == nil {
		t.Fatal(j, err)
	}
	j.Attempts = 8
	if final, err := e.q.Retry(ctx, j, errors.New("boom"), 0, 8); err != nil || !final {
		t.Fatal(final, err)
	}
	if n, err := e.q.RequeueFailed(ctx, KindRunsCrawl); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if n, err := e.q.RequeueFailed(ctx, ""); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if got := e.count(t, "SELECT COUNT(*) FROM jobs WHERE state = 'scheduled' AND attempts = 0"); got != 1 {
		t.Fatal("job not requeued")
	}
}

func TestCandidateTieBreak(t *testing.T) {
	earlier := candidate{id: "z", t: 6, date: "2021-10-06"}
	later := candidate{id: "a", t: 6, date: "2021-10-10"}
	undated := candidate{id: "0", t: 6}
	faster := candidate{id: "y", t: 5, date: "2024-01-01"}
	cases := []struct {
		a, b candidate
		want bool
	}{
		{earlier, later, true}, {later, earlier, false},
		{later, undated, true}, {undated, later, false},
		{faster, earlier, true}, {earlier, faster, false},
		{candidate{id: "a", t: 6}, candidate{id: "b", t: 6}, true},
	}
	for i, c := range cases {
		if got := c.a.beats(c.b); got != c.want {
			t.Errorf("case %d: %s beats %s = %v", i, c.a.id, c.b.id, got)
		}
	}
}
