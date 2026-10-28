// Package api is a client for the speedrun.com REST API v1.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const DefaultBaseURL = "https://www.speedrun.com/api/v1"

// MaxOffset is the highest offset+max the API accepts on capped collections
// such as /runs. Requests beyond it fail with "Invalid pagination values".
const MaxOffset = 10000

var (
	ErrNotFound      = errors.New("api: not found")
	ErrPaginationCap = errors.New("api: pagination offset limit reached")
)

// HTTPError is returned for non-retryable error responses.
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("api: HTTP %d: %s", e.Status, e.Message)
}

// Stats holds cumulative request counters.
type Stats struct {
	Requests  int64
	Throttled int64
	Errors    int64
}

type Options struct {
	BaseURL       string
	UserAgent     string
	Limiter       *Limiter
	HTTPClient    *http.Client
	Logger        *slog.Logger
	MaxRetries    int           // retries for 5xx and network errors
	MaxThrottled  int           // retries for 420/429 responses
	ThrottlePause time.Duration // global pause after a 420
	BaseBackoff   time.Duration
	MaxOffset     int
}

type Client struct {
	opt Options

	requests  atomic.Int64
	throttled atomic.Int64
	errors    atomic.Int64
}

func New(opt Options) *Client {
	if opt.BaseURL == "" {
		opt.BaseURL = DefaultBaseURL
	}
	opt.BaseURL = strings.TrimRight(opt.BaseURL, "/")
	if opt.UserAgent == "" {
		opt.UserAgent = "speedscraper/0.1"
	}
	if opt.Limiter == nil {
		opt.Limiter = NewLimiter(90)
	}
	if opt.HTTPClient == nil {
		opt.HTTPClient = &http.Client{Timeout: 90 * time.Second}
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.MaxRetries == 0 {
		opt.MaxRetries = 5
	}
	if opt.MaxThrottled == 0 {
		opt.MaxThrottled = 30
	}
	if opt.ThrottlePause == 0 {
		opt.ThrottlePause = 60 * time.Second
	}
	if opt.BaseBackoff == 0 {
		opt.BaseBackoff = 2 * time.Second
	}
	if opt.MaxOffset == 0 {
		opt.MaxOffset = MaxOffset
	}
	return &Client{opt: opt}
}

func (c *Client) Stats() Stats {
	return Stats{
		Requests:  c.requests.Load(),
		Throttled: c.throttled.Load(),
		Errors:    c.errors.Load(),
	}
}

// MaxOffset reports the pagination cap of capped collections.
func (c *Client) MaxOffset() int { return c.opt.MaxOffset }

// Get fetches path with query q and decodes the JSON body into out.
func (c *Client) Get(ctx context.Context, path string, q url.Values, out any) error {
	body, err := c.getRaw(ctx, path, q)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("api: decode %s: %w", path, err)
	}
	return nil
}

type maxRetriesKey struct{}

// WithMaxRetries overrides, for requests made with the returned context, how
// many times 5xx responses and network errors are retried.
func WithMaxRetries(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, maxRetriesKey{}, n)
}

func (c *Client) getRaw(ctx context.Context, path string, q url.Values) ([]byte, error) {
	maxRetries := c.opt.MaxRetries
	if n, ok := ctx.Value(maxRetriesKey{}).(int); ok {
		maxRetries = n
	}
	u := c.opt.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	retries, throttles := 0, 0
	for {
		if err := c.opt.Limiter.Wait(ctx); err != nil {
			return nil, err
		}
		status, body, err := c.do(ctx, u)
		c.requests.Add(1)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			c.errors.Add(1)
		case status == http.StatusOK:
			return body, nil
		case status == 420 || status == http.StatusTooManyRequests:
			c.throttled.Add(1)
			throttles++
			if throttles > c.opt.MaxThrottled {
				return nil, &HTTPError{Status: status, Message: "rate limited too many times"}
			}
			c.opt.Logger.Warn("rate limited, pausing all requests", "url", u, "pause", c.opt.ThrottlePause)
			c.opt.Limiter.Pause(c.opt.ThrottlePause)
			continue
		case status == http.StatusNotFound:
			return nil, ErrNotFound
		case status >= 400 && status < 500:
			msg := errorMessage(body)
			if status == http.StatusBadRequest && strings.Contains(strings.ToLower(msg), "pagination") {
				return nil, ErrPaginationCap
			}
			c.errors.Add(1)
			return nil, &HTTPError{Status: status, Message: msg}
		default:
			c.errors.Add(1)
			err = &HTTPError{Status: status, Message: errorMessage(body)}
		}
		retries++
		if retries > maxRetries {
			return nil, fmt.Errorf("api: GET %s: giving up after %d retries: %w", u, maxRetries, err)
		}
		wait := backoff(c.opt.BaseBackoff, retries)
		c.opt.Logger.Warn("request failed, retrying", "url", u, "err", err, "retry", retries, "wait", wait)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

func (c *Client) do(ctx context.Context, u string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", c.opt.UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.opt.HTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}

// backoff returns an exponential delay with full jitter for the given attempt.
func backoff(base time.Duration, attempt int) time.Duration {
	d := base << min(attempt-1, 6)
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

func errorMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		return e.Message
	}
	if len(body) > 200 {
		body = body[:200]
	}
	return strconv.Quote(string(body))
}
