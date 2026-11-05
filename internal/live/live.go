// Package live is replan's during-the-event co-pilot. Planners help before
// re:Invent; live answers the questions people have on the Strip:
//
//   - Next: where do I need to be, when do I leave, will I make it (also if I
//     am running late), and if not, what can I still reach instead?
//   - Tonight: which of tomorrow's reservations can't realistically be kept
//     (clashes, impossible walks, a repeat held twice)? Giving those seats back
//     the evening before lets someone in the walk-up line have them.
//   - Impact: which catalog changes (moves, cancellations, new launches)
//     matter to *my* schedule.
//
// Everything here is read-only analysis. Acting on it goes through the plan
// file (skip, add a fallback) and the usual plan/approve/apply loop.
package live

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/awsfanboy/replan/internal/api"
	"github.com/awsfanboy/replan/internal/catalog"
	"github.com/awsfanboy/replan/internal/plan"
	"github.com/awsfanboy/replan/internal/spec"
	"github.com/awsfanboy/replan/internal/venues"
)

// SeatRelease is how long before the start re:Invent releases reserved seats
// to the walk-up line if the holder hasn't been scanned in.
const SeatRelease = 10 * time.Minute

type Input struct {
	Catalog  *catalog.Catalog
	Schedule *api.Schedule
	Spec     *spec.Spec // optional: ranks and skips
	Matrix   *venues.Matrix
	Now      time.Time
	At       string        // where you are now (venue name); default: inferred
	Late     time.Duration // extra delay before you can leave
	// IgnoreSeatBands treats sessions marked full as open (simulator demos).
	IgnoreSeatBands bool
}

// Status of getting to a session.
const (
	InProgress = "in-progress"
	Missed     = "missed" // under way, and you are somewhere else
	OnTrack    = "on-track"
	Tight      = "tight"        // you make it, with under 5 minutes to spare before seats are released
	SeatRisk   = "seat-at-risk" // you arrive after reserved seats are released: walk-up line
	WontMake   = "wont-make-it"
)

type Leg struct {
	Session      *catalog.Session
	From         string
	Travel       time.Duration
	LeaveBy      time.Time // to arrive before reserved seats are released
	ArriveAt     time.Time
	Slack        time.Duration // vs. the seat release time; negative = late
	Status       string
	Alternatives []*catalog.Session
}

type Report struct {
	Now    time.Time
	At     string
	Day    string
	Legs   []Leg
	Done   int // sessions already over today
	Advice []string
}

func (in Input) matrix() *venues.Matrix {
	if in.Matrix != nil {
		return in.Matrix
	}
	return venues.ReInvent()
}

func (in Input) loc() *time.Location { return in.Catalog.Location }

func sameDay(a, b time.Time, loc *time.Location) bool {
	return a.In(loc).Format("2006-01-02") == b.In(loc).Format("2006-01-02")
}

