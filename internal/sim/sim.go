// Package sim is a local stand-in for the schedule half of the AWS Events API.
//
// It lets anyone try replan without touching a real schedule, and lets tests
// exercise the ugly paths: full sessions, clashes,
// operations that are closed, and writes whose outcome is lost. It follows the
// real API's documented semantics, including bulk 200s with per-session
// failures and personal time that is created without returning an ID.
package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/awsfanboy/replan/internal/api"
	"github.com/awsfanboy/replan/internal/catalog"
)

type State struct {
	Reserved     []string           `json:"reserved"`
	Favorites    []string           `json:"favorites"`
	PersonalTime []api.PersonalTime `json:"personalTime"`
	NextID       int                `json:"nextId"`
}

type Sim struct {
	Catalog *catalog.Catalog
	Path    string // where state persists; "" keeps it in memory
	Now     time.Time

	// FullRate makes that fraction of sessions report sessionFull,
	// deterministically per session ID, so fallbacks can be demonstrated.
	FullRate float64
	// Full forces specific session IDs or codes to be full.
	Full map[string]bool
	// ReservationsClosed makes ReserveSessions answer 409, as the real API
	// does before reserved seating opens.
	ReservationsClosed bool
	// OpenSeats ignores the catalog's real seat bands (most sessions are full
	// once reserved seating opens), so a demo week can always be booked.
	OpenSeats bool
	// LoseResponse makes the next write of this operation succeed on the
	// server but report an unknown outcome to the caller.
	LoseResponse map[string]int

	mu    sync.Mutex
	state State
}

