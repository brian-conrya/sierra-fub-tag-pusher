package sierra_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brian-conrya/sierra-fub-tag-pusher/internal/retry"
	"github.com/brian-conrya/sierra-fub-tag-pusher/internal/sierra"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sierra", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func TestWebhookEvent_Validate(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		wantErr bool
	}{
		{"valid single lead", "valid_single_lead.json", false},
		{"valid multi lead", "valid_multi_lead.json", false},
		{"wrong event type", "wrong_event_type.json", true},
		{"empty resource list", "empty_resource_list.json", true},
		{"missing tag", "missing_tag.json", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := loadFixture(t, tc.fixture)
			var ev sierra.WebhookEvent
			if err := json.Unmarshal(raw, &ev); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			err := ev.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func testRetry(httpClient *http.Client) *retry.Client {
	return &retry.Client{
		HTTP:    httpClient,
		Budget:  500 * time.Millisecond,
		Backoff: []time.Duration{1 * time.Millisecond, 2 * time.Millisecond},
		Sleep:   func(_ context.Context, _ time.Duration) error { return nil },
		Jitter:  func() time.Duration { return 0 },
	}
}

func TestClient_GetLead_Success(t *testing.T) {
	var gotPath, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotAPIKey = r.Header.Get("Sierra-ApiKey")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"data":{"id":345678,"email":"johndoe@server.com"}}`)
	}))
	t.Cleanup(srv.Close)

	c := sierra.New("secret-key", testRetry(srv.Client()))
	c.BaseURL = srv.URL
	lead, err := c.GetLead(t.Context(), 345678)
	if err != nil {
		t.Fatalf("GetLead: %v", err)
	}
	if lead.Email != "johndoe@server.com" {
		t.Errorf("email = %q, want johndoe@server.com", lead.Email)
	}
	if lead.ID != 345678 {
		t.Errorf("id = %d, want 345678", lead.ID)
	}
	if gotPath != "/leads/get/345678?includeTags=true" {
		t.Errorf("path = %q, want /leads/get/345678?includeTags=true", gotPath)
	}
	if gotAPIKey != "secret-key" {
		t.Errorf("Sierra-ApiKey = %q", gotAPIKey)
	}
}

func TestClient_GetLead_SuccessFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"success":false}`)
	}))
	t.Cleanup(srv.Close)
	c := sierra.New("k", testRetry(srv.Client()))
	c.BaseURL = srv.URL
	if _, err := c.GetLead(t.Context(), 1); err == nil {
		t.Fatal("expected error for success=false")
	}
}

func TestClient_GetLead_RetriesOn5xx(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"success":true,"data":{"id":1,"email":"a@b.com"}}`)
	}))
	t.Cleanup(srv.Close)
	c := sierra.New("k", testRetry(srv.Client()))
	c.BaseURL = srv.URL
	if _, err := c.GetLead(t.Context(), 1); err != nil {
		t.Fatalf("GetLead: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestClient_GetLead_NonOKError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `unauthorized`)
	}))
	t.Cleanup(srv.Close)
	c := sierra.New("k", testRetry(srv.Client()))
	c.BaseURL = srv.URL
	_, err := c.GetLead(t.Context(), 1)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want 401-containing error", err)
	}
}

// Sierra returns success=true with a "data" block that has no id field.
// The client should fall back to the requested id so callers don't get a
// zero-valued Lead.
func TestClient_GetLead_PopulatesIDWhenAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"success":true,"data":{"email":"x@y.com"}}`)
	}))
	t.Cleanup(srv.Close)

	c := sierra.New("k", testRetry(srv.Client()))
	c.BaseURL = srv.URL
	lead, err := c.GetLead(t.Context(), 12345)
	if err != nil {
		t.Fatalf("GetLead: %v", err)
	}
	if lead.ID != 12345 {
		t.Errorf("id = %d, want 12345 (fallback to requested id)", lead.ID)
	}
}

func TestClient_GetLead_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `not-json`)
	}))
	t.Cleanup(srv.Close)

	c := sierra.New("k", testRetry(srv.Client()))
	c.BaseURL = srv.URL
	_, err := c.GetLead(t.Context(), 1)
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %v, want decode error", err)
	}
}

// Transport error path: client points at a closed server so the underlying
// HTTP roundtrip fails. Forces retry.Do to return its wrapped error.
func TestClient_GetLead_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := sierra.New("k", testRetry(http.DefaultClient))
	c.BaseURL = url
	_, err := c.GetLead(t.Context(), 1)
	if err == nil {
		t.Fatal("expected transport error")
	}
}

func TestClient_AddWebhook(t *testing.T) {
	var body sierra.SubscriptionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/webhook" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		_, _ = io.WriteString(w, `{"success":true,"data":{"id":42,"eventTypes":["LeadTagAdded"],"url":"https://example.com/webhook"}}`)
	}))
	t.Cleanup(srv.Close)

	c := sierra.New("k", testRetry(srv.Client()))
	c.BaseURL = srv.URL
	got, err := c.AddWebhook(t.Context(), sierra.SubscriptionRequest{
		EventTypes: []string{sierra.EventLeadTagAdded},
		WebhookURL: "https://example.com/webhook",
	})
	if err != nil {
		t.Fatalf("AddWebhook: %v", err)
	}
	if got.Data.ID != 42 {
		t.Errorf("id = %d, want 42", got.Data.ID)
	}
	if body.WebhookURL != "https://example.com/webhook" {
		t.Errorf("url field = %q", body.WebhookURL)
	}
	if len(body.EventTypes) != 1 || body.EventTypes[0] != sierra.EventLeadTagAdded {
		t.Errorf("eventTypes = %v", body.EventTypes)
	}
}

func TestClient_AddWebhook_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `bad`)
	}))
	t.Cleanup(srv.Close)

	c := sierra.New("k", testRetry(srv.Client()))
	c.BaseURL = srv.URL
	_, err := c.AddWebhook(t.Context(), sierra.SubscriptionRequest{
		EventTypes: []string{sierra.EventLeadTagAdded},
		WebhookURL: "https://example.com/webhook",
	})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("err = %v, want 400-containing error", err)
	}
}

func TestClient_AddWebhook_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `not-json`)
	}))
	t.Cleanup(srv.Close)

	c := sierra.New("k", testRetry(srv.Client()))
	c.BaseURL = srv.URL
	_, err := c.AddWebhook(t.Context(), sierra.SubscriptionRequest{EventTypes: []string{"x"}})
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %v, want decode error", err)
	}
}

func TestClient_AddWebhook_SuccessFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"success":false}`)
	}))
	t.Cleanup(srv.Close)

	c := sierra.New("k", testRetry(srv.Client()))
	c.BaseURL = srv.URL
	_, err := c.AddWebhook(t.Context(), sierra.SubscriptionRequest{EventTypes: []string{"x"}})
	if err == nil || !strings.Contains(err.Error(), "success=false") {
		t.Fatalf("err = %v, want success=false error", err)
	}
}

func TestClient_AddWebhook_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := sierra.New("k", testRetry(http.DefaultClient))
	c.BaseURL = url
	_, err := c.AddWebhook(t.Context(), sierra.SubscriptionRequest{EventTypes: []string{"x"}})
	if err == nil {
		t.Fatal("expected transport error")
	}
}
