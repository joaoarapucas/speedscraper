package jobs

import (
	"context"
	"errors"
	"net/url"

	"github.com/joaoarapucas/speedscraper/internal/api"
	"github.com/joaoarapucas/speedscraper/internal/scheduler"
	"github.com/joaoarapucas/speedscraper/internal/store"
)

// seedActivity reads the most recent runs of the global feeds and counts them
// per game. The counts order the initial metadata and run crawls, so the most
// active games are scraped first. The runs themselves are stored too.
func (r *Runner) seedActivity(ctx context.Context, j *scheduler.Job) (scheduler.Outcome, error) {
	counts := map[string]int{}
	feeds := []url.Values{
		{"status": {"verified"}, "orderby": {"verify-date"}, "direction": {"desc"}, "embed": {"players"}},
		{"orderby": {"submitted"}, "direction": {"desc"}, "embed": {"players"}},
	}
	size := r.runsPageSize()
	maxPages := min(r.Cfg.ActivityPages, r.API.MaxOffset()/size)
	for _, q := range feeds {
		err := api.Each(ctx, r.API, "/runs", q, 0, size, func(offset int, runs []api.Run) (bool, error) {
			for _, run := range runs {
				counts[run.Game]++
			}
			if err := r.DB.UpsertRuns(ctx, runs, nil); err != nil {
				return false, err
			}
			return offset/size+1 >= maxPages, nil
		})
		if err != nil && !errors.Is(err, api.ErrPaginationCap) {
			return scheduler.Outcome{}, err
		}
	}
	if err := r.DB.AddActivity(ctx, counts); err != nil {
		return scheduler.Outcome{}, err
	}
	// Reorder metadata jobs queued before the activity was known.
	_, err := r.DB.W.ExecContext(ctx, `UPDATE jobs SET priority = ? + 1000 * COALESCE((SELECT runs FROM game_activity WHERE game_id = jobs.key), 0)
		WHERE kind = ? AND state IN ('scheduled', 'pending')`, PrioMeta, KindGameMeta)
	r.Log.Info("activity seeded", "games", len(counts))
	return scheduler.Outcome{}, err
}

// discoverGames lists every game (bulk mode, 1000 per page) and queues the
// metadata fetch of games that never had it.
func (r *Runner) discoverGames(ctx context.Context, j *scheduler.Job) (scheduler.Outcome, error) {
	total, fresh := 0, 0
	err := api.Each(ctx, r.API, "/games", url.Values{"_bulk": {"yes"}}, 0, 1000, func(_ int, games []api.GameBulk) (bool, error) {
		ids, err := r.DB.UpsertGamesBulk(ctx, games)
		total += len(games)
		fresh += len(ids)
		return false, err
	})
	if err != nil {
		return scheduler.Outcome{}, err
	}
	now := r.DB.Now()
	_, err = r.DB.W.ExecContext(ctx, `INSERT OR IGNORE INTO jobs (kind, key, priority, not_before, state, created_at, updated_at)
		SELECT ?, g.id, ? + 1000 * COALESCE(a.runs, 0), ?, 'scheduled', ?, ?
		FROM games g LEFT JOIN game_activity a ON a.game_id = g.id
		WHERE g.meta_fetched_at IS NULL AND g.gone = 0`,
		KindGameMeta, PrioMeta, store.FormatTime(now), store.FormatTime(now), store.FormatTime(now))
	if err != nil {
		return scheduler.Outcome{}, err
	}
	if err := r.DB.SetCheckpoint(ctx, CheckpointGamesListed, store.FormatTime(now)); err != nil {
		return scheduler.Outcome{}, err
	}
	r.Log.Info("games listed", "total", total, "new", fresh)
	return scheduler.Outcome{Next: now.Add(r.Cfg.DiscoverInterval), NextPriority: PrioDiscovery + 1}, nil
}

func (r *Runner) discoverSeries(ctx context.Context, j *scheduler.Job) (scheduler.Outcome, error) {
	n := 0
	err := api.Each(ctx, r.API, "/series", nil, 0, 200, func(_ int, series []api.Series) (bool, error) {
		n += len(series)
		return false, r.DB.UpsertSeries(ctx, series)
	})
	if errors.Is(err, api.ErrPaginationCap) {
		r.Log.Warn("series listing hit the pagination cap", "stored", n)
		err = nil
	}
	if err != nil {
		return scheduler.Outcome{}, err
	}
	return scheduler.Outcome{Next: r.DB.Now().Add(r.Cfg.SeriesInterval), NextPriority: PrioDiscovery}, nil
}

// gameMeta fetches a game with categories, levels, variables and dimensions,
// then queues its run crawl.
func (r *Runner) gameMeta(ctx context.Context, j *scheduler.Job) (scheduler.Outcome, error) {
	g, err := r.API.GetGame(ctx, j.Key)
	if errors.Is(err, api.ErrNotFound) {
		if err := r.DB.MarkGameGone(ctx, j.Key); err != nil {
			return scheduler.Outcome{}, err
		}
	}
	if err != nil {
		return scheduler.Outcome{}, goneIf(err)
	}
	if err := r.DB.SaveGame(ctx, g); err != nil {
		return scheduler.Outcome{}, err
	}
	st, err := r.DB.GameState(ctx, g.ID)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	if !st.RunsCrawled {
		if err := r.Q.EnqueueOnce(ctx, KindRunsCrawl, g.ID, PrioRuns+gameScore(st), r.DB.Now()); err != nil {
			return scheduler.Outcome{}, err
		}
	}
	return scheduler.Outcome{Next: r.DB.Now().Add(r.Cfg.MetaRefresh), NextPriority: PrioReconcile + gameScore(st)}, nil
}
