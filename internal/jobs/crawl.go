package jobs

import (
	"context"
	"errors"
	"net/url"

	"github.com/joaoarapucas/speedscraper/internal/api"
	"github.com/joaoarapucas/speedscraper/internal/scheduler"
)

// runsCrawl downloads every run of a game (all statuses, players embedded),
// records the exact run count, creates the game's boards and queues the
// boards that were never fetched.
func (r *Runner) runsCrawl(ctx context.Context, j *scheduler.Job) (scheduler.Outcome, error) {
	gameID := j.Key
	start := r.DB.NowString()
	c := &crawler{r: r, gameID: gameID, size: r.runsPageSize()}
	truncated, err := c.partition(ctx, url.Values{"game": {gameID}}, 0)
	if err != nil {
		return scheduler.Outcome{}, goneIf(err)
	}
	count, err := r.DB.FinishRunsCrawl(ctx, gameID, start, truncated)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	if truncated {
		r.Log.Warn("run crawl truncated by the pagination cap", "game", gameID, "runs", count)
	}
	st, err := r.DB.GameState(ctx, gameID)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	refs, err := r.DB.EnsureBoards(ctx, gameID)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	queued := 0
	for _, ref := range refs {
		if ref.FirstFetched {
			continue
		}
		if err := r.Q.EnqueueOnce(ctx, KindBoard, ref.Key.String(), PrioBoard+boardScore(st, ref.VerifiedRuns), r.DB.Now()); err != nil {
			return scheduler.Outcome{}, err
		}
		queued++
	}
	r.Log.Info("runs crawled", "game", gameID, "runs", count, "requests", c.requests, "boards_queued", queued)
	return scheduler.Outcome{Next: r.DB.Now().Add(r.refreshPeriod(st)), NextPriority: PrioReconcile + gameScore(st)}, nil
}

type crawler struct {
	size     int
	r        *Runner
	gameID   string
	requests int
}

// splitLevels lists the ways a partition can be narrowed, in order.
const (
	splitStatus = iota
	splitCategory
	splitLevel
	splitNone
)

// partition crawls the runs matching q. When a partition holds more runs than
// the API lets us page through, it is split by status, then category, then
// level. A partition that cannot be split further is read from both ends
// (ascending and descending), which reaches up to twice the cap, and is
// reported as truncated.
func (c *crawler) partition(ctx context.Context, q url.Values, depth int) (truncated bool, err error) {
	base := cloneQuery(q)
	base.Set("orderby", "submitted")
	base.Set("direction", "asc")
	base.Set("embed", "players")

	first, err := c.page(ctx, base, 0, c.size)
	if err != nil || len(first) < c.size {
		return false, err
	}
	capOff := c.r.API.MaxOffset()
	probe, err := api.Page[api.Run](ctx, c.r.API, "/runs", base, capOff-1, 1)
	c.requests++
	if err != nil && !errors.Is(err, api.ErrPaginationCap) {
		return false, err
	}
	if err == nil && len(probe) == 0 {
		// Fits under the cap: read the rest.
		return false, c.walk(ctx, base, c.size)
	}

	for d := depth; d < splitNone; d++ {
		subs, err := c.split(ctx, q, d)
		if err != nil {
			return false, err
		}
		if len(subs) == 0 {
			continue
		}
		for _, sq := range subs {
			t, err := c.partition(ctx, sq, d+1)
			if err != nil {
				return false, err
			}
			truncated = truncated || t
		}
		return truncated, nil
	}

	if err := c.walk(ctx, base, c.size); err != nil {
		return false, err
	}
	desc := cloneQuery(base)
	desc.Set("direction", "desc")
	return true, c.walk(ctx, desc, 0)
}

// split returns the sub-queries of q along the given dimension, or nil when
// that dimension does not apply.
func (c *crawler) split(ctx context.Context, q url.Values, dim int) ([]url.Values, error) {
	with := func(k string, vals []string) []url.Values {
		out := make([]url.Values, 0, len(vals))
		for _, v := range vals {
			sq := cloneQuery(q)
			sq.Set(k, v)
			out = append(out, sq)
		}
		return out
	}
	switch dim {
	case splitStatus:
		return with("status", []string{"new", "verified", "rejected"}), nil
	case splitCategory:
		cats, err := c.r.DB.Categories(ctx, c.gameID)
		if err != nil {
			return nil, err
		}
		ids := make([]string, len(cats))
		for i, cat := range cats {
			ids[i] = cat.ID
		}
		return with("category", ids), nil
	case splitLevel:
		cat := q.Get("category")
		if cat == "" {
			return nil, nil
		}
		cats, err := c.r.DB.Categories(ctx, c.gameID)
		if err != nil {
			return nil, err
		}
		for _, cc := range cats {
			if cc.ID == cat && cc.Type == "per-level" {
				levels, err := c.r.DB.Levels(ctx, c.gameID)
				if err != nil {
					return nil, err
				}
				return with("level", levels), nil
			}
		}
	}
	return nil, nil
}

// walk pages from offset until a short page or the pagination cap.
func (c *crawler) walk(ctx context.Context, q url.Values, offset int) error {
	for off := offset; off+c.size <= c.r.API.MaxOffset(); off += c.size {
		runs, err := c.page(ctx, q, off, c.size)
		if errors.Is(err, api.ErrPaginationCap) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(runs) < c.size {
			return nil
		}
	}
	return nil
}

func (c *crawler) page(ctx context.Context, q url.Values, offset, max int) ([]api.Run, error) {
	runs, err := api.Page[api.Run](ctx, c.r.API, "/runs", q, offset, max)
	c.requests++
	if err != nil {
		return nil, err
	}
	return runs, c.r.DB.UpsertRuns(ctx, runs, nil)
}

func cloneQuery(q url.Values) url.Values {
	out := url.Values{}
	for k, v := range q {
		out[k] = append([]string(nil), v...)
	}
	return out
}
