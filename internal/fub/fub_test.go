package fub_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brian-conrya/sierra-fub-tag-pusher/internal/fub"
	"github.com/brian-conrya/sierra-fub-tag-pusher/internal/retry"
)

func testRetry(httpClient *http.Client) *retry.Client {
	return &retry.Client{
		HTTP:    httpClient,
		Budget:  500 * time.Millisecond,
		Backoff: []time.Duration{1 * time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond},
		Sleep:   func(_ context.Context, _ time.Duration) error { return nil },
		Jitter:  func() time.Duration { return 0 },
	}
}

func TestFindByEmail_Success(t *testing.T) {
	var gotPath string
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"people":[{"id":42,"tags":["X"]}]}`)
	}))
	t.Cleanup(srv.Close)

	c := fub.New("fub-key", testRetry(srv.Client()))
	c.BaseURL = srv.URL
	p, err := c.FindByEmail(t.Context(), "user@example.com")
	if err != nil {
		t.Fatalf("FindByEmail: %v", err)
	}
	if p.ID != 42 {
		t.Errorf("id = %d, want 42", p.ID)
	}
	if !strings.Contains(gotPath, "email=user%40example.com") {
		t.Errorf("path = %q, missing email query", gotPath)
	}
	if !strings.Contains(gotPath, "includeTrash=true") {
		t.Errorf("path = %q, missing includeTrash=true", gotPath)
	}
	if !strings.Contains(gotPath, "sort=id") {
		t.Errorf("path = %q, missing sort=id", gotPath)
	}
	if !strings.Contains(gotPath, "limit=1") {
		t.Errorf("path = %q, missing limit=1", gotPath)
	}
	// Basic auth: "fub-key:" base64.
	if gotAuth == "" || !strings.HasPrefix(gotAuth, "Basic ") {
		t.Errorf("auth = %q, want Basic header", gotAuth)
	}
}

// Multi-match resolution is delegated to FUB (sort=id&limit=1). The client
// just takes the single returned record; this test pins that contract.
func TestFindByEmail_TakesFirstReturnedRecord(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sort") != "id" || r.URL.Query().Get("limit") != "1" {
			t.Errorf("query = %q, missing sort=id and limit=1", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"people":[{"id":7}]}`)
	}))
	t.Cleanup(srv.Close)

	c := fub.New("k", testRetry(srv.Client()))
	c.BaseURL = srv.URL
	p, err := c.FindByEmail(t.Context(), "shared@example.com")
	if err != nil {
		t.Fatalf("FindByEmail: %v", err)
	}
	if p.ID != 7 {
		t.Errorf("id = %d, want 7", p.ID)
	}
}

func TestFindByEmail_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"people":[]}`)
	}))
	t.Cleanup(srv.Close)

	c := fub.New("k", testRetry(srv.Client()))
	c.BaseURL = srv.URL
	_, err := c.FindByEmail(t.Context(), "nobody@example.com")
	if !errors.Is(err, fub.ErrPersonNotFound) {
		t.Fatalf("err = %v, want ErrPersonNotFound", err)
	}
}

func TestFindByEmail_RateLimitRecovery(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"people":[{"id":1}]}`)
	}))
	t.Cleanup(srv.Close)

	c := fub.New("k", testRetry(srv.Client()))
	c.BaseURL = srv.URL
	p, err := c.FindByEmail(t.Context(), "a@b.com")
	if err != nil {
		t.Fatalf("FindByEmail: %v", err)
	}
	if p.ID != 1 {
		t.Errorf("id = %d, want 1", p.ID)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestFindByEmail_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `not-json`)
	}))
	t.Cleanup(srv.Close)

	c := fub.New("k", testRetry(srv.Client()))
	c.BaseURL = srv.URL
	_, err := c.FindByEmail(t.Context(), "a@b.com")
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %v, want decode error", err)
	}
}

func TestFindByEmail_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := fub.New("k", testRetry(http.DefaultClient))
	c.BaseURL = url
	_, err := c.FindByEmail(t.Context(), "a@b.com")
	if err == nil {
		t.Fatal("expected transport error")
	}
}

func TestFindByEmail_TerminalFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	c := fub.New("k", testRetry(srv.Client()))
	c.BaseURL = srv.URL
	_, err := c.FindByEmail(t.Context(), "a@b.com")
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want 503-containing error", err)
	}
}

func TestMergeTag_SendsMergeTagsTrueAndCorrectBody(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)

	c := fub.New("k", testRetry(srv.Client()))
	c.BaseURL = srv.URL
	if err := c.MergeTag(t.Context(), 42, "HotLead"); err != nil {
		t.Fatalf("MergeTag: %v", err)
	}
	if gotPath != "/v1/people/42?mergeTags=true" {
		t.Errorf("path = %q, want /v1/people/42?mergeTags=true", gotPath)
	}
	tags, ok := gotBody["tags"].([]any)
	if !ok || len(tags) != 1 || tags[0] != "HotLead" {
		t.Errorf("body tags = %v, want [HotLead]", gotBody["tags"])
	}
}

func TestMergeTag_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := fub.New("k", testRetry(http.DefaultClient))
	c.BaseURL = url
	if err := c.MergeTag(t.Context(), 1, "T"); err == nil {
		t.Fatal("expected transport error")
	}
}

func TestMergeTag_NonOKError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `bad payload`)
	}))
	t.Cleanup(srv.Close)

	c := fub.New("k", testRetry(srv.Client()))
	c.BaseURL = srv.URL
	err := c.MergeTag(t.Context(), 1, "T")
	if err == nil || !strings.Contains(err.Error(), "422") {
		t.Fatalf("err = %v, want 422-containing error", err)
	}
}
