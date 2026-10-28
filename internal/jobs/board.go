package jobs

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"

	"github.com/joaoarapucas/speedscraper/internal/api"
	"github.com/joaoarapucas/speedscraper/internal/board"
	"github.com/joaoarapucas/speedscraper/internal/scheduler"
	"github.com/joaoarapucas/speedscraper/internal/store"
)

// board fetches one leaderboard (all subcategories at once) and stores only
// the runs that entered or left it. Leaderboards too large for the API, which
// answers them with a server error, are fetched in slices instead.
func (r *Runner) board(ctx context.Context, j *scheduler.Job) (scheduler.Outcome, error) {
	k, err := board.ParseKey(j.Key)
	if err != nil {
		return scheduler.Outcome{}, errors.Join(scheduler.ErrGone, err)
	}
	wasSplit, err := r.DB.BoardSplit(ctx, k)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	var data store.BoardData
	var fullErr error
	if !wasSplit {
		lb, err := r.API.GetLeaderboard(ctx, k.Game, k.Category, k.Level, nil)
		if err != nil {
			fullErr = err
			if isGone(err) {
				if gerr := r.DB.MarkBoardGone(ctx, k); gerr != nil {
					return scheduler.Outcome{}, gerr
				}
				return scheduler.Outcome{}, errors.Join(scheduler.ErrGone, err)
			}
			if !isServerError(err) {
				return scheduler.Outcome{}, err
			}
			r.Log.Warn("leaderboard too large for the API, fetching it in slices", "board", j.Key, "err", err)
		}
		data.LB = lb
	}
	vars, err := r.DB.Vars(ctx, k.Game)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	subcats := board.Subcategories(vars, k.Category, k.Level)
	subcatKey := func(run *api.Run) string { return board.SubcatKey(subcats, run.Values) }
	if data.LB == nil {
		var slices int
		data, slices, err = r.fetchSplit(ctx, k, subcats)
		if err != nil {
			return scheduler.Outcome{}, errors.Join(fullErr, err)
		}
		if !wasSplit {
			r.reportSplit(ctx, j.Key, slices, data.ComputedSlices)
		}
	}
	res, err := r.DB.SaveBoardData(ctx, k, data, subcatKey)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	if res.RankMismatch > 0 && !res.Unchanged {
		r.Log.Debug("board places differ from time ranking", "board", j.Key, "mismatches", res.RankMismatch)
	}
	if !res.Unchanged {
		r.Log.Debug("board changed", "board", j.Key, "entries", res.Entries, "added", res.Added, "removed", res.Removed)
	}
	st, err := r.DB.GameState(ctx, k.Game)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	return scheduler.Outcome{
		Next:         r.DB.Now().Add(r.refreshPeriod(st)),
		NextPriority: PrioReconcile + boardScore(st, res.Entries),
	}, nil
}

func isGone(err error) bool {
	var he *api.HTTPError
	return errors.Is(err, api.ErrNotFound) || (errors.As(err, &he) && he.Status >= 400 && he.Status < 500 &&
		he.Status != 420 && he.Status != 429)
}

// isServerError reports whether the API failed to build a response, which for
// leaderboards means it timed out on a board with too many runs.
func isServerError(err error) bool {
	var he *api.HTTPError
	if errors.As(err, &he) {
		return he.Status >= 500
	}
	var ue *url.Error
	return errors.As(err, &ue) && ue.Timeout()
}

func (r *Runner) reportSplit(ctx context.Context, key string, slices, computed int) {
	r.Log.Warn("leaderboard fetched in slices", "board", key, "slices", slices, "computed", computed)
	if r.Warn == nil {
		return
	}
	body := fmt.Sprintf("A API devolve erro de servidor para o leaderboard %s, grande demais para ela montar.\n"+
		"Ele passa a ser buscado em %d fatias (subcategoria, plataforma, emulador).\n", key, slices)
	if computed > 0 {
		body += fmt.Sprintf("%d fatia(s) falham mesmo assim e foram ranqueadas a partir das runs do banco "+
			"(boards.computed_slices). Elas so ficam completas se o crawl de runs do jogo nao foi truncado.\n", computed)
	}
	r.Warn(ctx, "board_split:"+key, "Leaderboard buscado em fatias: "+key, body)
}

