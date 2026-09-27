// Package client is a small Go client for the Stellar Jay HTTP API described in
// docs/api.md. It follows the write rules in llm.md: every append carries the
// root read just before it and a stable Idempotency-Key, and a replay pins the
// root captured on its first page.
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxResponseBytes = 8 << 20
	payloadBatchSize = 100
	replayPageSize   = 1000
)

// Error is a structured API error: {"error":{"code","message"}} plus the HTTP
// status it came with.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("stellar jay: %s (HTTP %d)", e.Code, e.Status)
	}
	return "stellar jay: " + e.Message
}

// IsRootConflict reports whether err is a 409 caused by a stale
// expected_root. Callers may re-read the root and retry.
func IsRootConflict(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict &&
		strings.HasPrefix(strings.ToLower(apiErr.Message), "root changed")
}

// IsKeyReuse reports whether err is a 409 for an Idempotency-Key that was
// already used for a different request. Retrying cannot fix it.
func IsKeyReuse(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict &&
		strings.Contains(strings.ToLower(apiErr.Message), "different content")
}

// Options configure New.
type Options struct {
	// HTTPClient defaults to a client with a 30 second timeout.
	HTTPClient *http.Client
	// AllowInsecureHTTP permits http:// origins that are not loopback.
	AllowInsecureHTTP bool
}

// Client talks to one Stellar Jay origin with one bearer token.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a client for baseURL, an origin with no path, query or
// credentials. Plain http is accepted only for loopback hosts unless
// opts.AllowInsecureHTTP is set.
func New(baseURL, token string, opts Options) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("stellar jay URL must be an http(s) origin, got %q", baseURL)
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("stellar jay URL must be an origin only, with no credentials, path, query or fragment")
	}
	if u.Scheme == "http" && !opts.AllowInsecureHTTP && !loopbackHost(u.Hostname()) {
		return nil, errors.New("stellar jay URL must use https")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("stellar jay token is required")
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{base: u.Scheme + "://" + u.Host, token: token, http: hc}, nil
}

// BaseURL is the origin this client talks to.
func (c *Client) BaseURL() string { return c.base }

