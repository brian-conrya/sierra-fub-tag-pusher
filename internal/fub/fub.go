// Package fub contains the Follow Up Boss API client used to look up a
// person by email and merge a tag onto their record.
//
// API reference: https://docs.followupboss.com/
package fub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/brian-conrya/sierra-fub-tag-pusher/internal/retry"
)

// BaseURL is the Follow Up Boss REST API host.
const BaseURL = "https://api.followupboss.com"

// Person is the minimal projection of FUB's person record needed by the
// service. Decoding additional fields is unnecessary.
type Person struct {
	ID   int64    `json:"id"`
	Tags []string `json:"tags"`
}

// peopleListResponse mirrors the wire format of GET /v1/people.
type peopleListResponse struct {
	People []Person `json:"people"`
}

// ErrPersonNotFound signals that no person matches the queried email. The
// handler treats this as a benign condition (logged at info, no FUB write).
var ErrPersonNotFound = errors.New("fub: person not found")

// Client calls Follow Up Boss's REST API.
type Client struct {
	// BaseURL defaults to the package-level BaseURL. Exported so tests can
	// point the client at an httptest server.
	BaseURL string
	APIKey  string
	Retry   *retry.Client
}

// New returns a Client targeting FUB's production API.
func New(apiKey string, r *retry.Client) *Client {
	return &Client{
		BaseURL: BaseURL,
		APIKey:  apiKey,
		Retry:   r,
	}
}

// FindByEmail returns the FUB person matching the given email address.
//
// Trashed leads are included (includeTrash=true) so they still receive tag
// updates. If multiple records share the email (FUB doesn't enforce
// uniqueness), we ask FUB to sort by id ascending and return only the
// first — deterministically the oldest record, no client-side scan.
//
// Returns ErrPersonNotFound if no match exists.
func (c *Client) FindByEmail(ctx context.Context, email string) (Person, error) {
	q := url.Values{}
	q.Set("email", email)
	q.Set("includeTrash", "true")
	q.Set("sort", "id")
	q.Set("limit", "1")
	target := strings.TrimRight(c.BaseURL, "/") + "/v1/people?" + q.Encode()

	build := func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		c.setAuth(req)
		req.Header.Set("Accept", "application/json")
		return req, nil
	}

	resp, err := c.Retry.Do(ctx, build)
	if err != nil {
		return Person{}, fmt.Errorf("fub find by email: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Person{}, fmt.Errorf("fub find by email: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed peopleListResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return Person{}, fmt.Errorf("fub find by email: decode: %w", err)
	}
	if len(parsed.People) == 0 {
		return Person{}, ErrPersonNotFound
	}
	return parsed.People[0], nil
}

// MergeTag appends a tag to the given person. Uses FUB's mergeTags=true
// query parameter so the call is idempotent and does not overwrite existing
// tags. A no-op if the tag is already present on the person.
func (c *Client) MergeTag(ctx context.Context, personID int64, tag string) error {
	target := fmt.Sprintf("%s/v1/people/%d?mergeTags=true", strings.TrimRight(c.BaseURL, "/"), personID)
	payload, err := json.Marshal(map[string]any{"tags": []string{tag}})
	if err != nil {
		return fmt.Errorf("fub merge tag: marshal: %w", err)
	}

	build := func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		c.setAuth(req)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		return req, nil
	}

	resp, err := c.Retry.Do(ctx, build)
	if err != nil {
		return fmt.Errorf("fub merge tag: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("fub merge tag: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c *Client) setAuth(req *http.Request) {
	req.SetBasicAuth(c.APIKey, "")
}