// errNoSlice reports a split fetch in which the API served no slice at all,
// so the board is left untouched rather than rebuilt from stored runs only.
var errNoSlice = errors.New("leaderboard: the API served none of the slices")

// boardSlice is a part of a leaderboard selected with API filters. local holds
// the stored verified runs that fall in it, used to find its sub-slices and to
// rank it when even the smallest slice cannot be fetched.
type boardSlice struct {
	filter url.Values
	subcat string
	local  []store.LocalRun
	depth  int // 0 subcategory, 1 platform, 2 emulator
}

type candidate struct {
	run    *api.Run // nil for a stored run
	id     string
	t      float64
	date   string
	subcat string
}

// beats reports whether c ranks ahead of o for the same player. Ties on time
// go to the run played first, as on the site.
func (c candidate) beats(o candidate) bool {
	if c.t != o.t {
		return c.t < o.t
	}
	cd, od := c.date, o.date
	if cd == "" {
		cd = "~" // unknown dates lose ties
	}
	if od == "" {
		od = "~"
	}
	if cd != od {
		return cd < od
	}
	return c.id < o.id
}

// splitFetch merges the slices of one leaderboard, keeping the best run of
// each player in each subcategory as the unfiltered leaderboard does.
type splitFetch struct {
	best     map[string]candidate
	players  api.Players
	timing   string
	slices   int
	fetched  int
	computed int
}

func (s *splitFetch) add(c candidate, players string) {
	key := c.subcat + "\x00" + players
	if cur, ok := s.best[key]; ok && !c.beats(cur) {
		return
	}
	s.best[key] = c
}

func (r *Runner) fetchSplit(ctx context.Context, k board.Key, subcats []board.Var) (store.BoardData, int, error) {
	local, err := r.DB.BoardLocalRuns(ctx, k)
	if err != nil {
		return store.BoardData{}, 0, err
	}
	bySub := map[string][]store.LocalRun{}
	for _, lr := range local {
		sk := board.SubcatKey(subcats, lr.Values)
		bySub[sk] = append(bySub[sk], lr)
	}
	keys := make([]string, 0, len(bySub))
	for sk := range bySub {
		keys = append(keys, sk)
	}
	sort.Strings(keys)

	sf := &splitFetch{best: map[string]candidate{}}
	for _, sk := range keys {
		runs := bySub[sk]
		s := boardSlice{filter: url.Values{}, subcat: sk, local: runs}
		for _, v := range subcats {
			s.filter.Set("var-"+v.ID, board.SubcatValue(v, runs[0].Values))
		}
		if len(subcats) == 0 {
			// The only slice is the whole board, which already failed.
			err = r.splitSlice(ctx, k, s, sf)
		} else {
			err = r.fetchSlice(ctx, k, s, sf)
		}
		if err != nil {
			return store.BoardData{}, 0, err
		}
	}
	if sf.fetched == 0 {
		return store.BoardData{}, 0, errNoSlice
	}

	data := store.BoardData{LB: &api.Leaderboard{Timing: sf.timing, Players: sf.players}, Split: true, ComputedSlices: sf.computed}
	cands := make([]candidate, 0, len(sf.best))
	for _, c := range sf.best {
		cands = append(cands, c)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].id < cands[j].id })
	times := make([]float64, len(cands))
	for i, c := range cands {
		times[i] = c.t
	}
	// Places are ranked jointly across subcategories, like the unfiltered endpoint.
	for i, place := range board.Rank(times) {
		c := cands[i]
		if c.run != nil {
			data.LB.Runs = append(data.LB.Runs, api.LeaderboardEntry{Place: place, Run: *c.run})
		} else {
			data.Local = append(data.Local, store.LocalEntry{RunID: c.id, Place: place, PrimaryT: c.t, Subcat: c.subcat})
		}
	}
	return data, sf.slices, nil
}