// CredentialID is a short, non-secret fingerprint of the token. Callers use it
// to keep idempotency keys from different credentials apart.
func (c *Client) CredentialID() string {
	sum := sha256.Sum256([]byte(c.token))
	return hex.EncodeToString(sum[:6])
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Event is one entry of the history as returned by GET /v1/events.
type Event struct {
	EventID   string          `json:"event_id"`
	Hash      string          `json:"hash"`
	Type      string          `json:"type"`
	EntityID  string          `json:"entity_id,omitempty"`
	Parents   []string        `json:"parents"`
	Actor     string          `json:"actor"`
	Role      string          `json:"role"`
	Command   string          `json:"command"`
	CreatedAt time.Time       `json:"created_at"`
	RequestID string          `json:"request_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// Ready checks GET /health/ready.
func (c *Client) Ready(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/health/ready", nil, "", nil)
}

// Root returns the current root, "" for an empty store.
func (c *Client) Root(ctx context.Context) (string, error) {
	var out struct {
		Root string `json:"root"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/root", nil, "", &out)
	return out.Root, err
}

// AppendRequest is the caller-controlled part of an event. The server derives
// actor, role, time, parents and hash.
type AppendRequest struct {
	Type     string `json:"type"`
	EntityID string `json:"entity_id,omitempty"`
	Command  string `json:"command"`
	Payload  any    `json:"payload"`
}

// AppendResult is the response to an append. Replayed is true when the same
// Idempotency-Key and body were already committed.
type AppendResult struct {
	Hash     string `json:"hash"`
	Root     string `json:"root"`
	Replayed bool   `json:"replayed"`
}

// Append reads the current root and appends req against it. A concurrent
// writer between the two calls yields an error for which IsRootConflict is
// true.
func (c *Client) Append(ctx context.Context, req AppendRequest, idempotencyKey string) (AppendResult, error) {
	root, err := c.Root(ctx)
	if err != nil {
		return AppendResult{}, err
	}
	return c.AppendAt(ctx, req, root, idempotencyKey)
}

// AppendAt appends req only if the store root is still expectedRoot.
func (c *Client) AppendAt(ctx context.Context, req AppendRequest, expectedRoot, idempotencyKey string) (AppendResult, error) {
	if n := len(idempotencyKey); n < 8 || n > 200 {
		return AppendResult{}, errors.New("idempotency key must be 8-200 characters")
	}
	body := struct {
		AppendRequest
		ExpectedRoot string `json:"expected_root"`
	}{req, expectedRoot}
	raw, err := json.Marshal(body)
	if err != nil {
		return AppendResult{}, err
	}
	var out AppendResult
	err = c.do(ctx, http.MethodPost, "/v1/events", raw, idempotencyKey, &out)
	return out, err
}

// EventsQuery selects one page of GET /v1/events.
type EventsQuery struct {
	After          string
	Root           string
	Limit          int
	IncludePayload bool
}

// EventsPage is one page of history.
type EventsPage struct {
	Events  []Event `json:"events"`
	Root    string  `json:"root"`
	HasMore bool    `json:"has_more"`
}

// Events returns one page of history metadata (and payloads when asked).
func (c *Client) Events(ctx context.Context, q EventsQuery) (EventsPage, error) {
	v := url.Values{}
	if q.After != "" {
		v.Set("after", q.After)
	}
	if q.Root != "" {
		v.Set("root", q.Root)
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.IncludePayload {
		v.Set("include_payload", "true")
	}
	path := "/v1/events"
	if encoded := v.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var page EventsPage
	err := c.do(ctx, http.MethodGet, path, nil, "", &page)
	return page, err
}

// Replay walks history metadata after the given hash ("" for all of it) in
// chain order, calling fn for each event. The root captured on the first page
// bounds the whole walk, so concurrent appends are not mixed in. It returns
// that root, which is the checkpoint to resume from.
func (c *Client) Replay(ctx context.Context, after string, fn func(Event) error) (string, error) {
	return c.replay(ctx, EventsQuery{After: after, Limit: replayPageSize}, fn)
}

// ReplayTo walks all history up to and including root, in chain order. Two
// walks to the same root see the same events.
func (c *Client) ReplayTo(ctx context.Context, root string, fn func(Event) error) error {
	if root == "" {
		return nil
	}
	_, err := c.replay(ctx, EventsQuery{Root: root, Limit: replayPageSize}, fn)
	return err
}

func (c *Client) replay(ctx context.Context, first EventsQuery, fn func(Event) error) (string, error) {
	page, err := c.Events(ctx, first)
	if err != nil {
		return "", err
	}
	target := page.Root
	for {
		for _, ev := range page.Events {
			if err := fn(ev); err != nil {
				return target, err
			}
		}
		if !page.HasMore || len(page.Events) == 0 {
			return target, nil
		}
		last := page.Events[len(page.Events)-1].EventID
		page, err = c.Events(ctx, EventsQuery{After: last, Root: target, Limit: replayPageSize})
		if err != nil {
			return target, err
		}
	}
}

// Payloads returns the decrypted payloads of the given events, keyed by event
// ID, as seen at root. Requests are split into batches the server accepts.
func (c *Client) Payloads(ctx context.Context, root string, eventIDs []string) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(eventIDs))
	seen := make(map[string]bool, len(eventIDs))
	ids := make([]string, 0, len(eventIDs))
	for _, id := range eventIDs {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for start := 0; start < len(ids); start += payloadBatchSize {
		batch := ids[start:min(start+payloadBatchSize, len(ids))]
		raw, err := json.Marshal(map[string]any{"root": root, "event_ids": batch})
		if err != nil {
			return nil, err
		}
		var resp struct {
			Payloads []struct {
				EventID string          `json:"event_id"`
				Payload json.RawMessage `json:"payload"`
			} `json:"payloads"`
		}
		if err := c.do(ctx, http.MethodPost, "/v1/events/payloads", raw, "", &resp); err != nil {
			return nil, err
		}
		for _, p := range resp.Payloads {
			out[p.EventID] = p.Payload
		}
	}
	return out, nil
}

// Ref returns the root a named ref points at. A missing ref is an *Error with
// Status 404.
func (c *Client) Ref(ctx context.Context, name string) (string, error) {
	var out struct {
		Root string `json:"root"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/refs/"+url.PathEscape(name), nil, "", &out)
	return out.Root, err
}

// PutRef points a named ref at root if it currently points at expectedRoot
// ("" to create it).
func (c *Client) PutRef(ctx context.Context, name, root, expectedRoot string) error {
	raw, err := json.Marshal(map[string]string{"root": root, "expected_root": expectedRoot})
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, "/v1/refs/"+url.PathEscape(name), raw, "", nil)
}

// do sends one request. Reads, payload selection and appends with an
// Idempotency-Key are retried on transport errors and 5xx responses, because
// repeating them cannot change the result.
func (c *Client) do(ctx context.Context, method, path string, body []byte, idempotencyKey string, result any) error {
	retryable := method == http.MethodGet || idempotencyKey != "" || path == "/v1/events/payloads"
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(100<<attempt) * time.Millisecond):
			}
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		if !strings.HasPrefix(path, "/health/") {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if idempotencyKey != "" {
			req.Header.Set("Idempotency-Key", idempotencyKey)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("stellar jay %s %s: %w", method, path, err)
			if retryable && ctx.Err() == nil {
				continue
			}
			return lastErr
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("read stellar jay response: %w", err)
		}
		if len(data) > maxResponseBytes {
			return errors.New("stellar jay response exceeded the client limit")
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = decodeError(resp.StatusCode, data)
			if retryable && resp.StatusCode >= 500 && resp.StatusCode != http.StatusInsufficientStorage {
				continue
			}
			return lastErr
		}
		if result != nil && len(data) > 0 {
			if err := json.Unmarshal(data, result); err != nil {
				return fmt.Errorf("decode stellar jay response: %w", err)
			}
		}
		return nil
	}
	return lastErr
}

func decodeError(status int, data []byte) error {
	var envelope struct {
		Error Error `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err == nil && envelope.Error.Code != "" {
		envelope.Error.Status = status
		return &envelope.Error
	}
	code := "internal_error"
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		code = "permission_denied"
	case status == http.StatusNotFound:
		code = "not_found"
	case status == http.StatusConflict:
		code = "conflict"
	case status == http.StatusTooManyRequests:
		code = "rate_limited"
	case status < 500:
		code = "validation_error"
	}
	return &Error{Status: status, Code: code, Message: fmt.Sprintf("HTTP %d", status)}
}
