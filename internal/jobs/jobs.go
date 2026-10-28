// Package jobs implements the scraping jobs executed by the scheduler.
package jobs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/joaoarapucas/speedscraper/internal/api"
	"github.com/joaoarapucas/speedscraper/internal/scheduler"
	"github.com/joaoarapucas/speedscraper/internal/store"
)

// Job kinds.
const (
	KindSeedActivity   = "seed_activity"
	KindDiscoverGames  = "discover_games"
	KindDiscoverSeries = "discover_series"
	KindFeedVerified   = "feed_verified"
	KindFeedSubmitted  = "feed_submitted"
	KindGameMeta       = "game_meta"
	KindRunsCrawl      = "runs_crawl"
	KindBoard          = "board"
)

// Priority bands. Within a band jobs are ordered by a game score, so the
// initial load proceeds metadata -> runs -> leaderboards, each phase in order
// of game size, while feeds and dirty boards always jump ahead.
const (
	band           = 1e12
	PrioFeed       = 9 * band
	PrioDirtyBoard = 8 * band
	PrioDiscovery  = 7 * band
	PrioMeta       = 6 * band
	PrioRuns       = 5 * band
	PrioBoard      = 4 * band
	PrioReconcile  = 2 * band
)

// Checkpoint keys.
const (
	CheckpointGamesListed   = "games_listed"
	CheckpointFeedVerified  = "feed_verified"
	CheckpointFeedSubmitted = "feed_submitted"
)

type Config struct {
	FeedInterval     time.Duration // how often the run feeds are polled
	FeedOverlap      time.Duration // re-read window behind the feed checkpoint
	DirtyDelay       time.Duration // debounce before refetching a changed board
	HotRunCount      int           // games with at least this many runs are refreshed more often
	HotRefresh       time.Duration // board/run reconciliation period of hot games
	ColdRefresh      time.Duration // same for the other games
	MetaRefresh      time.Duration // game metadata refresh period
	DiscoverInterval time.Duration // full game listing period
	SeriesInterval   time.Duration
	ActivityPages    int      // pages of each global feed read to seed priorities
	RunsPageSize     int      // page size for /runs (API maximum: 200)
	OnlyGames        []string // restrict scraping to these games (IDs or abbreviations)
}

func DefaultConfig() Config {
	return Config{
		FeedInterval:     5 * time.Minute,
		FeedOverlap:      time.Hour,
		DirtyDelay:       10 * time.Minute,
		HotRunCount:      1000,
		HotRefresh:       7 * 24 * time.Hour,
		ColdRefresh:      30 * 24 * time.Hour,
		MetaRefresh:      7 * 24 * time.Hour,
		DiscoverInterval: 24 * time.Hour,
		SeriesInterval:   7 * 24 * time.Hour,
		ActivityPages:    50,
		RunsPageSize:     200,
	}
}

type Runner struct {
	API *api.Client
	DB  *store.DB
	Q   *scheduler.Queue
	Cfg Config
	Log *slog.Logger
	// Warn, when set, reports conditions worth an alert that do not fail a job.
	Warn func(ctx context.Context, kind, subject, body string)
}

// Handlers maps job kinds to their implementation.
func (r *Runner) Handlers() map[string]scheduler.Handler {
	return map[string]scheduler.Handler{
		KindSeedActivity:   r.seedActivity,
		KindDiscoverGames:  r.discoverGames,
		KindDiscoverSeries: r.discoverSeries,
		KindFeedVerified:   r.feedVerified,
		KindFeedSubmitted:  r.feedSubmitted,
		KindGameMeta:       r.gameMeta,
		KindRunsCrawl:      r.runsCrawl,
		KindBoard:          r.board,
	}
}

// Bootstrap queues the recurring jobs. It is safe to call on every start.
func (r *Runner) Bootstrap(ctx context.Context) error {
	now := r.DB.Now()
	if len(r.Cfg.OnlyGames) > 0 {
		return r.bootstrapOnly(ctx)
	}
	if err := r.Q.EnqueueOnce(ctx, KindSeedActivity, "", PrioDiscovery+2, now); err != nil {
		return err
	}
	for _, j := range []struct {
		kind string
		prio float64
	}{
		{KindDiscoverGames, PrioDiscovery + 1},
		{KindDiscoverSeries, PrioDiscovery},
		{KindFeedVerified, PrioFeed},
		{KindFeedSubmitted, PrioFeed},
	} {
		if err := r.Q.Enqueue(ctx, j.kind, "", j.prio, now); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) bootstrapOnly(ctx context.Context) error {
	for _, ref := range r.Cfg.OnlyGames {
		g, err := r.API.GetGame(ctx, ref)
		if err != nil {
			return err
		}
		if err := r.DB.SaveGame(ctx, g); err != nil {
			return err
		}
		if err := r.Q.EnqueueOnce(ctx, KindRunsCrawl, g.ID, PrioRuns, r.DB.Now()); err != nil {
			return err
		}
		r.Log.Info("restricted mode: game queued", "game", g.ID, "name", g.Names.International)
	}
	return r.DB.SetCheckpoint(ctx, CheckpointGamesListed, store.FormatTime(r.DB.Now()))
}

// runsPageSize is the page size for /runs, never above the pagination cap.
func (r *Runner) runsPageSize() int {
	size := r.Cfg.RunsPageSize
	if size <= 0 || size > 200 {
		size = 200
	}
	return min(size, r.API.MaxOffset())
}

// gameScore orders games inside a priority band: exact run count once
// crawled, recent activity before that.
func gameScore(st store.GameState) float64 {
	if st.RunsCrawled {
		return float64(st.RunCount) * 1000
	}
	return float64(st.Activity) * 1000
}

func boardScore(st store.GameState, verifiedRuns int) float64 {
	return gameScore(st) + float64(min(verifiedRuns, 999))
}

func (r *Runner) refreshPeriod(st store.GameState) time.Duration {
	if st.RunCount >= r.Cfg.HotRunCount {
		return r.Cfg.HotRefresh
	}
	return r.Cfg.ColdRefresh
}

// goneIf converts "the target does not exist" API errors into ErrGone.
func goneIf(err error) error {
	if errors.Is(err, api.ErrNotFound) {
		return errors.Join(scheduler.ErrGone, err)
	}
	return err
}
