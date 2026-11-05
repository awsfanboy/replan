package sim

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/awsfanboy/replan/internal/api"
	"github.com/awsfanboy/replan/internal/catalog"
)

// Shake returns a copy of the catalog with the kind of changes that happen
// during re:Invent, aimed at the given reservations so a demo shows real
// impact: one of your sessions moves, another changes venue, a related session
// is cancelled, and two "NEW LAUNCH" sessions appear after the keynote.
func Shake(cat *catalog.Catalog, reserved []string) (*catalog.Catalog, []string, error) {
	var held []*catalog.Session
	for _, id := range reserved {
		if s, ok := cat.Lookup(id); ok && s.HasTime() {
			held = append(held, s)
		}
	}
	sort.Slice(held, func(i, j int) bool { return held[i].Start.Before(held[j].Start) })
	if len(held) == 0 {
		return nil, nil, fmt.Errorf("reserve a few sessions first (in the simulator: replan apply --sim), then shake")
	}
	raw := make([]api.Session, 0, len(cat.Sessions)+2)
	byID := map[string]int{}
	for _, s := range cat.Sessions {
		byID[s.SessionID] = len(raw)
		raw = append(raw, s.Session)
	}
	loc := cat.Location
	var notes []string

	// 1. Your first session moves: on top of your next one when there is one
	// (the clash replan tonight should catch), otherwise 90 minutes later.
	h0 := held[0]
	moved := h0.Start.Add(90 * time.Minute).In(loc)
	if len(held) > 1 && sameDay(held[1].Start, h0.Start, loc) {
		moved = held[1].Start.Add(-30 * time.Minute).In(loc)
	}
	r := &raw[byID[h0.SessionID]]
	st := *r.SessionTime
	st.Date, st.Time = moved.Format("2006-01-02"), moved.Format("15:04")
	r.SessionTime = &st
	notes = append(notes, fmt.Sprintf("%s moved from %s to %s", h0.Code, h0.Start.In(loc).Format("Mon 15:04"), moved.Format("Mon 15:04")))

	// 2. Your last session changes venue.
	if len(held) > 1 {
		h1 := held[len(held)-1]
		r := &raw[byID[h1.SessionID]]
		if h1.Venue == "" { // single-venue event: another room
			r.Room = "Level 2, Room 2D01"
			notes = append(notes, fmt.Sprintf("%s moved from %s to %s", h1.Code, h1.Room, r.Room))
		} else {
			to, room := "MGM Grand", "MGM Grand | Level 1 | Room 116"
			if strings.Contains(h1.Venue, "MGM") {
				to, room = "Caesars Forum", "Caesars Forum | Level 1 | Academy 415"
			}
			r.Venue, r.Room = to, room
			notes = append(notes, fmt.Sprintf("%s moved from %s to %s", h1.Code, h1.Venue, to))
		}
	}

	// 3. A related session you don't hold is cancelled.
	tags := map[string]bool{}
	for _, t := range append(append([]string{}, h0.Topics...), h0.Services...) {
		tags[t] = true
	}
	taken := map[string]bool{}
	for _, s := range held {
		taken[s.SessionID] = true
	}
	for _, s := range cat.Sessions {
		if taken[s.SessionID] || !s.HasTime() || s.Start.In(loc).YearDay() != h0.Start.In(loc).YearDay() {
			continue
		}
		hit := false
		for _, t := range s.Topics {
			hit = hit || tags[t]
		}
		if hit {
			delete(byID, s.SessionID)
			notes = append(notes, fmt.Sprintf("%s was cancelled", s.Code))
			break
		}
	}

	// 4. Two sessions on something launched at the keynote, on your topics.
	topic := "your topics"
	if len(h0.Topics) > 0 {
		topic = h0.Topics[0]
	}
	day := h0.Start.In(loc)
	next := day.AddDate(0, 0, 1)
	tz := loc.String()
	for i, n := range []struct{ code, title, typ, date, clock string }{
		{"KEY301-NEW", "NEW LAUNCH: What's new for " + topic + ", announced at the keynote", "Breakout session", day.Format("2006-01-02"), "16:30"},
		{"KEY302-NEW", "NEW LAUNCH: Hands-on with today's " + topic + " launch", "Chalk talk", next.Format("2006-01-02"), "10:00"},
	} {
		venue, room := "Venetian", "Venetian | Level 2 | Titian 2201"
		if h0.Venue == "" {
			venue, room = "", "Level 1, Keynote Hall"
		}
		s := api.Session{SessionID: fmt.Sprintf("shake-new-%d", i+1), Abbreviation: n.code, Title: n.title, Type: n.typ,
			Abstract: "NEW LAUNCH. Added after the keynote: the team behind today's announcement walks through it.",
			Venue:    venue, Room: room, Level: "300 – Advanced",
			Topics: h0.Topics, Services: h0.Services, AreasOfInterest: h0.AreasOfInterest,
			SessionTime: &api.SessionTime{Date: n.date, Time: n.clock, Length: "60", Timezone: tz}}
		byID[s.SessionID] = len(raw)
		raw = append(raw, s)
		notes = append(notes, fmt.Sprintf("%s added: %s", n.code, n.title))
	}

	keep := make([]api.Session, 0, len(raw))
	for _, s := range raw {
		if _, ok := byID[s.SessionID]; ok {
			keep = append(keep, s)
		}
	}
	out, err := catalog.Build(cat.Event, keep, cat.FetchedAt)
	return out, notes, err
}

func sameDay(a, b time.Time, loc *time.Location) bool {
	return a.In(loc).Format("2006-01-02") == b.In(loc).Format("2006-01-02")
}
