package api

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limiter is the process-wide request budget shared by every worker. Besides
// the token bucket it supports a global pause, used when the API answers 420.
type Limiter struct {
	tokens *rate.Limiter

	mu          sync.Mutex
	pausedUntil time.Time
}

// NewLimiter allows rpm requests per minute with a small burst.
func NewLimiter(rpm int) *Limiter {
	if rpm <= 0 {
		rpm = 1
	}
	burst := rpm / 30
	if burst < 1 {
		burst = 1
	}
	return &Limiter{tokens: rate.NewLimiter(rate.Limit(float64(rpm)/60.0), burst)}
}

// Wait blocks until a request may be sent or ctx is done.
func (l *Limiter) Wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		until := l.pausedUntil
		l.mu.Unlock()
		d := time.Until(until)
		if d <= 0 {
			break
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	return l.tokens.Wait(ctx)
}

// Pause stops all requests for d. Overlapping pauses keep the later deadline.
func (l *Limiter) Pause(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if until := time.Now().Add(d); until.After(l.pausedUntil) {
		l.pausedUntil = until
	}
}
