// Package handler is the HTTP entry point. It validates inbound Sierra
// webhook payloads, fans out to Sierra for lead lookup and to FUB for the
// tag merge, and emits structured logs.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/brian-conrya/sierra-fub-tag-pusher/internal/fub"
	"github.com/brian-conrya/sierra-fub-tag-pusher/internal/sierra"
)

// SierraClient is the subset of *sierra.Client the handler needs.
type SierraClient interface {
	GetLead(ctx context.Context, id int64) (sierra.Lead, error)
}

// FUBClient is the subset of *fub.Client the handler needs.
type FUBClient interface {
	FindByEmail(ctx context.Context, email string) (fub.Person, error)
	MergeTag(ctx context.Context, personID int64, tag string) error
}

// Handler holds shared dependencies.
type Handler struct {
	Sierra SierraClient
	FUB    FUBClient
	Logger *slog.Logger
}

// Routes wires the HTTP routes onto a mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /webhook", h.handleWebhook)
	mux.HandleFunc("GET /healthz", h.handleHealth)
}

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

// maxBodyBytes caps the inbound webhook payload size. Real Sierra
// LeadTagAdded envelopes are a few hundred bytes (a small JSON object with
// a list of int lead IDs and a short data block). 1 MiB is ~1000× the
// largest plausible real payload — generous headroom for unexpected fields
// and large resourceList arrays, while still preventing an attacker from
// streaming an unbounded body into memory.
const maxBodyBytes = 1 << 20 // 1 MiB

func (h *Handler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	var ev sierra.WebhookEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if err := ev.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	for _, leadID := range ev.ResourceList {
		h.processLead(r.Context(), leadID, ev.Data.Tag)
	}

	// Always 200 on a well-formed request: terminal downstream failures are
	// logged but masked from Sierra to avoid webhook quarantine.
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) processLead(ctx context.Context, leadID int64, tag string) {
	log := h.Logger.With(slog.Int64("sierra_lead_id", leadID), slog.String("tag", tag))

	lead, err := h.Sierra.GetLead(ctx, leadID)
	if err != nil {
		log.Error("sierra_lead_fetch_failed", slog.String("error", err.Error()))
		return
	}
	if lead.Email == "" {
		log.Info("sierra_lead_missing_email")
		return
	}
	log = log.With(slog.String("email", lead.Email))

	person, err := h.FUB.FindByEmail(ctx, lead.Email)
	if errors.Is(err, fub.ErrPersonNotFound) {
		log.Info("fub_person_not_found")
		return
	}
	if err != nil {
		log.Error("fub_find_failed", slog.String("error", err.Error()))
		return
	}
	log = log.With(slog.Int64("fub_person_id", person.ID))

	if err := h.FUB.MergeTag(ctx, person.ID, tag); err != nil {
		log.Error("fub_merge_tag_failed", slog.String("error", err.Error()))
		return
	}
	log.Info("tag_synced")
}
