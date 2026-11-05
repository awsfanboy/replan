// Package api is a client for the AWS Events API (https://api.awsevents.com).
//
// It deliberately encodes the API's documented sharp edges so callers don't
// have to: bulk writes that return 200 with per-session failures, writes with
// no idempotency key, per-session quotas with Retry-After, operations that are
// closed (409), and edge refusals with non-JSON bodies.
package api

// Event is one AWS event, such as re:Invent.
type Event struct {
	EventID                string        `json:"eventId"`
	Name                   string        `json:"name"`
	EventType              string        `json:"eventType"`
	StartDate              string        `json:"startDate"`
	EndDate                string        `json:"endDate"`
	IsOnline               bool          `json:"isOnline"`
	Timezone               string        `json:"timezone,omitempty"`
	TimezoneAbbreviation   string        `json:"timezoneAbbreviation,omitempty"`
	TimeFormat             string        `json:"timeFormat,omitempty"`
	Address                *EventAddress `json:"address,omitempty"`
	SupportedLanguageCodes []string      `json:"supportedLanguageCodes,omitempty"`
	AuthenticationRequired bool          `json:"authenticationRequired"`
}

type EventAddress struct {
	City string `json:"city,omitempty"`
}

// Seat availability bands. The API never exposes seat counts.
const (
	SeatAvailable   = "available"
	SeatLimited     = "limited"
	SeatVeryLimited = "veryLimited"
	SeatUnavailable = "unavailable"
	SeatWalkUp      = "walkUp"
)

// Session is a catalog entry exactly as the API returns it. See the catalog
// package for the normalized form.
type Session struct {
	SessionID       string `json:"sessionId"`
	Title           string `json:"title"`
	Abbreviation    string `json:"abbreviation,omitempty"`
	Abstract        string `json:"abstract,omitempty"`
	Type            string `json:"type,omitempty"`
	Level           string `json:"level,omitempty"`
	Venue           string `json:"venue,omitempty"`
	Room            string `json:"room,omitempty"`
	IsAllDaySession bool   `json:"isAllDaySession,omitempty"`
	IsReservable    *bool  `json:"isReservable,omitempty"` // absent until reserved seating opens

	SeatAvailability     string       `json:"seatAvailability,omitempty"`
	SessionTime          *SessionTime `json:"sessionTime,omitempty"`
	Speakers             []Speaker    `json:"speakers,omitempty"`
	Tracks               []string     `json:"tracks,omitempty"`
	Topics               []string     `json:"topics,omitempty"`
	Industries           []string     `json:"industries,omitempty"`
	AreasOfInterest      []string     `json:"areasOfInterest,omitempty"`
	Roles                []string     `json:"roles,omitempty"`
	Services             []string     `json:"services,omitempty"`
	Segments             []string     `json:"segments,omitempty"`
	Features             []string     `json:"features,omitempty"`
	CustomerPersonas     []string     `json:"customerPersonas,omitempty"`
	Experiences          []string     `json:"experiences,omitempty"`
	AdditionalActivities []string     `json:"additionalActivities,omitempty"`
	FocusAreas           []string     `json:"focusAreas,omitempty"`
}

type SessionTime struct {
	Date     string `json:"date,omitempty"`     // "2025-12-02"
	Time     string `json:"time,omitempty"`     // "16:00", local
	Length   string `json:"length,omitempty"`   // minutes, as a string
	Timezone string `json:"timezone,omitempty"` // IANA, when the source provides one
}

type Speaker struct {
	Name string `json:"name,omitempty"`
}

// Reservable reports whether a session takes reservations. An absent flag
// (as in catalogs before reserved seating opens) counts as reservable: the
// server is the judge, and refuses with sessionNotReservable if not.
func (s *Session) Reservable() bool {
	if s.SeatAvailability == SeatWalkUp {
		return false
	}
	return s.IsReservable == nil || *s.IsReservable
}

// Schedule is one attendee's schedule. Session lists carry IDs only.
type Schedule struct {
	Reserved     []string       `json:"reserved"`
	Favorites    []string       `json:"favorites"`
	PersonalTime []PersonalTime `json:"personalTime"`
}

// PersonalTime times are UTC, formatted YYYY-MM-DDTHH:MM:SS with no zone suffix.
type PersonalTime struct {
	PersonalTimeID string `json:"personalTimeId"`
	StartDateTime  string `json:"startDateTime"`
	EndDateTime    string `json:"endDateTime"`
	Title          string `json:"title"`
	Description    string `json:"description"`
	Location       string `json:"location,omitempty"`
}

// PersonalTimeInput: UTC, seconds must be 00, length a multiple of 5 minutes,
// title 1-128 chars, description 1-250 chars, location up to 255 chars.
type PersonalTimeInput struct {
	StartDateTime string `json:"startDateTime"`
	EndDateTime   string `json:"endDateTime"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	Location      string `json:"location,omitempty"`
}

// Bulk failure codes. Treat anything else as a generic refusal: the API adds
// codes over time.
const (
	CodeSessionNotReservable = "sessionNotReservable"
	CodeScheduleConflict     = "scheduleConflict"
	CodeAlreadyScheduled     = "alreadyScheduled"
	CodeSessionFull          = "sessionFull"
	CodeInsufficientAccess   = "insufficientAccess"
	CodeTimePassed           = "timePassed"
	CodeAlreadyFavorited     = "alreadyFavorited"
	CodeNotFavorited         = "notFavorited"
	CodeOther                = "other"
)

// BulkResult: a 200 does not mean everything succeeded. Always read Failed.
type BulkResult struct {
	Successful []string      `json:"successful"`
	Failed     []BulkFailure `json:"failed"`
}

type BulkFailure struct {
	SessionID     string   `json:"sessionId"`
	Code          string   `json:"code"`
	ConflictsWith []string `json:"conflictsWith,omitempty"`
}

// ScheduleService is the write/read surface for one attendee's schedule. It is
// implemented by *Client (the real API) and by sim.Sim (a local simulator used
// for demos against public catalogs and for tests).
type ScheduleService interface {
	GetSchedule(ctx Context, eventID string) (*Schedule, error)
	ReserveSessions(ctx Context, eventID string, sessionIDs []string) (*BulkResult, error)
	CancelReservation(ctx Context, eventID, sessionID string) error
	AssociateFavorites(ctx Context, eventID string, sessionIDs []string) (*BulkResult, error)
	DisassociateFavorite(ctx Context, eventID, sessionID string) error
	CreatePersonalTime(ctx Context, eventID string, in PersonalTimeInput) error
	UpdatePersonalTime(ctx Context, eventID, personalTimeID string, in PersonalTimeInput) error
	DeletePersonalTime(ctx Context, eventID, personalTimeID string) error
}
