package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const DefaultBaseURL = "https://api.awsevents.com"

// MaxBulk is the most session IDs one bulk request may carry.
const MaxBulk = 10

// TokenSource supplies bearer tokens. force asks for a fresh token after a 401.
type TokenSource interface {
	Token(ctx context.Context, force bool) (string, error)
}

type Client struct {
	BaseURL   string
	HTTP      *http.Client
	Tokens    TokenSource // nil means anonymous
	UserAgent string

	// MaxThrottleWait bounds how long the client will sleep on a 429 before
	// giving up and returning the error. A 429 performed no write, so waiting
	// and retrying is always safe.
	MaxThrottleWait time.Duration
	// ReadRetries is how many times a read is retried on 500/503/network errors.
	ReadRetries int

	sleep func(context.Context, time.Duration) error
}

func NewClient(tokens TokenSource) *Client {
	return &Client{
		BaseURL:         DefaultBaseURL,
		HTTP:            &http.Client{Timeout: 30 * time.Second},
		Tokens:          tokens,
		UserAgent:       "replan/0.1 (+https://github.com/awsfanboy/replan)",
		MaxThrottleWait: 65 * time.Second,
		ReadRetries:     3,
		sleep:           sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---- Catalog ----

func (c *Client) ListEvents(ctx context.Context, includePast bool) ([]Event, error) {
	q := url.Values{}
	if includePast {
		q.Set("includePast", "true")
	}
	var out struct {
		Items []Event `json:"items"`
	}
	err := c.do(ctx, "ListEvents", http.MethodGet, "/v1/events", q, nil, &out, false)
	return out.Items, err
}

func (c *Client) GetEvent(ctx context.Context, eventID string) (*Event, error) {
	var out struct {
		Event Event `json:"event"`
	}
	if err := c.do(ctx, "GetEvent", http.MethodGet, "/v1/events/"+esc(eventID), nil, nil, &out, false); err != nil {
		return nil, err
	}
	return &out.Event, nil
}

// ListSessions walks every page. Pages can come back short; only an absent
// nextToken means the end.
func (c *Client) ListSessions(ctx context.Context, eventID, locale string, includeAbstracts bool) ([]Session, error) {
	var all []Session
	token := ""
	for {
		q := url.Values{}
		if locale != "" {
			q.Set("locale", locale)
		}
		if !includeAbstracts {
			q.Set("includeAbstracts", "false")
		}
		if token != "" {
			q.Set("nextToken", token)
		}
		var page struct {
			Items      []Session `json:"items"`
			TotalCount float64   `json:"totalCount"`
			NextToken  string    `json:"nextToken"`
		}
		if err := c.do(ctx, "ListSessions", http.MethodGet, "/v1/events/"+esc(eventID)+"/sessions", q, nil, &page, false); err != nil {
			return all, err
		}
		all = append(all, page.Items...)
		if page.NextToken == "" {
			return all, nil
		}
		token = page.NextToken
	}
}

func (c *Client) GetSession(ctx context.Context, eventID, sessionID string) (*Session, error) {
	var out struct {
		Session Session `json:"session"`
	}
	p := "/v1/events/" + esc(eventID) + "/sessions/" + esc(sessionID)
	if err := c.do(ctx, "GetSession", http.MethodGet, p, nil, nil, &out, false); err != nil {
		return nil, err
	}
	return &out.Session, nil
}

// ---- Schedule (implements ScheduleService) ----

func (c *Client) GetSchedule(ctx context.Context, eventID string) (*Schedule, error) {
	var out struct {
		Schedule Schedule `json:"schedule"`
	}
	if err := c.do(ctx, "GetSchedule", http.MethodGet, "/v1/events/"+esc(eventID)+"/schedule", nil, nil, &out, false); err != nil {
		return nil, err
	}
	return &out.Schedule, nil
}

func (c *Client) ReserveSessions(ctx context.Context, eventID string, ids []string) (*BulkResult, error) {
	return c.bulk(ctx, "ReserveSessions", "/v1/events/"+esc(eventID)+"/reservations", ids)
}

func (c *Client) AssociateFavorites(ctx context.Context, eventID string, ids []string) (*BulkResult, error) {
	return c.bulk(ctx, "AssociateFavorites", "/v1/events/"+esc(eventID)+"/favorites", ids)
}

// CancelReservation treats 404 as success: the reservation is already gone,
// which makes a retry after an unknown outcome safe.
func (c *Client) CancelReservation(ctx context.Context, eventID, sessionID string) error {
	p := "/v1/events/" + esc(eventID) + "/reservations/" + esc(sessionID)
	return ignoreNotFound(c.do(ctx, "CancelReservation", http.MethodDelete, p, nil, nil, nil, true))
}

func (c *Client) DisassociateFavorite(ctx context.Context, eventID, sessionID string) error {
	p := "/v1/events/" + esc(eventID) + "/favorites/" + esc(sessionID)
	return ignoreNotFound(c.do(ctx, "DisassociateFavorite", http.MethodDelete, p, nil, nil, nil, true))
}

// CreatePersonalTime returns 204 with no body, so the new entry's ID is not
// known until the next GetSchedule. Re-sending it creates a duplicate.
func (c *Client) CreatePersonalTime(ctx context.Context, eventID string, in PersonalTimeInput) error {
	return c.do(ctx, "CreatePersonalTime", http.MethodPost, "/v1/events/"+esc(eventID)+"/personal-time", nil, in, nil, true)
}

func (c *Client) UpdatePersonalTime(ctx context.Context, eventID, id string, in PersonalTimeInput) error {
	p := "/v1/events/" + esc(eventID) + "/personal-time/" + esc(id)
	return c.do(ctx, "UpdatePersonalTime", http.MethodPut, p, nil, in, nil, true)
}

func (c *Client) DeletePersonalTime(ctx context.Context, eventID, id string) error {
	p := "/v1/events/" + esc(eventID) + "/personal-time/" + esc(id)
	return ignoreNotFound(c.do(ctx, "DeletePersonalTime", http.MethodDelete, p, nil, nil, nil, true))
}

// bulk splits ids into requests of at most MaxBulk and merges the results. If
// one chunk's outcome is unknown, the merged result so far is returned along
// with the *UnknownOutcomeError.
func (c *Client) bulk(ctx context.Context, op, path string, ids []string) (*BulkResult, error) {
	res := &BulkResult{Successful: []string{}, Failed: []BulkFailure{}}
	for start := 0; start < len(ids); start += MaxBulk {
		end := min(start+MaxBulk, len(ids))
		var out struct {
			Result BulkResult `json:"result"`
		}
		body := map[string][]string{"sessionIds": ids[start:end]}
		if err := c.do(ctx, op, http.MethodPost, path, nil, body, &out, true); err != nil {
			return res, err
		}
		res.Successful = append(res.Successful, out.Result.Successful...)
		res.Failed = append(res.Failed, out.Result.Failed...)
	}
	return res, nil
}

func ignoreNotFound(err error) error {
	if IsKind(err, KindNotFound) {
		return nil
	}
	return err
}

func esc(s string) string { return url.PathEscape(s) }

// ---- transport ----

func (c *Client) do(ctx context.Context, op, method, path string, q url.Values, in, out any, write bool) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	refreshed := false
	attempt := 0
	for {
		attempt++
		status, hdr, data, err := c.roundTrip(ctx, method, path, q, body, refreshed)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if write {
				return &UnknownOutcomeError{Op: op, Err: err}
			}
			if attempt <= c.ReadRetries {
				if err := c.sleep(ctx, backoff(attempt)); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("%s: %w", op, err)
		}
		if status >= 200 && status < 300 {
			if out != nil && len(data) > 0 {
				if err := json.Unmarshal(data, out); err != nil {
					return fmt.Errorf("%s: decoding response: %w", op, err)
				}
			}
			return nil
		}

		apiErr := classify(op, status, hdr, data)
		switch apiErr.Kind {
		case KindAuthRequired:
			if c.Tokens != nil && !refreshed {
				refreshed = true
				continue
			}
		case KindThrottled:
			// A 429 performed nothing, so retrying is safe for writes too.
			wait := apiErr.RetryAfter
			if wait <= 0 {
				wait = 5 * time.Second
			}
			if wait <= c.MaxThrottleWait {
				if err := c.sleep(ctx, wait); err != nil {
					return err
				}
				continue
			}
		case KindServer, KindUnavailable:
			if write {
				return &UnknownOutcomeError{Op: op, Err: apiErr}
			}
			if attempt <= c.ReadRetries {
				if err := c.sleep(ctx, backoff(attempt)); err != nil {
					return err
				}
				continue
			}
		case KindEdgeRefused:
			if !write && attempt <= c.ReadRetries {
				if err := c.sleep(ctx, 2*backoff(attempt)); err != nil {
					return err
				}
				continue
			}
		}
		return apiErr
	}
}

func (c *Client) roundTrip(ctx context.Context, method, path string, q url.Values, body []byte, forceToken bool) (int, http.Header, []byte, error) {
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Tokens != nil {
		tok, err := c.Tokens.Token(ctx, forceToken)
		if err != nil {
			return 0, nil, nil, err
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, data, nil
}

// classify maps a response to an *Error. The edge can answer with no body or
// with HTML, so the body is never assumed to be JSON.
func classify(op string, status int, hdr http.Header, data []byte) *Error {
	e := &Error{Op: op, Status: status}
	var j struct {
		Message string `json:"message"`
	}
	isJSON := len(bytes.TrimSpace(data)) > 0 && json.Unmarshal(data, &j) == nil
	if isJSON {
		e.Message = j.Message
	}
	switch status {
	case 400:
		e.Kind = KindValidation
	case 401:
		e.Kind = KindAuthRequired
	case 403:
		if isJSON {
			e.Kind = KindNotRegistered
		} else {
			e.Kind = KindEdgeRefused
		}
	case 404:
		e.Kind = KindNotFound
	case 409:
		e.Kind = KindOperationClosed
	case 429:
		e.Kind = KindThrottled
		if s, err := strconv.Atoi(strings.TrimSpace(hdr.Get("Retry-After"))); err == nil {
			e.RetryAfter = time.Duration(s) * time.Second
		}
	case 500:
		e.Kind = KindServer
	case 503:
		e.Kind = KindUnavailable
	default:
		if status >= 500 {
			e.Kind = KindServer
		}
	}
	return e
}

func backoff(attempt int) time.Duration {
	return time.Duration(1<<min(attempt, 5)) * 250 * time.Millisecond
}

var _ ScheduleService = (*Client)(nil)

// IsUnknownOutcome reports whether err means a write may or may not have happened.
func IsUnknownOutcome(err error) bool {
	var u *UnknownOutcomeError
	return errors.As(err, &u)
}
