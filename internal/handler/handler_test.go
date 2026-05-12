package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/brian-conrya/sierra-fub-tag-pusher/internal/fub"
	"github.com/brian-conrya/sierra-fub-tag-pusher/internal/handler"
	"github.com/brian-conrya/sierra-fub-tag-pusher/internal/sierra"
)

// fakeSierra and fakeFUB stub the upstream clients so handler tests stay in
// process and assert exactly which calls were made.

type sierraCall struct {
	id int64
}

type fakeSierra struct {
	mu    sync.Mutex
	calls []sierraCall
	leads map[int64]sierra.Lead
	errs  map[int64]error
}

func (f *fakeSierra) GetLead(_ context.Context, id int64) (sierra.Lead, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sierraCall{id: id})
	if err, ok := f.errs[id]; ok {
		return sierra.Lead{}, err
	}
	if l, ok := f.leads[id]; ok {
		return l, nil
	}
	return sierra.Lead{ID: id}, nil
}

type fubFind struct {
	email string
}
type fubMerge struct {
	personID int64
	tag      string
}

type fakeFUB struct {
	mu     sync.Mutex
	finds  []fubFind
	merges []fubMerge

	findByEmail   map[string]fub.Person
	findErr       error
	notFoundEmail string
	mergeErr      error
}

func (f *fakeFUB) FindByEmail(_ context.Context, email string) (fub.Person, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finds = append(f.finds, fubFind{email: email})
	if f.findErr != nil {
		return fub.Person{}, f.findErr
	}
	if email == f.notFoundEmail {
		return fub.Person{}, fub.ErrPersonNotFound
	}
	if p, ok := f.findByEmail[email]; ok {
		return p, nil
	}
	return fub.Person{ID: 999}, nil
}

func (f *fakeFUB) MergeTag(_ context.Context, personID int64, tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merges = append(f.merges, fubMerge{personID: personID, tag: tag})
	return f.mergeErr
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sierra", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func newTestHandler(t *testing.T, s handler.SierraClient, f handler.FUBClient) (*handler.Handler, *bytes.Buffer) {
	t.Helper()
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &handler.Handler{
		Sierra: s,
		FUB:    f,
		Logger: logger,
	}, logBuf
}

