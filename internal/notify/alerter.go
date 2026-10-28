package notify

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type Severity string

const (
	Info     Severity = "INFO"
	Warning  Severity = "AVISO"
	Critical Severity = "CRITICO"
)

// CooldownStore persists when each alert kind was last sent, so repeated
// conditions do not flood the inbox, even across restarts.
type CooldownStore interface {
	AlertLastSent(ctx context.Context, kind string) (time.Time, error)
	MarkAlertSent(ctx context.Context, kind string) error
}

// Alerter deduplicates alerts per kind and sends them in the background with
// retries. A failed delivery is logged and never affects the scraper.
type Alerter struct {
	N        Notifier
	Store    CooldownStore
	Log      *slog.Logger
	Cooldown time.Duration
	Prefix   string // e.g. "[speedscraper@host]"
	Retries  int
	RetryGap time.Duration

	mu       sync.Mutex
	inflight map[string]bool
	wg       sync.WaitGroup
}

// Alert sends subject/body unless an alert of the same kind was sent within
// the cooldown. Kinds that must always be delivered (phase changes) should
// be unique, e.g. "phase:runs".
func (a *Alerter) Alert(ctx context.Context, kind string, sev Severity, subject, body string) {
	a.mu.Lock()
	if a.inflight == nil {
		a.inflight = map[string]bool{}
	}
	if a.inflight[kind] {
		a.mu.Unlock()
		return
	}
	last, err := a.Store.AlertLastSent(ctx, kind)
	if err != nil {
		a.Log.Error("reading alert cooldown failed", "kind", kind, "err", err)
	}
	if !last.IsZero() && time.Since(last) < a.Cooldown {
		a.mu.Unlock()
		a.Log.Debug("alert suppressed by cooldown", "kind", kind)
		return
	}
	a.inflight[kind] = true
	a.mu.Unlock()

	full := fmt.Sprintf("%s [%s] %s", a.Prefix, sev, subject)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() {
			a.mu.Lock()
			delete(a.inflight, kind)
			a.mu.Unlock()
		}()
		bg := context.WithoutCancel(ctx)
		retries := max(a.Retries, 1)
		for i := 1; i <= retries; i++ {
			err := a.N.Send(bg, full, body)
			if err == nil {
				if err := a.Store.MarkAlertSent(bg, kind); err != nil {
					a.Log.Error("recording alert failed", "kind", kind, "err", err)
				}
				a.Log.Info("alert sent", "kind", kind, "subject", subject)
				return
			}
			a.Log.Error("sending alert failed", "kind", kind, "attempt", i, "err", err)
			if i < retries {
				time.Sleep(a.RetryGap)
			}
		}
	}()
}

// Wait blocks until in-flight alerts are delivered or abandoned.
func (a *Alerter) Wait() { a.wg.Wait() }
