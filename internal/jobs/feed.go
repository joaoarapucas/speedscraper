package jobs

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/joaoarapucas/speedscraper/internal/api"
	"github.com/joaoarapucas/speedscraper/internal/board"
	"github.com/joaoarapucas/speedscraper/internal/scheduler"
	"github.com/joaoarapucas/speedscraper/internal/store"
)

// ErrFeedGap reports that a feed fell so far behind that runs between the
// checkpoint and the oldest reachable page could not be read.
var ErrFeedGap = errors.New("feed fell behind the pagination cap; reconciliation will cover the gap")

// feedVerified reads newly verified runs since the last checkpoint and marks
// the boards they belong to as dirty. This is what keeps leaderboards current
// without refetching the ones that did not change.
func (r *Runner) feedVerified(ctx context.Context, j *scheduler.Job) (scheduler.Outcome, error) {
	q := url.Values{"status": {"verified"}, "orderby": {"verify-date"}, "direction": {"desc"}, "embed": {"players"}}
	err := r.readFeed(ctx, CheckpointFeedVerified, q, func(run *api.Run) *string { return run.Status.VerifyDate }, true)
	return r.feedOutcome(ctx, j, err)
}

// feedSubmitted reads newly submitted runs (any status), which captures
// pending runs that the verified feed never shows.
func (r *Runner) feedSubmitted(ctx context.Context, j *scheduler.Job) (scheduler.Outcome, error) {
	q := url.Values{"orderby": {"submitted"}, "direction": {"desc"}, "embed": {"players"}}
	err := r.readFeed(ctx, CheckpointFeedSubmitted, q, func(run *api.Run) *string { return run.Submitted }, false)
	return r.feedOutcome(ctx, j, err)
}

// feedOutcome re-arms a feed job. A feed gap is not retried (the next poll
// starts from the newest run again) but is reported.
func (r *Runner) feedOutcome(ctx context.Context, j *scheduler.Job, err error) (scheduler.Outcome, error) {
	if errors.Is(err, ErrFeedGap) {
		if r.Warn != nil {
			r.Warn(ctx, "feed_gap:"+j.Kind, "Feed "+j.Kind+" fell behind",
				"The "+j.Kind+" feed had more new runs than the API lets us page through. Runs in the gap are picked up by the periodic re-crawl.")
		}
		err = nil
	}
	if err != nil {
		return scheduler.Outcome{}, err
	}
	return scheduler.Outcome{Next: r.DB.Now().Add(r.Cfg.FeedInterval), NextPriority: PrioFeed}, nil
}

// readFeed pages a newest-first feed until it reaches the checkpoint minus
// the overlap window, then advances the checkpoint. On the very first run
// only one page is read: the initial crawl covers history.
func (r *Runner) readFeed(ctx context.Context, key string, q url.Values, stamp func(*api.Run) *string, markDirty bool) error {
	cp, _, err := r.DB.Checkpoint(ctx, key)
	if err != nil {
		return err
	}
	stopBefore := ""
	if cp != "" {
		if t, err := time.Parse(store.TimeFormat, cp); err == nil {
			stopBefore = store.FormatTime(t.Add(-r.Cfg.FeedOverlap))
		}
	}
	newest := cp
	runsRead := 0
	err = api.Each(ctx, r.API, "/runs", q, 0, r.runsPageSize(), func(_ int, runs []api.Run) (bool, error) {
		ids := make([]string, len(runs))
		for i := range runs {
			ids[i] = runs[i].ID
		}
		before, err := r.DB.RunSnapshots(ctx, ids)
		if err != nil {
			return false, err
		}
		if err := r.DB.UpsertRuns(ctx, runs, nil); err != nil {
			return false, err
		}
		runsRead += len(runs)
		reached := cp == ""
		if err := r.afterFeedPage(ctx, runs, before, markDirty); err != nil {
			return false, err
		}
		for i := range runs {
			s := stamp(&runs[i])
			if s == nil {
				continue
			}
			if *s > newest {
				newest = *s
			}
			if stopBefore != "" && *s < stopBefore {
				reached = true
			}
		}
		return reached, nil
	})
	gap := errors.Is(err, api.ErrPaginationCap)
	if err != nil && !gap {
		return err
	}
	if newest != "" && newest != cp {
		if err := r.DB.SetCheckpoint(ctx, key, newest); err != nil {
			return err
		}
	}
	r.Log.Debug("feed read", "feed", key, "runs", runsRead, "checkpoint", newest)
	if gap {
		r.Log.Warn("feed gap", "feed", key, "runs", runsRead)
		return ErrFeedGap
	}
	return nil
}

// afterFeedPage makes sure the games of the runs are known and, for runs that
// became verified or changed time since they were last stored, schedules a
// debounced refetch of their boards. Runs re-read in the overlap window are
// unchanged and cost nothing.
func (r *Runner) afterFeedPage(ctx context.Context, runs []api.Run, before map[string]store.RunSnapshot, markDirty bool) error {
	now := r.DB.Now()
	activity := map[string]int{}
	states := map[string]store.GameState{}
	dirty := map[board.Key]bool{}
	for i := range runs {
		run := &runs[i]
		activity[run.Game]++
		st, ok := states[run.Game]
		if !ok {
			var err error
			if st, err = r.DB.GameState(ctx, run.Game); err != nil {
				return err
			}
			states[run.Game] = st
		}
		if !st.Known {
			if _, err := r.DB.EnsureGame(ctx, run.Game); err != nil {
				return err
			}
			if err := r.Q.EnqueueOnce(ctx, KindGameMeta, run.Game, PrioMeta, now); err != nil {
				return err
			}
			st.Known = true
			states[run.Game] = st
			continue
		}
		if st.MetaFetched {
			ok, err := r.DB.HasCategoryAndLevel(ctx, run.Category, run.LevelID())
			if err != nil {
				return err
			}
			if !ok {
				// New category or level: refresh metadata before the board.
				if err := r.Q.Enqueue(ctx, KindGameMeta, run.Game, PrioDiscovery, now); err != nil {
					return err
				}
			}
		}
		prev, seen := before[run.ID]
		changed := !seen || prev.Status != run.Status.Status || prev.PrimaryT != run.Times.PrimaryT
		if markDirty && changed && st.RunsCrawled && run.Status.Status == "verified" {
			dirty[board.Key{Game: run.Game, Category: run.Category, Level: run.LevelID()}] = true
		}
	}
	for k := range dirty {
		if err := r.Q.Enqueue(ctx, KindBoard, k.String(), PrioDirtyBoard+boardScore(states[k.Game], 0), now.Add(r.Cfg.DirtyDelay)); err != nil {
			return err
		}
	}
	return r.DB.AddActivity(ctx, activity)
}
