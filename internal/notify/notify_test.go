package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type memStore struct {
	mu   sync.Mutex
	sent map[string]time.Time
}

func (m *memStore) AlertLastSent(_ context.Context, kind string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sent[kind], nil
}

func (m *memStore) MarkAlertSent(_ context.Context, kind string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sent == nil {
		m.sent = map[string]time.Time{}
	}
	m.sent[kind] = time.Now()
	return nil
}

type fakeNotifier struct {
	mu       sync.Mutex
	subjects []string
	failures int
}

func (f *fakeNotifier) Send(_ context.Context, subject, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures > 0 {
		f.failures--
		return errors.New("smtp down")
	}
	f.subjects = append(f.subjects, subject)
	return nil
}

func newAlerter(n Notifier) *Alerter {
	return &Alerter{N: n, Store: &memStore{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Cooldown: time.Hour, Prefix: "[test]", Retries: 3, RetryGap: time.Millisecond}
}

func TestAlerterCooldownPerKind(t *testing.T) {
	n := &fakeNotifier{}
	a := newAlerter(n)
	ctx := context.Background()
	a.Alert(ctx, "throttle", Warning, "rate limited", "x")
	a.Wait()
	a.Alert(ctx, "throttle", Warning, "rate limited", "x") // suppressed
	a.Alert(ctx, "phase:runs", Info, "phase finished", "y")
	a.Wait()
	if len(n.subjects) != 2 {
		t.Fatalf("sent %v", n.subjects)
	}
	if !strings.HasPrefix(n.subjects[0], "[test] [AVISO] ") {
		t.Fatalf("subject %q", n.subjects[0])
	}
}

func TestAlerterRetriesAndDoesNotRecordFailures(t *testing.T) {
	n := &fakeNotifier{failures: 2}
	a := newAlerter(n)
	a.Alert(context.Background(), "k", Critical, "s", "b")
	a.Wait()
	if len(n.subjects) != 1 {
		t.Fatalf("want delivery on third attempt, got %v", n.subjects)
	}

	n = &fakeNotifier{failures: 10}
	a = newAlerter(n)
	a.Alert(context.Background(), "k", Critical, "s", "b")
	a.Wait()
	if last, _ := a.Store.AlertLastSent(context.Background(), "k"); !last.IsZero() {
		t.Fatal("undelivered alert must not start the cooldown")
	}
}

func TestBuildMessage(t *testing.T) {
	msg := string(buildMessage("a@x", []string{"b@x", "c@x"}, "Fase concluída", "linha1\nlinha2", time.Unix(0, 0)))
	for _, want := range []string{"From: a@x\r\n", "To: b@x, c@x\r\n", "Subject: =?UTF-8?b?", "charset=UTF-8", "linha1\r\nlinha2\r\n"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("SMTP_HOST", "")
	t.Setenv("ALERT_TO", "")
	if _, ok, err := FromEnv(); ok || err != nil {
		t.Fatalf("unset SMTP must be ok=false, got %v %v", ok, err)
	}
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("SMTP_USER", "me@example.com")
	t.Setenv("SMTP_PORT", "")
	t.Setenv("ALERT_TO", "a@example.com, b@example.com")
	cfg, ok, err := FromEnv()
	if !ok || err != nil || cfg.Port != 587 || cfg.From != "me@example.com" || len(cfg.To) != 2 {
		t.Fatalf("%+v %v %v", cfg, ok, err)
	}
}
