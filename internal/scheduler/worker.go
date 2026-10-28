package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Outcome tells the pool what to do with a job after its handler returns
// without error. The zero value completes the job.
type Outcome struct {
	// Next, when set, re-arms the job at that time with NextPriority.
	Next         time.Time
	NextPriority float64
}

// Handler executes one job.
type Handler func(ctx context.Context, j *Job) (Outcome, error)

// ErrGone marks a job whose target no longer exists; it is not retried.
var ErrGone = errors.New("target gone")

type Pool struct {
	Queue       *Queue
	Handlers    map[string]Handler
	Workers     int
	MaxAttempts int
	Grace       time.Duration // how long in-flight jobs may run after shutdown starts
	Logger      *slog.Logger
	// OnError is called for every failed attempt (for alerting).
	OnError func(j *Job, err error, final bool)

	lastDone  atomic.Int64 // unix seconds of the last finished job
	completed atomic.Int64
}

// LastProgress returns when a job last finished (or the pool started); zero
// before Run.
func (p *Pool) LastProgress() time.Time {
	if v := p.lastDone.Load(); v != 0 {
		return time.Unix(v, 0)
	}
	return time.Time{}
}

// Completed returns the number of jobs finished since start.
func (p *Pool) Completed() int64 { return p.completed.Load() }

// Run drains the queue until ctx is canceled. In-flight jobs get Grace to
// finish before their context is canceled too.
func (p *Pool) Run(ctx context.Context) error {
	if p.Workers <= 0 {
		p.Workers = 1
	}
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 8
	}
	if p.Logger == nil {
		p.Logger = slog.Default()
	}
	p.lastDone.Store(time.Now().Unix())

	jobCtx, cancelJobs := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelJobs()
	go func() {
		<-ctx.Done()
		t := time.NewTimer(p.Grace)
		defer t.Stop()
		select {
		case <-t.C:
			cancelJobs()
		case <-jobCtx.Done():
		}
	}()

	wake := make(chan struct{}, p.Workers)
	var wg sync.WaitGroup
	for i := 0; i < p.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.worker(ctx, jobCtx, wake)
		}()
	}
	wg.Wait()
	return nil
}

func (p *Pool) worker(ctx, jobCtx context.Context, wake chan struct{}) {
	for ctx.Err() == nil {
		j, err := p.Queue.Claim(jobCtx)
		if err != nil {
			p.Logger.Error("claim failed", "err", err)
			if p.OnError != nil {
				p.OnError(&Job{Kind: "claim"}, err, false)
			}
			sleep(ctx, 5*time.Second, nil)
			continue
		}
		if j == nil {
			p.idle(ctx, wake)
			continue
		}
		p.execute(jobCtx, j)
		// A finished job may have enqueued ready work for idle workers.
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func (p *Pool) idle(ctx context.Context, wake chan struct{}) {
	wait := 5 * time.Second
	if next, err := p.Queue.NextReady(context.WithoutCancel(ctx)); err == nil && !next.IsZero() {
		if d := time.Until(next); d < wait {
			wait = max(d, 100*time.Millisecond)
		}
	}
	sleep(ctx, wait, wake)
}

func sleep(ctx context.Context, d time.Duration, wake chan struct{}) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	case <-wake:
	}
}

func (p *Pool) execute(ctx context.Context, j *Job) {
	log := p.Logger.With("kind", j.Kind, "key", j.Key, "attempt", j.Attempts)
	h, ok := p.Handlers[j.Kind]
	if !ok {
		_ = p.Queue.MarkGone(ctx, j, fmt.Errorf("no handler for kind %q", j.Kind))
		return
	}
	start := time.Now()
	out, err := safeCall(ctx, h, j)
	// Bookkeeping must survive a canceled job context.
	bg := context.WithoutCancel(ctx)
	switch {
	case err == nil:
		if out.Next.IsZero() {
			err = p.Queue.Complete(bg, j)
		} else {
			err = p.Queue.Reschedule(bg, j, out.NextPriority, out.Next)
		}
		if err != nil {
			log.Error("finishing job failed", "err", err)
		}
		p.lastDone.Store(time.Now().Unix())
		p.completed.Add(1)
		log.Debug("job done", "took", time.Since(start).Round(time.Millisecond))
	case ctx.Err() != nil:
		if err := p.Queue.Release(bg, j); err != nil {
			log.Error("releasing job failed", "err", err)
		}
	case errors.Is(err, ErrGone):
		log.Info("job target gone", "err", err)
		if err := p.Queue.MarkGone(bg, j, err); err != nil {
			log.Error("marking job gone failed", "err", err)
		}
		p.lastDone.Store(time.Now().Unix())
		p.completed.Add(1)
	default:
		delay := time.Duration(j.Attempts*j.Attempts) * time.Minute
		final, qerr := p.Queue.Retry(bg, j, err, delay, p.MaxAttempts)
		if qerr != nil {
			log.Error("retrying job failed", "err", qerr)
		}
		if final {
			log.Error("job failed permanently", "err", err)
		} else {
			log.Warn("job failed, will retry", "err", err, "in", delay)
		}
		if p.OnError != nil {
			p.OnError(j, err, final)
		}
	}
}

func safeCall(ctx context.Context, h Handler, j *Job) (out Outcome, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return h(ctx, j)
}
