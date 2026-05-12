// Package sierra contains the Sierra Interactive webhook payload types and
// the client for fetching lead details by id.
//
// API reference: https://api.sierrainteractivedev.com/
package sierra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/brian-conrya/sierra-fub-tag-pusher/internal/retry"
)

const (
	// BaseURL is the Sierra Interactive REST API host.
	BaseURL = "https://api.sierrainteractivedev.com"

	// EventLeadTagAdded is the only event the service subscribes to.
	EventLeadTagAdded = "LeadTagAdded"

	leadPath = "/leads/get/"
)

// WebhookEvent is the envelope Sierra POSTs to subscribed webhook URLs.
type WebhookEvent struct {
	EventCreated string           `json:"eventCreated"`
	EventType    string           `json:"eventType"`
	ResourceList []int64          `json:"resourceList"`
	Data         WebhookEventData `json:"data"`
}

// WebhookEventData is the payload-specific block. For LeadTagAdded /
// LeadTagRemoved Sierra populates Tag with the affected tag string.
type WebhookEventData struct {
	Claimed bool   `json:"claimed"`
	Tag     string `json:"tag"`
}

// Validate enforces the invariants the handler relies on: correct event
// type, at least one lead in the resource list, and a non-empty tag.
func (e *WebhookEvent) Validate() error {
	if e.EventType != EventLeadTagAdded {
		return fmt.Errorf("unsupported eventType %q", e.EventType)
	}
	if len(e.ResourceList) == 0 {
		return errors.New("empty resourceList")
	}
	if strings.TrimSpace(e.Data.Tag) == "" {
		return errors.New("missing data.tag")
	}
	return nil
}

// Lead is the subset of Sierra's lead response the service cares about.
type Lead struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
}

// leadResponse mirrors Sierra's wire format: { success, data: {...} }.
type leadResponse struct {
	Success bool `json:"success"`
	Data    Lead `json:"data"`
}

// Client calls Sierra's REST API.
type Client struct {
	// BaseURL defaults to the package-level BaseURL. Exported so tests can
	// point the client at an httptest server.
	BaseURL string
	APIKey  string
	Retry   *retry.Client
}

// New returns a Client targeting Sierra's production API.
func New(apiKey string, r *retry.Client) *Client {
	return &Client{
		BaseURL: BaseURL,
		APIKey:  apiKey,
		Retry:   r,
	}
}

// GetLead fetches lead details by Sierra's numeric id.
func (c *Client) GetLead(ctx context.Context, id int64) (Lead, error) {
	u := strings.TrimRight(c.BaseURL, "/") + leadPath + url.PathEscape(strconv.FormatInt(id, 10)) + "?includeTags=true"
	build := func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		c.setAuthHeaders(req)
		req.Header.Set("Accept", "application/json")
		return req, nil
	}

	resp, err := c.Retry.Do(ctx, build)
	if err != nil {
		return Lead{}, fmt.Errorf("sierra get lead %d: %w", id, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Lead{}, fmt.Errorf("sierra get lead %d: status %d: %s", id, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed leadResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return Lead{}, fmt.Errorf("sierra get lead %d: decode response: %w", id, err)
	}
	if !parsed.Success {
		return Lead{}, fmt.Errorf("sierra get lead %d: success=false", id)
	}
	if parsed.Data.ID == 0 {
		parsed.Data.ID = id
	}
	return parsed.Data, nil
}

// SubscriptionRequest is the body for POST /webhook.
type SubscriptionRequest struct {
	EventTypes []string `json:"eventTypes"`
	WebhookURL string   `json:"url"`
}

// SubscriptionResponse is the envelope returned by POST /webhook.
type SubscriptionResponse struct {
	Success bool `json:"success"`
	Data    struct {
		ID         int64    `json:"id"`
		EventTypes []string `json:"eventTypes"`
		URL        string   `json:"url"`
	} `json:"data"`
}

// AddWebhook registers a webhook subscription with Sierra. Used by the
// deployment script after the Cloud Run service is created.
func (c *Client) AddWebhook(ctx context.Context, req SubscriptionRequest) (SubscriptionResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return SubscriptionResponse{}, fmt.Errorf("marshal subscription: %w", err)
	}

	build := func(ctx context.Context) (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/webhook", strings.NewReader(string(body)))
		if err != nil {
			return nil, err
		}
		c.setAuthHeaders(r)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json")
		return r, nil
	}

	resp, err := c.Retry.Do(ctx, build)
	if err != nil {
		return SubscriptionResponse{}, fmt.Errorf("sierra add webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return SubscriptionResponse{}, fmt.Errorf("sierra add webhook: status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var parsed SubscriptionResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return SubscriptionResponse{}, fmt.Errorf("sierra add webhook: decode response: %w", err)
	}
	if !parsed.Success {
		return SubscriptionResponse{}, errors.New("sierra add webhook: success=false")
	}
	return parsed, nil
}

func (c *Client) setAuthHeaders(req *http.Request) {
	req.Header.Set("Sierra-ApiKey", c.APIKey)
}
