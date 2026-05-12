// Package retry executes HTTP calls with retry on 429 and 5xx responses.
//
// It honors the Retry-After header when present and falls back to capped
// exponential backoff with jitter. A total time budget bounds the whole
// operation so a single inbound webhook handler can guarantee a timely
// response regardless of downstream behavior.
package retry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// DefaultBackoff is the fallback delay schedule when no Retry-After is given.
var DefaultBackoff = []time.Duration{
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
}

// DefaultBudget caps the total time spent inside a single Do call.
const DefaultBudget = 8 * time.Second

// Client wraps an *http.Client with retry behavior.
type Client struct {
	HTTP    *http.Client
	Budget  time.Duration
	Backoff []time.Duration

	// Sleep is injectable for tests. If nil, a context-aware sleep is used.
	Sleep func(ctx context.Context, d time.Duration) error
	// Jitter returns an additive jitter for backoff steps. If nil, uses 0–100ms.
	Jitter func() time.Duration
}

// New returns a Client populated with library defaults.
func New(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &Client{
		HTTP:    httpClient,
		Budget:  DefaultBudget,
		Backoff: DefaultBackoff,
	}
}

// BuildFunc constructs a fresh *http.Request for each attempt. Required
// because *http.Request bodies are single-use after the first Do.
type BuildFunc func(ctx context.Context) (*http.Request, error)

// Do executes build->HTTP.Do, retrying on 429 and 5xx until the call succeeds,
// the budget is exhausted, or the context is cancelled. The returned response
// is the last attempt; callers must close its Body.
func (c *Client) Do(ctx context.Context, build BuildFunc) (*http.Response, error) {
	if build == nil {
		return nil, errors.New("retry: nil build func")
	}
	budget := c.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}
	backoff := c.Backoff
	if backoff == nil {
		backoff = DefaultBackoff
	}

	deadlineCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	var (
		lastResp *http.Response
		lastErr  error
		attempt  int
	)
	for {
		if err := deadlineCtx.Err(); err != nil {
			if lastErr == nil {
				lastErr = err
			}
			return lastResp, lastErr
		}

		req, err := build(deadlineCtx)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}

		resp, err := c.HTTP.Do(req)
		if err == nil && !shouldRetry(resp.StatusCode) {
			return resp, nil
		}

		// Drain and close before retry to allow connection reuse.
		if lastResp != nil {
			drainAndClose(lastResp.Body)
		}
		lastResp = resp
		lastErr = err

		wait, ok := nextDelay(resp, attempt, backoff, c.jitter())
		attempt++
		if !ok {
			return lastResp, lastErr
		}
		// Cap wait at remaining budget. deadlineCtx always has a deadline
		// (we set it above via context.WithTimeout).
		deadline, _ := deadlineCtx.Deadline()
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return lastResp, lastErr
		}
		if wait > remaining {
			wait = remaining
		}
		if err := c.sleep(deadlineCtx, wait); err != nil {
			return lastResp, lastErr
		}
	}
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) jitter() time.Duration {
	if c.Jitter != nil {
		return c.Jitter()
	}
	return time.Duration(rand.Int64N(int64(100 * time.Millisecond)))
}

func shouldRetry(status int) bool {
	return status == http.StatusTooManyRequests || (status >= 500 && status <= 599)
}

func nextDelay(resp *http.Response, attempt int, backoff []time.Duration, jitter time.Duration) (time.Duration, bool) {
	if d, ok := retryAfter(resp); ok {
		return d, true
	}
	if attempt >= len(backoff) {
		return 0, false
	}
	return backoff[attempt] + jitter, true
}

func retryAfter(resp *http.Response) (time.Duration, bool) {
	if resp == nil {
		return 0, false
	}
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// drainAndClose reads any remaining bytes off body and closes it so the
// HTTP transport can reuse the connection. Body is the caller's
// responsibility to ensure non-nil.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}
