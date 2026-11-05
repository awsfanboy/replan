package api

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Context is an alias so the interface in types.go reads cleanly.
type Context = context.Context

// ErrorKind classifies an API failure by what the caller should do about it.
type ErrorKind int

const (
	KindUnknown         ErrorKind = iota
	KindValidation                // 400: fix the request, never retry
	KindAuthRequired              // 401: sign in or refresh, retry once
	KindNotRegistered             // 403 with JSON body: register for the event; signing in again won't help
	KindEdgeRefused               // 403 with no body: the edge is refusing this address; slow down
	KindNotFound                  // 404: for removals, treat as already done
	KindOperationClosed           // 409: e.g. reservations not open yet; retrying now won't help
	KindThrottled                 // 429: wait RetryAfter
	KindServer                    // 500
	KindUnavailable               // 503
)

func (k ErrorKind) String() string {
	switch k {
	case KindValidation:
		return "validation"
	case KindAuthRequired:
		return "auth-required"
	case KindNotRegistered:
		return "not-registered"
	case KindEdgeRefused:
		return "edge-refused"
	case KindNotFound:
		return "not-found"
	case KindOperationClosed:
		return "operation-closed"
	case KindThrottled:
		return "throttled"
	case KindServer:
		return "server-error"
	case KindUnavailable:
		return "unavailable"
	}
	return "unknown"
}

// Error is returned for every non-2xx response.
type Error struct {
	Op         string
	Status     int
	Kind       ErrorKind
	Message    string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = hint(e.Kind)
	}
	return fmt.Sprintf("%s: HTTP %d (%s): %s", e.Op, e.Status, e.Kind, msg)
}

func hint(k ErrorKind) string {
	switch k {
	case KindAuthRequired:
		return "sign in with `replan login`"
	case KindNotRegistered:
		return "you are signed in but not registered for this event"
	case KindEdgeRefused:
		return "requests from your address are being refused; slow down"
	case KindOperationClosed:
		return "this operation is closed right now (reservations open via the API on the date AWS announces)"
	case KindThrottled:
		return "quota exceeded for this minute"
	}
	return "no details"
}

// IsKind reports whether err is an *Error of kind k.
func IsKind(err error, k ErrorKind) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == k
}

// UnknownOutcomeError wraps a write whose result we never learned (timeout,
// dropped connection, 500, 503). The API has no idempotency key, so the only
// safe next step is to read GetSchedule and reconcile.
type UnknownOutcomeError struct {
	Op  string
	Err error
}

func (e *UnknownOutcomeError) Error() string {
	return fmt.Sprintf("%s: outcome unknown, reconcile from GetSchedule before retrying: %v", e.Op, e.Err)
}

func (e *UnknownOutcomeError) Unwrap() error { return e.Err }