// held returns the reserved sessions with a known time, sorted by start.
func (in Input) held() []*catalog.Session {
	var out []*catalog.Session
	seen := map[string]bool{}
	for _, id := range in.Schedule.Reserved {
		if s, ok := in.Catalog.Lookup(id); ok && s.HasTime() && !seen[id] {
			out = append(out, s)
			seen[id] = true
		}
	}
	// Walk-up sessions in the plan that you favorited are sessions you plan to
	// attend, even though they hold no seat.
	if in.Spec != nil {
		fav := map[string]bool{}
		for _, id := range in.Schedule.Favorites {
			fav[id] = true
		}
		for _, w := range in.Spec.Sessions {
			for _, ref := range w.Candidates() {
				s, ok := in.Catalog.Lookup(ref)
				if ok && fav[s.SessionID] && !s.Reservable() && s.HasTime() && !seen[s.SessionID] {
					out = append(out, s)
					seen[s.SessionID] = true
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

func (in Input) heldSet() map[string]bool {
	m := map[string]bool{}
	for _, id := range in.Schedule.Reserved {
		m[id] = true
	}
	if in.Spec != nil {
		for _, ref := range in.Spec.Skip {
			if s, ok := in.Catalog.Lookup(ref); ok {
				m[s.SessionID] = true // never suggest a skipped session
			}
		}
	}
	return m
}

// Next answers "what's next today, and will I make it?".
func Next(in Input) Report {
	m, loc := in.matrix(), in.loc()
	r := Report{Now: in.Now, Day: in.Now.In(loc).Format("Monday 2006-01-02")}
	var today []*catalog.Session
	var current *catalog.Session
	var last *catalog.Session
	for _, s := range in.held() {
		if !sameDay(s.Start, in.Now, loc) {
			continue
		}
		switch {
		case !s.End.After(in.Now):
			r.Done++
			last = s
		case !s.Start.After(in.Now):
			current = s
		default:
			today = append(today, s)
		}
	}

	at := venues.Resolve(in.At, "")
	switch {
	case at != "":
	case current != nil:
		at = current.Venue
	case last != nil:
		at = last.Venue
	}
	r.At = at

	ready := in.Now.Add(in.Late)
	if current != nil {
		explicit := venues.Resolve(in.At, "")
		if explicit != "" && current.Venue != "" && explicit != current.Venue {
			// You said where you are, and it isn't where this session is.
			r.Legs = append(r.Legs, Leg{Session: current, From: explicit, Status: Missed})
		} else {
			r.Legs = append(r.Legs, Leg{Session: current, From: current.Venue, Status: InProgress})
			if current.End.After(ready) {
				ready = current.End
			}
		}
	}
	from := at
	for _, s := range today {
		travel := m.Travel(from, s.Venue)
		if from == "" {
			travel = m.SameVenue
		}
		arrive := ready.Add(travel)
		release := s.Start.Add(-SeatRelease)
		lg := Leg{Session: s, From: from, Travel: travel, ArriveAt: arrive, LeaveBy: release.Add(-travel), Slack: release.Sub(arrive)}
		switch {
		case arrive.After(s.Start):
			lg.Status = WontMake
		case arrive.After(release):
			lg.Status = SeatRisk
		case lg.Slack < 5*time.Minute:
			lg.Status = Tight
		default:
			lg.Status = OnTrack
		}
		if lg.Status == WontMake || lg.Status == SeatRisk {
			lg.Alternatives = in.alternatives(s, from, ready, 3)
		}
		r.Legs = append(r.Legs, lg)
		// You stay for the session if you make it; otherwise you are still free
		// at the same place and time for the next one.
		if lg.Status != WontMake {
			ready, from = s.End, s.Venue
		}
	}

	for _, lg := range r.Legs {
		code := lg.Session.Code
		switch lg.Status {
		case Missed:
			r.Advice = append(r.Advice, fmt.Sprintf("%s started at %s at %s while you're at %s. If you're not going, mark it skipped so replan stops planning around it.",
				code, clock(lg.Session.Start, loc), lg.Session.Venue, lg.From))
		case WontMake:
			msg := fmt.Sprintf("You won't make %s (you'd arrive %s, it starts %s). Give the seat back now so someone in the walk-up line gets it: skip %s.",
				code, clock(lg.ArriveAt, loc), clock(lg.Session.Start, loc), code)
			if lg.Session.Recorded {
				msg += " It's likely recorded, so you can watch it later."
			}
			r.Advice = append(r.Advice, msg)
		case SeatRisk:
			r.Advice = append(r.Advice, fmt.Sprintf("%s: reserved seats are released at %s and you'd arrive %s. Leave now, or expect the walk-up line.",
				code, clock(lg.Session.Start.Add(-SeatRelease), loc), clock(lg.ArriveAt, loc)))
		case Tight:
			r.Advice = append(r.Advice, fmt.Sprintf("%s: leave by %s.", code, clock(lg.LeaveBy, loc)))
		}
	}
	if len(r.Legs) == 0 {
		r.Advice = append(r.Advice, "Nothing else reserved today.")
	}
	return r
}

// alternatives finds sessions you can still reach instead of s: starting
// from 15 minutes before to 90 minutes after it, reachable in time from where
// you are, most similar first. Walk-up counts: re:Invent keeps walk-up seats.
func (in Input) alternatives(s *catalog.Session, from string, ready time.Time, n int) []*catalog.Session {
	m := in.matrix()
	taken := in.heldSet()
	tags := tagSet(s)
	type cand struct {
		s     *catalog.Session
		score int
	}
	var cs []cand
	for _, o := range in.Catalog.Sessions {
		if o == s || taken[o.SessionID] || !o.HasTime() || (!in.IgnoreSeatBands && o.SeatAvailability == api.SeatUnavailable) {
			continue
		}
		if o.Start.Before(s.Start.Add(-15*time.Minute)) || o.Start.After(s.Start.Add(90*time.Minute)) {
			continue
		}
		if ready.Add(m.Travel(from, o.Venue)).After(o.Start) {
			continue
		}
		score := 0
		for _, t := range tagList(o) {
			if tags[strings.ToLower(t)] {
				score += 2
			}
		}
		if score == 0 {
			continue
		}
		if o.BaseCode == s.BaseCode {
			score += 10 // a repeat of the same session
		}
		if o.Kind == s.Kind {
			score++
		}
		if !o.Recorded {
			score++
		}
		if o.Venue == from {
			score++ // no walk
		}
		cs = append(cs, cand{o, score})
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].score != cs[j].score {
			return cs[i].score > cs[j].score
		}
		return cs[i].s.Start.Before(cs[j].s.Start)
	})
	var out []*catalog.Session
	for i := 0; i < len(cs) && i < n; i++ {
		out = append(out, cs[i].s)
	}
	return out
}

func tagList(s *catalog.Session) []string {
	var out []string
	out = append(out, s.Topics...)
	out = append(out, s.Services...)
	return append(out, s.AreasOfInterest...)
}

func tagSet(s *catalog.Session) map[string]bool {
	m := map[string]bool{}
	for _, t := range tagList(s) {
		m[strings.ToLower(t)] = true
	}
	return m
}

func clock(t time.Time, loc *time.Location) string { return t.In(loc).Format("15:04") }

// ---- tonight: seats you can't realistically keep ----

type Issue struct {
	Session *catalog.Session `json:"-"`
	Code    string           `json:"code"`
	Problem string           `json:"problem"`
	Suggest string           `json:"suggest"`
	Other   string           `json:"other,omitempty"` // the session it conflicts with
}

// Tonight checks one day's reservations (YYYY-MM-DD in event time) for
// seats that can't realistically be used, and suggests which to give back.
// The lower-ranked session of a conflicting pair is the one suggested.
func Tonight(in Input, day string) []Issue {
	m, loc := in.matrix(), in.loc()
	rank := in.ranks()
	var held []*catalog.Session
	for _, s := range in.held() {
		if s.Start.In(loc).Format("2006-01-02") == day {
			held = append(held, s)
		}
	}
	var out []Issue
	flagged := map[string]bool{}
	lower := func(a, b *catalog.Session) (*catalog.Session, *catalog.Session) {
		if rank(a) <= rank(b) {
			return b, a
		}
		return a, b
	}
	add := func(s, other *catalog.Session, problem string) {
		if flagged[s.SessionID] {
			return
		}
		flagged[s.SessionID] = true
		code := ""
		if other != nil {
			code = other.Code
		}
		out = append(out, Issue{Session: s, Code: s.Code, Other: code, Problem: problem,
			Suggest: "skip " + s.Code + " so the seat goes back to the walk-up line"})
	}

	// A repeat held twice.
	byBase := map[string]*catalog.Session{}
	for _, s := range in.held() {
		if o, ok := byBase[s.BaseCode]; ok {
			drop, keep := lower(o, s)
			if drop.Start.In(loc).Format("2006-01-02") == day {
				add(drop, keep, fmt.Sprintf("you also hold %s, another run of the same session", keep.Code))
			}
			continue
		}
		byBase[s.BaseCode] = s
	}
	// Overlaps and impossible walks between consecutive sessions.
	for i := 0; i < len(held); i++ {
		for j := i + 1; j < len(held); j++ {
			a, b := held[i], held[j]
			if a.Overlaps(b) {
				drop, keep := lower(a, b)
				add(drop, keep, fmt.Sprintf("overlaps %s (%s–%s)", keep.Code, clock(keep.Start, loc), clock(keep.End, loc)))
				continue
			}
			if j != i+1 {
				continue
			}
			arrive := a.End.Add(m.Travel(a.Venue, b.Venue))
			if arrive.After(b.Start) {
				add(b, a, fmt.Sprintf("after %s ends at %s you'd reach %s at %s; it starts %s",
					a.Code, clock(a.End, loc), b.Venue, clock(arrive, loc), clock(b.Start, loc)))
			}
		}
	}
	// Your own personal time (meetings) on top of a reservation.
	_, own := splitOwn(in.Schedule.PersonalTime)
	for _, pt := range own {
		st, en, err := plan.ParseUTC(pt.StartDateTime, pt.EndDateTime)
		if err != nil {
			continue
		}
		for _, s := range held {
			if s.Start.Before(en) && st.Before(s.End) {
				add(s, nil, fmt.Sprintf("clashes with your own %q (%s–%s)", pt.Title, clock(st, loc), clock(en, loc)))
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Session.Start.Before(out[j].Session.Start) })
	return out
}

func splitOwn(pts []api.PersonalTime) (managed, own []api.PersonalTime) {
	for _, pt := range pts {
		if _, ok := plan.MarkerKey(pt); ok {
			managed = append(managed, pt)
		} else {
			own = append(own, pt)
		}
	}
	return
}

// ranks maps a session to its rank in the plan file; unranked sessions
// rank last, so they are the ones suggested for giving back.
func (in Input) ranks() func(*catalog.Session) int {
	r := map[string]int{}
	if in.Spec != nil {
		for i, w := range in.Spec.Sessions {
			for _, ref := range w.Candidates() {
				if s, ok := in.Catalog.Lookup(ref); ok {
					r[s.SessionID] = i + 1
				}
			}
		}
	}
	return func(s *catalog.Session) int {
		if v, ok := r[s.SessionID]; ok {
			return v
		}
		return 1 << 20
	}
}

// NextEventDay returns the first event day (YYYY-MM-DD) after now that has
// reservations, or today's if now is before the first one.
func NextEventDay(in Input) string {
	loc := in.loc()
	today := in.Now.In(loc).Format("2006-01-02")
	for _, s := range in.held() {
		d := s.Start.In(loc).Format("2006-01-02")
		if d > today {
			return d
		}
	}
	return today
}
