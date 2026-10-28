package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/joaoarapucas/speedscraper/internal/api"
	"github.com/joaoarapucas/speedscraper/internal/board"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustJSON[T any](t *testing.T, src string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(src), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		db, err := Open(context.Background(), filepath.Join(dir, "x.db"))
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		db.Close()
	}
}

func TestSaveGameAndRemovedFlags(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	g := mustJSON[api.Game](t, `{"id":"g1","names":{"international":"Game"},"ruleset":{"run-times":["realtime"],"default-time":"realtime"},
		"platforms":{"data":[{"id":"p1","name":"PC","released":1990}]},"regions":[],"genres":{"data":[]},
		"moderators":{"u1":"super-moderator"},"links":[{"rel":"series","uri":"https://x/series/s1"}],
		"categories":{"data":[{"id":"c1","name":"Any%","type":"per-game","players":{"type":"exactly","value":1}},{"id":"c2","name":"IL","type":"per-level"}]},
		"levels":{"data":[{"id":"l1","name":"L1"}]},
		"variables":{"data":[{"id":"v1","name":"Plat","category":null,"scope":{"type":"global"},"is-subcategory":true,
			"values":{"values":{"a":{"label":"A"},"b":{"label":"B"}},"default":"a"}}]}}`)
	if err := db.SaveGame(ctx, &g); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]int64{
		"SELECT COUNT(*) FROM categories WHERE game_id='g1' AND removed_at IS NULL": 2,
		"SELECT COUNT(*) FROM variable_values":                                      2,
		"SELECT COUNT(*) FROM game_platforms":                                       1,
		"SELECT COUNT(*) FROM platforms WHERE released = 1990":                      1,
		"SELECT COUNT(*) FROM game_series WHERE series_id='s1'":                     1,
		"SELECT COUNT(*) FROM game_moderators WHERE role='super-moderator'":         1,
		"SELECT COUNT(*) FROM games WHERE meta_fetched_at IS NOT NULL":              1,
	} {
		if n, err := db.Count(ctx, q); err != nil || n != want {
			t.Errorf("%s = %d (%v), want %d", q, n, err, want)
		}
	}
	g.Categories.Data = g.Categories.Data[:1]
	if err := db.SaveGame(ctx, &g); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.Count(ctx, "SELECT COUNT(*) FROM categories WHERE removed_at IS NOT NULL AND id='c2'"); n != 1 {
		t.Fatal("removed category not flagged")
	}
	vars, err := db.Vars(ctx, "g1")
	if err != nil || len(vars) != 1 || !vars[0].IsSubcategory || vars[0].Default != "a" || vars[0].ScopeType != "global" {
		t.Fatalf("vars %+v %v", vars, err)
	}
}

func TestUpsertGamesBulkReportsNew(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	db.Now = func() time.Time { return clock }
	n1, err := db.UpsertGamesBulk(ctx, []api.GameBulk{{ID: "a"}, {ID: "b"}})
	if err != nil || len(n1) != 2 {
		t.Fatalf("%v %v", n1, err)
	}
	clock = clock.Add(time.Hour)
	n2, err := db.UpsertGamesBulk(ctx, []api.GameBulk{{ID: "a"}, {ID: "c"}})
	if err != nil || len(n2) != 1 || n2[0] != "c" {
		t.Fatalf("%v %v", n2, err)
	}
}

const runsJSON = `[
 {"id":"r1","game":"g1","category":"c1","level":null,"status":{"status":"verified","examiner":"u9","verify-date":"2020-01-01T00:00:00Z"},
  "players":{"data":[{"rel":"user","id":"u1","names":{"international":"Ann"},"role":"user","location":{"country":{"code":"br","names":{"international":"Brazil"}}}}]},
  "times":{"primary_t":100},"system":{"platform":"p1","emulated":false,"region":null},"values":{"v1":"a"},
  "videos":{"links":[{"uri":"https://v/1"},{"uri":"https://v/2"}]}},
 {"id":"r2","game":"g1","category":"c1","level":null,"status":{"status":"new"},
  "players":{"data":[{"rel":"guest","name":"Bob"}]},"times":{"primary_t":90},"system":{},"values":{}}
]`

