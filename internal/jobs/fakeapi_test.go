package jobs

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fakeAPI is an in-memory speedrun.com used by the integration tests. It
// enforces a (small) pagination cap on /runs like the real API does.
type fakeAPI struct {
	mu       sync.Mutex
	cap      int
	games    []fakeGame
	runs     []fakeRun
	requests map[string]int
	// lbLimit, when set, makes leaderboard requests covering more verified
	// runs than this fail with HTTP 500, like the real API timing out.
	lbLimit int
}

type fakeGame struct {
	ID, Abbrev string
	Cats       []fakeCat
	Levels     []string
	SubVar     string   // subcategory variable ID ("" for none)
	SubVals    []string // its values, first is the default
}

type fakeCat struct {
	ID, Type string
}

type fakeRun struct {
	ID, Game, Cat, Level, Status, Player string
	SubVal                               string
	Platform                             string // "" means p1
	Emulated                             bool
	Time                                 float64
	Submitted, Verified                  time.Time
}

func (f *fakeAPI) game(id string) *fakeGame {
	for i := range f.games {
		if f.games[i].ID == id || f.games[i].Abbrev == id {
			return &f.games[i]
		}
	}
	return nil
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.requests == nil {
		f.requests = map[string]int{}
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	f.requests[parts[0]]++
	q := r.URL.Query()
	switch {
	case parts[0] == "games" && len(parts) == 1:
		var data []any
		for _, g := range f.games {
			data = append(data, map[string]any{"id": g.ID, "names": map[string]any{"international": "Game " + g.ID}, "abbreviation": g.Abbrev})
		}
		f.page(w, q, data, false)
	case parts[0] == "games" && len(parts) == 2:
		g := f.game(parts[1])
		if g == nil {
			f.status(w, 404, "not found")
			return
		}
		f.json(w, map[string]any{"data": f.gameJSON(g)})
	case parts[0] == "series":
		f.page(w, q, []any{map[string]any{"id": "s1", "names": map[string]any{"international": "Series"}}}, false)
	case parts[0] == "runs":
		f.listRuns(w, q)
	case parts[0] == "leaderboards":
		// /leaderboards/{g}/category/{c} or /leaderboards/{g}/level/{l}/{c}
		var cat, level string
		if parts[2] == "category" {
			cat = parts[3]
		} else {
			level, cat = parts[3], parts[4]
		}
		g := f.game(parts[1])
		if g == nil || !g.hasCat(cat) {
			f.status(w, 404, "not found")
			return
		}
		if f.lbLimit > 0 && len(f.boardRuns(g, cat, level, q)) > f.lbLimit {
			f.status(w, 500, "An internal error occured. We are terribly sorry, please repeat your request later.")
			return
		}
		f.json(w, map[string]any{"data": f.leaderboardFiltered(g, cat, level, q)})
	default:
		f.status(w, 404, "unknown endpoint")
	}
}

func (g *fakeGame) hasCat(id string) bool {
	for _, c := range g.Cats {
		if c.ID == id {
			return true
		}
	}
	return false
}

func (f *fakeAPI) gameJSON(g *fakeGame) map[string]any {
	var cats, levels, vars []any
	for _, c := range g.Cats {
		cats = append(cats, map[string]any{"id": c.ID, "name": c.ID, "type": c.Type, "players": map[string]any{"type": "exactly", "value": 1}})
	}
	for _, l := range g.Levels {
		levels = append(levels, map[string]any{"id": l, "name": l})
	}
	if g.SubVar != "" {
		vals := map[string]any{}
		for _, v := range g.SubVals {
			vals[v] = map[string]any{"label": v}
		}
		vars = append(vars, map[string]any{"id": g.SubVar, "name": "sub", "category": nil, "scope": map[string]any{"type": "global"},
			"is-subcategory": true, "obsoletes": true, "values": map[string]any{"values": vals, "default": g.SubVals[0]}})
	}
	return map[string]any{
		"id": g.ID, "names": map[string]any{"international": "Game " + g.ID}, "abbreviation": g.Abbrev,
		"ruleset":    map[string]any{"run-times": []string{"realtime"}, "default-time": "realtime"},
		"platforms":  map[string]any{"data": []any{map[string]any{"id": "p1", "name": "PC"}}},
		"regions":    map[string]any{"data": []any{}},
		"moderators": map[string]any{"mod1": "moderator"},
		"categories": map[string]any{"data": cats},
		"levels":     map[string]any{"data": levels},
		"variables":  map[string]any{"data": vars},
	}
}

func (f *fakeAPI) runJSON(r fakeRun, embedPlayers bool) map[string]any {
	var players any = []any{map[string]any{"rel": "user", "id": r.Player, "uri": "x"}}
	if embedPlayers {
		players = map[string]any{"data": []any{map[string]any{"rel": "user", "id": r.Player,
			"names": map[string]any{"international": "Player " + r.Player}, "role": "user"}}}
	}
	status := map[string]any{"status": r.Status}
	if r.Status == "verified" {
		status["verify-date"] = r.Verified.UTC().Format(time.RFC3339)
	}
	var level any
	if r.Level != "" {
		level = r.Level
	}
	values := map[string]string{}
	if r.SubVal != "" {
		values[f.game(r.Game).SubVar] = r.SubVal
	}
	return map[string]any{
		"id": r.ID, "game": r.Game, "category": r.Cat, "level": level, "status": status, "players": players,
		"submitted": r.Submitted.UTC().Format(time.RFC3339), "times": map[string]any{"primary_t": r.Time},
		"system": map[string]any{"platform": r.platform(), "emulated": r.Emulated}, "values": values,
	}
}

func (f *fakeAPI) listRuns(w http.ResponseWriter, q map[string][]string) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	var sel []fakeRun
	for _, r := range f.runs {
		if (get("game") == "" || r.Game == get("game")) && (get("status") == "" || r.Status == get("status")) &&
			(get("category") == "" || r.Cat == get("category")) && (get("level") == "" || r.Level == get("level")) {
			sel = append(sel, r)
		}
	}
	key := func(r fakeRun) time.Time { return r.Submitted }
	if get("orderby") == "verify-date" {
		key = func(r fakeRun) time.Time { return r.Verified }
	}
	sort.SliceStable(sel, func(i, j int) bool {
		if get("direction") == "desc" {
			return key(sel[i]).After(key(sel[j]))
		}
		return key(sel[i]).Before(key(sel[j]))
	})
	var data []any
	for _, r := range sel {
		data = append(data, f.runJSON(r, get("embed") == "players"))
	}
	f.page(w, q, data, true)
}

