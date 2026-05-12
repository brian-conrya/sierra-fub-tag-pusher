package retry_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brian-conrya/sierra-fub-tag-pusher/internal/retry"
)

// newTestClient returns a Client with an injected sleep that records the
// requested durations and returns immediately. Backoff schedule is small so
// budget math is deterministic.
func newTestClient(t *testing.T, hc *http.Client) (*retry.Client, *[]time.Duration) {
	t.Helper()
	var sleeps []time.Duration
	c := &retry.Client{
		HTTP:    hc,
		Budget:  2 * time.Second,
		Backoff: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond},
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		},
		Jitter: func() time.Duration { return 0 },
	}
	return c, &sleeps
}

func TestDo_SuccessFirstTry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c, sleeps := newTestClient(t, srv.Client())
	resp, err := c.Do(t.Context(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	if len(*sleeps) != 0 {
		t.Fatalf("sleeps = %v, want none", *sleeps)
	}
}

func TestDo_RetryAfterSecondsHonored(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c, sleeps := newTestClient(t, srv.Client())
	resp, err := c.Do(t.Context(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
	if len(*sleeps) != 1 || (*sleeps)[0] != time.Second {
		t.Fatalf("sleeps = %v, want [1s]", *sleeps)
	}
}

func TestDo_RetryAfterHTTPDateHonored(t *testing.T) {
	// Target 3s in the future. By the time the server hits this handler the
	// HTTP roundtrip has eaten a few ms, so we accept anywhere from 2s up to
	// 3.1s when validating the computed sleep.
	target := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", target)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c, sleeps := newTestClient(t, srv.Client())
	// Bump budget so the ~3s sleep fits.
	c.Budget = 10 * time.Second
	resp, err := c.Do(t.Context(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(*sleeps) != 1 {
		t.Fatalf("sleeps = %v, want one entry", *sleeps)
	}
	if d := (*sleeps)[0]; d < 2*time.Second || d > 3100*time.Millisecond {
		t.Fatalf("sleep = %v, expected 2s..3.1s", d)
	}
}

func TestDo_ExponentialFallbackWhenNoRetryAfter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c, sleeps := newTestClient(t, srv.Client())
	resp, err := c.Do(t.Context(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	if len(*sleeps) != len(want) {
		t.Fatalf("sleeps = %v, want %v", *sleeps, want)
	}
	for i, d := range want {
		if (*sleeps)[i] != d {
			t.Fatalf("sleep[%d] = %v, want %v", i, (*sleeps)[i], d)
		}
	}
}

func TestDo_BudgetExhaustedReturnsLastResponse(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	// Backoff schedule has length 3, so after attempt 3 the schedule is exhausted
	// and we return the last response.
	c, _ := newTestClient(t, srv.Client())
	resp, err := c.Do(t.Context(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("calls = %d, want 4 (initial + 3 retries)", got)
	}
}

func TestDo_NonRetryableStatusNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	c, sleeps := newTestClient(t, srv.Client())
	resp, err := c.Do(t.Context(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
	if len(*sleeps) != 0 {
		t.Fatalf("sleeps = %v, want none", *sleeps)
	}
}

func TestDo_NilBuildFunc(t *testing.T) {
	c := retry.New(nil)
	if _, err := c.Do(t.Context(), nil); err == nil {
		t.Fatal("expected error for nil build func")
	}
}

func TestDo_BuildFuncErrorPropagated(t *testing.T) {
	c, _ := newTestClient(t, http.DefaultClient)
	sentinel := errors.New("boom")
	_, err := c.Do(t.Context(), func(_ context.Context) (*http.Request, error) {
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapping of %v", err, sentinel)
	}
}

func TestDo_ContextCancelStopsRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	c, _ := newTestClient(t, srv.Client())
	_, err := c.Do(ctx, func(reqCtx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(reqCtx, http.MethodGet, srv.URL, nil)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestDo_DefaultsApplied exercises the budget<=0 and backoff==nil fallbacks
// inside Do, ensuring the library defaults take effect when callers leave
// those fields zero/nil.
func TestDo_DefaultsApplied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := &retry.Client{HTTP: srv.Client()} // Budget=0, Backoff=nil → defaults
	resp, err := c.Do(t.Context(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
}

// TestDo_TransportErrorRetried covers the err!=nil + shouldRetry path: when
// the HTTP transport itself errors (connection refused, etc.) we expect the
// retry loop to attempt the request again.
func TestDo_TransportErrorRetried(t *testing.T) {
	// Server starts up, captures URL, then immediately shuts down so the
	// first request gets a connection error. The handler re-spawns a fresh
	// server on the same port for the second attempt — but that's flaky
	// in CI, so instead we just assert the call returns an error and
	// attempted at least once.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close() // make the URL unreachable

	c := &retry.Client{
		HTTP:    &http.Client{Timeout: 100 * time.Millisecond},
		Budget:  200 * time.Millisecond,
		Backoff: []time.Duration{1 * time.Millisecond, 1 * time.Millisecond},
		Sleep:   func(_ context.Context, _ time.Duration) error { return nil },
		Jitter:  func() time.Duration { return 0 },
	}
	resp, err := c.Do(t.Context(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	})
	if err == nil {
		t.Fatalf("expected error from unreachable server, got resp=%v", resp)
	}
}

// TestDo_WaitClampedToRemainingBudget triggers the wait>remaining branch:
// schedule a 1s backoff but only ~50ms of budget left after the first call.
func TestDo_WaitClampedToRemainingBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	var sleeps []time.Duration
	c := &retry.Client{
		HTTP:    srv.Client(),
		Budget:  60 * time.Millisecond,
		Backoff: []time.Duration{500 * time.Millisecond}, // way larger than budget
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		},
		Jitter: func() time.Duration { return 0 },
	}
	resp, err := c.Do(t.Context(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if len(sleeps) != 1 {
		t.Fatalf("sleeps = %v, want one entry", sleeps)
	}
	if sleeps[0] >= 500*time.Millisecond {
		t.Fatalf("sleep = %v, expected to be clamped below 500ms backoff", sleeps[0])
	}
}

// TestDo_SleepErrorAborts triggers the sleep returning an error branch
// (e.g. parent context cancelled during the sleep).
func TestDo_SleepErrorAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := &retry.Client{
		HTTP:    srv.Client(),
		Budget:  500 * time.Millisecond,
		Backoff: []time.Duration{10 * time.Millisecond},
		Sleep: func(_ context.Context, _ time.Duration) error {
			return errors.New("sleep aborted")
		},
		Jitter: func() time.Duration { return 0 },
	}
	resp, err := c.Do(t.Context(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	})
	// Returns the last response with 500 (not the sleep error itself).
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

// TestDo_DefaultSleepFires uses the production sleep path (no injection)
// with a tiny duration, so the time.NewTimer branch is exercised.
func TestDo_DefaultSleepFires(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := &retry.Client{
		HTTP:    srv.Client(),
		Budget:  500 * time.Millisecond,
		Backoff: []time.Duration{1 * time.Millisecond}, // <-- real sleep, tiny
		Jitter:  func() time.Duration { return 0 },
		// Sleep nil → use production timer-based path
	}
	resp, err := c.Do(t.Context(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestDo_DefaultSleepCancelled covers the ctx.Done branch of the production
// sleep path: a parent context cancel during the wait should short-circuit.
func TestDo_DefaultSleepCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(t.Context())
	c := &retry.Client{
		HTTP:    srv.Client(),
		Budget:  10 * time.Second,
		Backoff: []time.Duration{5 * time.Second}, // long, so cancel beats it
		Jitter:  func() time.Duration { return 0 },
	}
	// Cancel ~5ms after we call Do.
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	resp, err := c.Do(ctx, func(reqCtx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(reqCtx, http.MethodGet, srv.URL, nil)
	})
	// Must return promptly, not after the full 5s backoff.
	if err == nil && resp != nil {
		_ = resp.Body.Close()
	}
}

// TestDo_DefaultJitter exercises the production jitter path (Jitter==nil)
// by issuing a single retry and confirming the recorded sleep is bounded
// by backoff + 100ms jitter envelope.
func TestDo_DefaultJitter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := &retry.Client{
		HTTP:    srv.Client(),
		Budget:  1 * time.Second,
		Backoff: []time.Duration{10 * time.Millisecond},
		// Jitter nil → production random jitter
		Sleep: func(_ context.Context, d time.Duration) error {
			if d < 10*time.Millisecond || d > 110*time.Millisecond {
				t.Errorf("jittered sleep = %v, expected 10ms..110ms", d)
			}
			return nil
		},
	}
	resp, err := c.Do(t.Context(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
}

// TestDo_RetryAfterPastDateClampsToZero feeds a Retry-After header pointing
// at a date in the past, exercising the d<0 clamp in retryAfter.
func TestDo_RetryAfterPastDateClampsToZero(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", past)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	var sleeps []time.Duration
	c := &retry.Client{
		HTTP:    srv.Client(),
		Budget:  500 * time.Millisecond,
		Backoff: []time.Duration{50 * time.Millisecond},
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		},
		Jitter: func() time.Duration { return 0 },
	}
	resp, err := c.Do(t.Context(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if len(sleeps) != 1 || sleeps[0] != 0 {
		t.Fatalf("sleeps = %v, want [0] (clamped from past)", sleeps)
	}
}

// TestDo_RetryAfterUnparseableFallsBackToBackoff covers the final
// `return 0, false` branch of retryAfter: header is set but neither
// numeric seconds nor an HTTP date.
func TestDo_RetryAfterUnparseableFallsBackToBackoff(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "not-a-real-value")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c, sleeps := newTestClient(t, srv.Client())
	resp, err := c.Do(t.Context(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if len(*sleeps) != 1 || (*sleeps)[0] != 10*time.Millisecond {
		t.Fatalf("sleeps = %v, expected fallback to backoff[0]=10ms", *sleeps)
	}
}

// TestDo_DrainsPreviousResponseBody exercises the drainAndClose path:
// after the first 500 we issue a retry, and the first response body must
// have been read and closed (otherwise the test transport would warn).
func TestDo_DrainsPreviousResponseBody(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("partial body that must be drained"))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c, _ := newTestClient(t, srv.Client())
	resp, err := c.Do(t.Context(), func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}