// fetchSlice fetches one slice, splitting it further when the API fails.
// Slices are requested without retries: a smaller slice is the better retry.
func (r *Runner) fetchSlice(ctx context.Context, k board.Key, s boardSlice, sf *splitFetch) error {
	lb, err := r.API.GetLeaderboard(api.WithMaxRetries(ctx, 0), k.Game, k.Category, k.Level, s.filter)
	if err != nil {
		if ctx.Err() != nil || !isServerError(err) {
			return err
		}
		return r.splitSlice(ctx, k, s, sf)
	}
	sf.slices++
	sf.fetched++
	if sf.timing == "" {
		sf.timing = lb.Timing
	}
	sf.players = append(sf.players, lb.Players...)
	for i := range lb.Runs {
		run := &lb.Runs[i].Run
		date := ""
		if run.Date != nil {
			date = *run.Date
		}
		sf.add(candidate{run: run, id: run.ID, t: run.Times.PrimaryT, date: date, subcat: s.subcat}, runPlayersKey(run))
	}
	return nil
}

// splitSlice fetches the sub-slices of a slice the API could not serve, and
// ranks from stored runs whatever cannot be split any further.
func (r *Runner) splitSlice(ctx context.Context, k board.Key, s boardSlice, sf *splitFetch) error {
	var children []boardSlice
	var rest []store.LocalRun
	switch s.depth {
	case 0:
		byPlatform := map[string][]store.LocalRun{}
		for _, lr := range s.local {
			if lr.Platform == "" {
				rest = append(rest, lr) // cannot be selected by a platform filter
				continue
			}
			byPlatform[lr.Platform] = append(byPlatform[lr.Platform], lr)
		}
		for p, runs := range byPlatform {
			children = append(children, s.child("platform", p, runs))
		}
	case 1:
		byEmu := map[bool][]store.LocalRun{}
		for _, lr := range s.local {
			byEmu[lr.Emulated] = append(byEmu[lr.Emulated], lr)
		}
		for emu, runs := range byEmu {
			children = append(children, s.child("emulators", fmt.Sprint(emu), runs))
		}
	default:
		rest = s.local
	}
	sort.Slice(children, func(i, j int) bool { return children[i].filter.Encode() < children[j].filter.Encode() })

	if len(rest) > 0 {
		sf.slices++
		sf.computed++
		r.Log.Debug("leaderboard slice ranked from stored runs", "board", k.String(), "filter", s.filter.Encode(), "runs", len(rest))
		for _, lr := range rest {
			sf.add(candidate{id: lr.ID, t: lr.PrimaryT, date: lr.Date, subcat: s.subcat}, lr.Players)
		}
	}
	for _, c := range children {
		var err error
		if len(children) == 1 && len(rest) == 0 {
			err = r.splitSlice(ctx, k, c, sf) // same runs as the slice that just failed
		} else {
			err = r.fetchSlice(ctx, k, c, sf)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s boardSlice) child(param, value string, local []store.LocalRun) boardSlice {
	f := url.Values{}
	for k, v := range s.filter {
		f[k] = v
	}
	f.Set(param, value)
	return boardSlice{filter: f, subcat: s.subcat, local: local, depth: s.depth + 1}
}

func runPlayersKey(run *api.Run) string {
	members := make([]string, 0, len(run.Players))
	for _, p := range run.Players {
		if p.Rel == "user" {
			members = append(members, "u:"+p.ID)
		} else {
			members = append(members, "g:"+p.Name)
		}
	}
	if len(members) == 0 {
		members = []string{"r:" + run.ID}
	}
	return board.PlayersKey(members)
}