func (f *fakeAPI) page(w http.ResponseWriter, q map[string][]string, all []any, capped bool) {
	atoi := func(k string, def int) int {
		if v := q[k]; len(v) > 0 {
			n, _ := strconv.Atoi(v[0])
			return n
		}
		return def
	}
	off, max := atoi("offset", 0), atoi("max", 20)
	if capped && off+max > f.cap {
		f.status(w, 400, "Invalid pagination values.")
		return
	}
	end := min(off+max, len(all))
	data := []any{}
	if off < len(all) {
		data = all[off:end]
	}
	f.json(w, map[string]any{"data": data, "pagination": map[string]any{"offset": off, "max": max, "size": len(data)}})
}

// boardRuns returns the verified runs of a leaderboard that pass the
// var-, platform and emulators filters.
func (f *fakeAPI) boardRuns(g *fakeGame, cat, level string, q map[string][]string) []fakeRun {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	var out []fakeRun
	for _, r := range f.runs {
		if r.Game != g.ID || r.Cat != cat || r.Level != level || r.Status != "verified" {
			continue
		}
		if v := get("var-" + g.SubVar); g.SubVar != "" && v != "" && r.subOrDefault(g) != v {
			continue
		}
		if p := get("platform"); p != "" && r.platform() != p {
			continue
		}
		if e := get("emulators"); e != "" && fmt.Sprint(r.Emulated) != e {
			continue
		}
		out = append(out, r)
	}
	return out
}

// leaderboard returns the union of the subcategory boards: the best verified
// run of each player per subcategory value, ranked jointly by time.
func (f *fakeAPI) leaderboard(g *fakeGame, cat, level string) map[string]any {
	return f.leaderboardFiltered(g, cat, level, nil)
}