func TestUpsertRunsAndDeletion(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	db.Now = func() time.Time { return clock }
	runs := mustJSON[[]api.Run](t, runsJSON)
	if _, err := db.EnsureGame(ctx, "g1"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := db.UpsertRuns(ctx, runs, nil); err != nil {
			t.Fatal(err)
		}
	}
	for q, want := range map[string]int64{
		"SELECT COUNT(*) FROM runs":                                   2,
		"SELECT COUNT(*) FROM run_players":                            2,
		"SELECT COUNT(*) FROM users WHERE country_code='br'":          1,
		"SELECT COUNT(*) FROM guests":                                 1,
		"SELECT COUNT(*) FROM run_values":                             1,
		"SELECT COUNT(*) FROM runs WHERE video_uris LIKE '%v/1%v/2%'": 1,
	} {
		if n, err := db.Count(ctx, q); err != nil || n != want {
			t.Errorf("%s = %d (%v), want %d", q, n, err, want)
		}
	}
	// A later full crawl that only sees r1 flags r2 as deleted.
	clock = clock.Add(time.Hour)
	start := db.NowString()
	if err := db.UpsertRuns(ctx, runs[:1], nil); err != nil {
		t.Fatal(err)
	}
	n, err := db.FinishRunsCrawl(ctx, "g1", start, false)
	if err != nil || n != 1 {
		t.Fatalf("run count %d %v", n, err)
	}
	if c, _ := db.Count(ctx, "SELECT COUNT(*) FROM runs WHERE id='r2' AND deleted_at IS NOT NULL"); c != 1 {
		t.Fatal("r2 should be flagged deleted")
	}
}

func lbWith(t *testing.T, entries ...[2]any) *api.Leaderboard {
	t.Helper()
	lb := &api.Leaderboard{Timing: "realtime"}
	for _, e := range entries {
		id, tm := e[0].(string), e[1].(float64)
		lb.Runs = append(lb.Runs, api.LeaderboardEntry{Run: api.Run{ID: id, Game: "g1", Category: "c1",
			Status: api.RunStatus{Status: "verified"}, Times: api.RunTimes{PrimaryT: tm},
			Players: api.Players{{Rel: "user", ID: "u-" + id}}}})
	}
	// assign competition ranking places
	for i := range lb.Runs {
		lb.Runs[i].Place = i + 1
		if i > 0 && lb.Runs[i].Run.Times.PrimaryT == lb.Runs[i-1].Run.Times.PrimaryT {
			lb.Runs[i].Place = lb.Runs[i-1].Place
		}
	}
	return lb
}

func TestSaveBoardWritesOnlyDiffs(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	db.Now = func() time.Time { return clock }
	k := board.Key{Game: "g1", Category: "c1"}
	sub := func(*api.Run) string { return "" }
	rows := func() int64 {
		n, _ := db.Count(ctx, "SELECT COUNT(*) FROM board_runs")
		return n
	}

	res, err := db.SaveBoard(ctx, k, lbWith(t, [2]any{"a", 10.0}, [2]any{"b", 20.0}, [2]any{"old", 30.0}), sub)
	if err != nil || res.Added != 3 || res.Unchanged {
		t.Fatalf("first: %+v %v", res, err)
	}

	clock = clock.Add(time.Hour)
	res, err = db.SaveBoard(ctx, k, lbWith(t, [2]any{"a", 10.0}, [2]any{"b", 20.0}, [2]any{"old", 30.0}), sub)
	if err != nil || !res.Unchanged || rows() != 3 {
		t.Fatalf("identical refetch must not write entries: %+v rows=%d %v", res, rows(), err)
	}

	clock = clock.Add(time.Hour)
	res, err = db.SaveBoard(ctx, k, lbWith(t, [2]any{"new", 5.0}, [2]any{"a", 10.0}, [2]any{"b", 20.0}), sub)
	if err != nil || res.Added != 1 || res.Removed != 1 || rows() != 4 || res.RankMismatch != 0 {
		t.Fatalf("new WR: %+v rows=%d %v", res, rows(), err)
	}
	if n, _ := db.Count(ctx, "SELECT COUNT(*) FROM board_runs WHERE run_id='old' AND valid_to IS NOT NULL"); n != 1 {
		t.Fatal("old PB not closed")
	}
	var place int
	if err := db.R.QueryRowContext(ctx, "SELECT place FROM board_ranking_current WHERE run_id='b'").Scan(&place); err != nil || place != 3 {
		t.Fatalf("derived place of b = %d (%v), want 3", place, err)
	}

	if err := db.MarkBoardGone(ctx, k); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.Count(ctx, "SELECT COUNT(*) FROM board_runs WHERE valid_to IS NULL"); n != 0 {
		t.Fatal("gone board still has open memberships")
	}
}

func TestEnsureBoards(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	if err := db.UpsertRuns(ctx, mustJSON[[]api.Run](t, runsJSON), nil); err != nil {
		t.Fatal(err)
	}
	refs, err := db.EnsureBoards(ctx, "g1")
	if err != nil || len(refs) != 1 || refs[0].VerifiedRuns != 1 || refs[0].Key.Category != "c1" || refs[0].Key.Level != "" {
		t.Fatalf("%+v %v", refs, err)
	}
}