func New(cat *catalog.Catalog, path string) (*Sim, error) {
	s := &Sim{Catalog: cat, Path: path, Full: map[string]bool{}, LoseResponse: map[string]int{}}
	s.state = State{Reserved: []string{}, Favorites: []string{}, PersonalTime: []api.PersonalTime{}}
	if path != "" {
		b, err := os.ReadFile(path)
		if err == nil {
			if err := json.Unmarshal(b, &s.state); err != nil {
				return nil, fmt.Errorf("sim state %s: %w", path, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return s, nil
}

func (s *Sim) now() time.Time {
	if s.Now.IsZero() {
		return time.Now()
	}
	return s.Now
}

func (s *Sim) save() error {
	if s.Path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s.state, "", "  ")
	return os.WriteFile(s.Path, b, 0o600)
}

// lose reports whether this write's response should be "lost".
func (s *Sim) lose(op string) bool {
	if s.LoseResponse[op] > 0 {
		s.LoseResponse[op]--
		return true
	}
	return false
}

func lostErr(op string) error {
	return &api.UnknownOutcomeError{Op: op, Err: errors.New("simulated dropped connection")}
}

func (s *Sim) IsFull(sess *catalog.Session) bool {
	if s.Full[sess.SessionID] || s.Full[strings.ToUpper(sess.Code)] || (!s.OpenSeats && sess.SeatAvailability == api.SeatUnavailable) {
		return true
	}
	if s.FullRate <= 0 {
		return false
	}
	h := fnv.New32a()
	h.Write([]byte(sess.SessionID))
	return float64(h.Sum32()%1000)/1000 < s.FullRate
}

func (s *Sim) GetSchedule(ctx context.Context, eventID string) (*api.Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.state)
	var out State
	_ = json.Unmarshal(b, &out)
	return &api.Schedule{Reserved: out.Reserved, Favorites: out.Favorites, PersonalTime: out.PersonalTime}, nil
}

func (s *Sim) ReserveSessions(ctx context.Context, eventID string, ids []string) (*api.BulkResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ReservationsClosed {
		return nil, &api.Error{Op: "ReserveSessions", Status: 409, Kind: api.KindOperationClosed, Message: "Reservations are not open yet."}
	}
	if err := checkBulk("ReserveSessions", ids); err != nil {
		return nil, err
	}
	res := &api.BulkResult{Successful: []string{}, Failed: []api.BulkFailure{}}
	for _, id := range ids {
		sess, ok := s.Catalog.Lookup(id)
		switch {
		case !ok || sess.SessionID != id:
			res.Failed = append(res.Failed, api.BulkFailure{SessionID: id, Code: api.CodeOther})
		case contains(s.state.Reserved, id):
			res.Failed = append(res.Failed, api.BulkFailure{SessionID: id, Code: api.CodeAlreadyScheduled})
		case !sess.HasTime() || !sess.Start.After(s.now()):
			res.Failed = append(res.Failed, api.BulkFailure{SessionID: id, Code: api.CodeTimePassed})
		case sess.SeatAvailability == api.SeatWalkUp:
			res.Failed = append(res.Failed, api.BulkFailure{SessionID: id, Code: api.CodeSessionNotReservable})
		case s.IsFull(sess):
			res.Failed = append(res.Failed, api.BulkFailure{SessionID: id, Code: api.CodeSessionFull})
		default:
			if clash := s.clashes(sess); len(clash) > 0 {
				res.Failed = append(res.Failed, api.BulkFailure{SessionID: id, Code: api.CodeScheduleConflict, ConflictsWith: clash})
				continue
			}
			s.state.Reserved = append(s.state.Reserved, id)
			res.Successful = append(res.Successful, id)
		}
	}
	if err := s.save(); err != nil {
		return nil, err
	}
	if s.lose("ReserveSessions") {
		return nil, lostErr("ReserveSessions")
	}
	return res, nil
}

func (s *Sim) clashes(sess *catalog.Session) []string {
	var out []string
	for _, rid := range s.state.Reserved {
		if o, ok := s.Catalog.Lookup(rid); ok && o.Overlaps(sess) {
			out = append(out, rid)
		}
	}
	return out
}

func (s *Sim) CancelReservation(ctx context.Context, eventID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !contains(s.state.Reserved, id) {
		return nil // the real client maps 404 to success
	}
	s.state.Reserved = remove(s.state.Reserved, id)
	return s.save()
}

func (s *Sim) AssociateFavorites(ctx context.Context, eventID string, ids []string) (*api.BulkResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := checkBulk("AssociateFavorites", ids); err != nil {
		return nil, err
	}
	res := &api.BulkResult{Successful: []string{}, Failed: []api.BulkFailure{}}
	for _, id := range ids {
		if _, ok := s.Catalog.Lookup(id); !ok {
			res.Failed = append(res.Failed, api.BulkFailure{SessionID: id, Code: api.CodeOther})
			continue
		}
		if contains(s.state.Favorites, id) {
			res.Failed = append(res.Failed, api.BulkFailure{SessionID: id, Code: api.CodeAlreadyFavorited})
			continue
		}
		s.state.Favorites = append(s.state.Favorites, id)
		res.Successful = append(res.Successful, id)
	}
	return res, s.save()
}

func (s *Sim) DisassociateFavorite(ctx context.Context, eventID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Favorites = remove(s.state.Favorites, id)
	return s.save()
}

func validatePT(op string, in api.PersonalTimeInput) error {
	bad := func(msg string) error {
		return &api.Error{Op: op, Status: 400, Kind: api.KindValidation, Message: msg}
	}
	st, err := time.ParseInLocation("2006-01-02T15:04:05", in.StartDateTime, time.UTC)
	if err != nil || strings.ContainsAny(in.StartDateTime, "Z+") {
		return bad("startDateTime must be YYYY-MM-DDTHH:MM:SS in UTC")
	}
	en, err := time.ParseInLocation("2006-01-02T15:04:05", in.EndDateTime, time.UTC)
	if err != nil {
		return bad("endDateTime must be YYYY-MM-DDTHH:MM:SS in UTC")
	}
	if st.Second() != 0 || en.Second() != 0 {
		return bad("seconds must be 00")
	}
	if !en.After(st) || en.Sub(st)%(5*time.Minute) != 0 {
		return bad("length must be a positive multiple of 5 minutes")
	}
	if l := len([]rune(in.Title)); l < 1 || l > 128 {
		return bad("title must be 1-128 characters")
	}
	if l := len([]rune(in.Description)); l < 1 || l > 250 {
		return bad("description must be 1-250 characters")
	}
	if len([]rune(in.Location)) > 255 {
		return bad("location must be at most 255 characters")
	}
	return nil
}

func (s *Sim) CreatePersonalTime(ctx context.Context, eventID string, in api.PersonalTimeInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validatePT("CreatePersonalTime", in); err != nil {
		return err
	}
	s.state.NextID++
	s.state.PersonalTime = append(s.state.PersonalTime, api.PersonalTime{
		PersonalTimeID: fmt.Sprintf("pt-%04d", s.state.NextID),
		StartDateTime:  in.StartDateTime, EndDateTime: in.EndDateTime,
		Title: in.Title, Description: in.Description, Location: in.Location,
	})
	if err := s.save(); err != nil {
		return err
	}
	if s.lose("CreatePersonalTime") {
		return lostErr("CreatePersonalTime")
	}
	return nil
}

func (s *Sim) UpdatePersonalTime(ctx context.Context, eventID, id string, in api.PersonalTimeInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validatePT("UpdatePersonalTime", in); err != nil {
		return err
	}
	for i, pt := range s.state.PersonalTime {
		if pt.PersonalTimeID == id {
			s.state.PersonalTime[i] = api.PersonalTime{PersonalTimeID: id, StartDateTime: in.StartDateTime, EndDateTime: in.EndDateTime,
				Title: in.Title, Description: in.Description, Location: in.Location}
			return s.save()
		}
	}
	return &api.Error{Op: "UpdatePersonalTime", Status: 404, Kind: api.KindNotFound}
}

func (s *Sim) DeletePersonalTime(ctx context.Context, eventID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.state.PersonalTime[:0]
	for _, pt := range s.state.PersonalTime {
		if pt.PersonalTimeID != id {
			out = append(out, pt)
		}
	}
	s.state.PersonalTime = out
	return s.save()
}

func checkBulk(op string, ids []string) error {
	if len(ids) < 1 || len(ids) > api.MaxBulk {
		return &api.Error{Op: op, Status: 400, Kind: api.KindValidation, Message: "sessionIds must hold 1 to 10 IDs"}
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return &api.Error{Op: op, Status: 400, Kind: api.KindValidation, Message: "sessionIds must be distinct"}
		}
		seen[id] = true
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func remove(xs []string, x string) []string {
	out := xs[:0]
	for _, v := range xs {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}

var _ api.ScheduleService = (*Sim)(nil)