func (f *fakeAPI) leaderboardFiltered(g *fakeGame, cat, level string, q map[string][]string) map[string]any {
	best := map[string]fakeRun{}
	for _, r := range f.boardRuns(g, cat, level, q) {
		k := r.Player + "/" + r.subOrDefault(g)
		if cur, ok := best[k]; !ok || r.Time < cur.Time {
			best[k] = r
		}
	}
	runs := make([]fakeRun, 0, len(best))
	for _, r := range best {
		runs = append(runs, r)
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].Time != runs[j].Time {
			return runs[i].Time < runs[j].Time
		}
		return runs[i].ID < runs[j].ID
	})
	var entries, players []any
	for i, r := range runs {
		place := i + 1
		if i > 0 && r.Time == runs[i-1].Time {
			place = entries[i-1].(map[string]any)["place"].(int)
		}
		entries = append(entries, map[string]any{"place": place, "run": f.runJSON(r, false)})
		players = append(players, map[string]any{"rel": "user", "id": r.Player, "names": map[string]any{"international": "Player " + r.Player}})
	}
	var lvl any
	if level != "" {
		lvl = level
	}
	return map[string]any{"game": g.ID, "category": cat, "level": lvl, "timing": "realtime", "values": map[string]any{},
		"runs": entries, "players": map[string]any{"data": players}}
}

func (r fakeRun) platform() string {
	if r.Platform == "" {
		return "p1"
	}
	return r.Platform
}

func (r fakeRun) subOrDefault(g *fakeGame) string {
	if r.SubVal != "" {
		return r.SubVal
	}
	if len(g.SubVals) > 0 {
		return g.SubVals[0]
	}
	return ""
}

func (f *fakeAPI) json(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeAPI) status(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	f.json(w, map[string]any{"status": code, "message": msg})
}

func (f *fakeAPI) addRun(r fakeRun) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, r)
}

func (f *fakeAPI) requestCount(endpoint string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[endpoint]
}

// newUniverse builds two games. g1 has a per-game category with more verified
// runs than the cap (forcing partition splits and the two-ended read), an IL
// category, and a subcategory variable. g2 is small.
func newUniverse(cap int, base time.Time) *fakeAPI {
	f := &fakeAPI{cap: cap}
	f.games = []fakeGame{
		{ID: "g1", Abbrev: "one", Cats: []fakeCat{{"c1", "per-game"}, {"c2", "per-level"}}, Levels: []string{"l1", "l2"},
			SubVar: "v1", SubVals: []string{"a", "b"}},
		{ID: "g2", Abbrev: "two", Cats: []fakeCat{{"d1", "per-game"}}},
	}
	n := 0
	add := func(game, cat, level, status, player, sub string, t float64) {
		n++
		ts := base.Add(time.Duration(n) * time.Minute)
		f.runs = append(f.runs, fakeRun{ID: fmt.Sprintf("r%03d", n), Game: game, Cat: cat, Level: level, Status: status,
			Player: player, SubVal: sub, Time: t, Submitted: ts, Verified: ts.Add(time.Hour)})
	}
	for i := 0; i < 30; i++ { // 30 verified c1 runs, 15 players, two subcategories
		sub := "a"
		if i%3 == 0 {
			sub = "b"
		}
		add("g1", "c1", "", "verified", fmt.Sprintf("u%02d", i%15), sub, float64(100+i))
	}
	for i := 0; i < 6; i++ {
		add("g1", "c1", "", "rejected", fmt.Sprintf("u%02d", i), "a", 50)
		add("g1", "c1", "", "new", fmt.Sprintf("u%02d", i), "a", 60)
	}
	for i := 0; i < 8; i++ {
		add("g1", "c2", []string{"l1", "l2"}[i%2], "verified", fmt.Sprintf("u%02d", i%4), "", float64(10+i%3))
	}
	for i := 0; i < 5; i++ {
		add("g2", "d1", "", "verified", fmt.Sprintf("w%d", i), "", float64(200-i))
	}
	return f
}