func doPost(t *testing.T, h *handler.Handler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.Routes(mux)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func logEvents(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("parse log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestHappyPath(t *testing.T) {
	s := &fakeSierra{leads: map[int64]sierra.Lead{345678: {ID: 345678, Email: "john@example.com"}}}
	f := &fakeFUB{findByEmail: map[string]fub.Person{"john@example.com": {ID: 42}}}
	h, logs := newTestHandler(t, s, f)

	rec := doPost(t, h, loadFixture(t, "valid_single_lead.json"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(f.merges) != 1 {
		t.Fatalf("merges = %+v, want one entry", f.merges)
	}
	if f.merges[0] != (fubMerge{personID: 42, tag: "HotLead"}) {
		t.Errorf("merge = %+v, want {42 HotLead}", f.merges[0])
	}
	events := logEvents(t, logs)
	if len(events) == 0 || events[len(events)-1]["msg"] != "tag_synced" {
		t.Errorf("expected tag_synced log, got %+v", events)
	}
	// Email is logged in the clear.
	if events[len(events)-1]["email"] != "john@example.com" {
		t.Errorf("expected email field, got %+v", events[len(events)-1])
	}
}

func TestPhantomLead(t *testing.T) {
	s := &fakeSierra{leads: map[int64]sierra.Lead{345678: {ID: 345678, Email: "ghost@example.com"}}}
	f := &fakeFUB{notFoundEmail: "ghost@example.com"}
	h, logs := newTestHandler(t, s, f)

	rec := doPost(t, h, loadFixture(t, "valid_single_lead.json"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(f.merges) != 0 {
		t.Fatalf("expected no merges, got %+v", f.merges)
	}
	events := logEvents(t, logs)
	found := false
	for _, e := range events {
		if e["msg"] == "fub_person_not_found" {
			found = true
			if int64(e["sierra_lead_id"].(float64)) != 345678 {
				t.Errorf("sierra_lead_id = %v, want 345678", e["sierra_lead_id"])
			}
		}
	}
	if !found {
		t.Errorf("expected fub_person_not_found log, got %+v", events)
	}
}

func TestTerminalFUBFailure(t *testing.T) {
	s := &fakeSierra{leads: map[int64]sierra.Lead{345678: {ID: 345678, Email: "x@example.com"}}}
	f := &fakeFUB{findErr: errors.New("503 service unavailable")}
	h, logs := newTestHandler(t, s, f)

	rec := doPost(t, h, loadFixture(t, "valid_single_lead.json"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	events := logEvents(t, logs)
	found := false
	for _, e := range events {
		if e["msg"] == "fub_find_failed" {
			found = true
			if e["level"] != "ERROR" {
				t.Errorf("level = %v, want ERROR", e["level"])
			}
			if e["sierra_lead_id"] == nil {
				t.Errorf("missing sierra_lead_id in log: %+v", e)
			}
		}
	}
	if !found {
		t.Errorf("expected fub_find_failed log, got %+v", events)
	}
}

func TestMalformedJSON_Returns400(t *testing.T) {
	s := &fakeSierra{}
	f := &fakeFUB{}
	h, _ := newTestHandler(t, s, f)
	rec := doPost(t, h, []byte(`{not-json`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestWrongEventType_Returns400(t *testing.T) {
	s := &fakeSierra{}
	f := &fakeFUB{}
	h, _ := newTestHandler(t, s, f)
	rec := doPost(t, h, loadFixture(t, "wrong_event_type.json"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestEmptyResourceList_Returns400(t *testing.T) {
	s := &fakeSierra{}
	f := &fakeFUB{}
	h, _ := newTestHandler(t, s, f)
	rec := doPost(t, h, loadFixture(t, "empty_resource_list.json"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestMissingTag_Returns400(t *testing.T) {
	s := &fakeSierra{}
	f := &fakeFUB{}
	h, _ := newTestHandler(t, s, f)
	rec := doPost(t, h, loadFixture(t, "missing_tag.json"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestMultiLead_AllProcessed(t *testing.T) {
	s := &fakeSierra{leads: map[int64]sierra.Lead{
		345678: {ID: 345678, Email: "a@example.com"},
		345679: {ID: 345679, Email: "b@example.com"},
		345680: {ID: 345680, Email: "c@example.com"},
	}}
	f := &fakeFUB{findByEmail: map[string]fub.Person{
		"a@example.com": {ID: 1},
		"b@example.com": {ID: 2},
		"c@example.com": {ID: 3},
	}}
	h, _ := newTestHandler(t, s, f)
	rec := doPost(t, h, loadFixture(t, "valid_multi_lead.json"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(f.merges) != 3 {
		t.Fatalf("merges = %+v, want 3", f.merges)
	}
}

func TestSierraGetFails_ContinuesNextLead(t *testing.T) {
	s := &fakeSierra{
		errs:  map[int64]error{345678: errors.New("503")},
		leads: map[int64]sierra.Lead{345679: {ID: 345679, Email: "b@example.com"}, 345680: {ID: 345680, Email: "c@example.com"}},
	}
	f := &fakeFUB{findByEmail: map[string]fub.Person{"b@example.com": {ID: 2}, "c@example.com": {ID: 3}}}
	h, _ := newTestHandler(t, s, f)
	rec := doPost(t, h, loadFixture(t, "valid_multi_lead.json"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(f.merges) != 2 {
		t.Errorf("merges = %d, want 2 (skipping failed lead)", len(f.merges))
	}
}

func TestLeadWithoutEmail_Skipped(t *testing.T) {
	s := &fakeSierra{leads: map[int64]sierra.Lead{345678: {ID: 345678, Email: ""}}}
	f := &fakeFUB{}
	h, logs := newTestHandler(t, s, f)
	rec := doPost(t, h, loadFixture(t, "valid_single_lead.json"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(f.finds) != 0 {
		t.Errorf("expected no FUB find, got %+v", f.finds)
	}
	events := logEvents(t, logs)
	found := false
	for _, e := range events {
		if e["msg"] == "sierra_lead_missing_email" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected sierra_lead_missing_email log")
	}
}

func TestHealthz(t *testing.T) {
	h, _ := newTestHandler(t, &fakeSierra{}, &fakeFUB{})
	mux := http.NewServeMux()
	h.Routes(mux)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// errorReader returns an error mid-read; used to drive the io.ReadAll
// failure branch in handleWebhook.
type errorReader struct{}

func (errorReader) Read(_ []byte) (int, error) { return 0, errors.New("read failed") }

func TestReadBodyError_Returns400(t *testing.T) {
	h, _ := newTestHandler(t, &fakeSierra{}, &fakeFUB{})
	mux := http.NewServeMux()
	h.Routes(mux)
	req := httptest.NewRequest(http.MethodPost, "/webhook", errorReader{})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestMergeTagFailure_LogsAndReturns200(t *testing.T) {
	s := &fakeSierra{leads: map[int64]sierra.Lead{345678: {ID: 345678, Email: "x@example.com"}}}
	f := &fakeFUB{
		findByEmail: map[string]fub.Person{"x@example.com": {ID: 42}},
		mergeErr:    errors.New("503 service unavailable"),
	}
	h, logs := newTestHandler(t, s, f)

	rec := doPost(t, h, loadFixture(t, "valid_single_lead.json"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	events := logEvents(t, logs)
	found := false
	for _, e := range events {
		if e["msg"] == "fub_merge_tag_failed" {
			found = true
			if e["level"] != "ERROR" {
				t.Errorf("level = %v, want ERROR", e["level"])
			}
			if int64(e["fub_person_id"].(float64)) != 42 {
				t.Errorf("fub_person_id = %v, want 42", e["fub_person_id"])
			}
			if int64(e["sierra_lead_id"].(float64)) != 345678 {
				t.Errorf("sierra_lead_id = %v, want 345678", e["sierra_lead_id"])
			}
		}
	}
	if !found {
		t.Errorf("expected fub_merge_tag_failed log, got %+v", events)
	}
}

// Ensure that even a maliciously huge body is bounded.
func TestOversizedBody_HandledGracefully(t *testing.T) {
	h, _ := newTestHandler(t, &fakeSierra{}, &fakeFUB{})
	mux := http.NewServeMux()
	h.Routes(mux)
	big := bytes.Repeat([]byte("a"), 2<<20) // 2 MiB
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(big))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (truncated/invalid)", rec.Code)
	}
	_, _ = io.Copy(io.Discard, rec.Body)
}
