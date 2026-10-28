package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(Options{
		BaseURL:       srv.URL,
		Limiter:       NewLimiter(60000),
		ThrottlePause: 10 * time.Millisecond,
		BaseBackoff:   time.Millisecond,
		MaxRetries:    3,
	})
}

func TestEachPaginatesUntilShortPage(t *testing.T) {
	const total = 45
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		max, _ := strconv.Atoi(r.URL.Query().Get("max"))
		var data []GameBulk
		for i := off; i < off+max && i < total; i++ {
			data = append(data, GameBulk{ID: fmt.Sprint(i)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "pagination": map[string]int{"offset": off, "max": max, "size": len(data)}})
	}))
	var got []string
	err := Each[GameBulk](context.Background(), c, "/games", nil, 0, 20, func(_ int, items []GameBulk) (bool, error) {
		for _, g := range items {
			got = append(got, g.ID)
		}
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != total || got[0] != "0" || got[total-1] != "44" {
		t.Fatalf("got %d items: %v", len(got), got)
	}
}

func TestThrottleRetriesAndPauses(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(420)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"g1","names":{"international":"G"}}}`))
	}))
	g, err := c.GetGame(context.Background(), "g1")
	if err != nil {
		t.Fatal(err)
	}
	if g.ID != "g1" || calls.Load() != 3 {
		t.Fatalf("id=%s calls=%d", g.ID, calls.Load())
	}
	if s := c.Stats(); s.Throttled != 2 || s.Requests != 3 {
		t.Fatalf("stats %+v", s)
	}
}

func TestServerErrorsRetryThenGiveUp(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	_, err := c.GetGame(context.Background(), "x")
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusServiceUnavailable {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 4 { // first try + 3 retries
		t.Fatalf("calls = %d", calls.Load())
	}

	calls.Store(0)
	if _, err := c.GetGame(WithMaxRetries(context.Background(), 0), "x"); !errors.As(err, &he) {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("WithMaxRetries(0): calls = %d", calls.Load())
	}
}

func TestLeaderboardFilters(t *testing.T) {
	var got url.Values
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = w.Write([]byte(`{"data":{"runs":[]}}`))
	}))
	filter := url.Values{"var-v1": {"a"}, "platform": {"p1"}}
	if _, err := c.GetLeaderboard(context.Background(), "g", "c", "", filter); err != nil {
		t.Fatal(err)
	}
	if got.Get("var-v1") != "a" || got.Get("platform") != "p1" || got.Get("embed") != "players" {
		t.Fatalf("query %v", got)
	}
	if _, ok := filter["embed"]; ok {
		t.Fatal("caller filter was modified")
	}
}

func TestNotFoundAndPaginationCap(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/runs" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":400,"message":"Invalid pagination values."}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":404,"message":"not found"}`))
	}))
	if _, err := c.GetGame(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := Page[Run](context.Background(), c, "/runs", nil, 10000, 200); !errors.Is(err, ErrPaginationCap) {
		t.Fatalf("want ErrPaginationCap, got %v", err)
	}
}

func TestRunPlayersBothForms(t *testing.T) {
	plain := `{"id":"r1","players":[{"rel":"user","id":"u1","uri":"x"},{"rel":"guest","name":"Bob","uri":"y"}]}`
	embedded := `{"id":"r1","players":{"data":[{"rel":"user","id":"u1","names":{"international":"Ann"},"role":"user","location":{"country":{"code":"br","names":{"international":"Brazil"}}}},{"rel":"guest","name":"Bob","links":[]}]}}`
	for name, src := range map[string]string{"plain": plain, "embedded": embedded} {
		var r Run
		if err := json.Unmarshal([]byte(src), &r); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(r.Players) != 2 || r.Players[0].ID != "u1" || r.Players[1].Name != "Bob" || r.Players[1].Rel != "guest" {
			t.Fatalf("%s: players %+v", name, r.Players)
		}
		if name == "embedded" && (r.Players[0].User == nil || r.Players[0].User.Location.Country.Code != "br") {
			t.Fatalf("embedded user not decoded: %+v", r.Players[0])
		}
		if name == "plain" && r.Players[0].User != nil {
			t.Fatalf("plain form should not carry a user")
		}
	}
}

func TestEmbedListBothForms(t *testing.T) {
	var g Game
	src := `{"id":"g","platforms":{"data":[{"id":"p1","name":"N64","released":1996}]},"regions":["r1","r2"],"links":[{"rel":"series","uri":"https://x/api/v1/series/s9"}]}`
	if err := json.Unmarshal([]byte(src), &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Platforms.Items) != 1 || g.Platforms.IDs[0] != "p1" || *g.Platforms.Items[0].Released != 1996 {
		t.Fatalf("platforms %+v", g.Platforms)
	}
	if len(g.Regions.IDs) != 2 || len(g.Regions.Items) != 0 {
		t.Fatalf("regions %+v", g.Regions)
	}
	if s := g.SeriesIDs(); len(s) != 1 || s[0] != "s9" {
		t.Fatalf("series %v", s)
	}
}

func TestLimiterPauseBlocks(t *testing.T) {
	l := NewLimiter(60000)
	l.Pause(50 * time.Millisecond)
	start := time.Now()
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("Wait returned before the pause ended")
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.Pause(time.Hour)
	cancel()
	if err := l.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
}
