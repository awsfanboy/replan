// Package plan compares the desired schedule (reinvent.yaml) with the real one
// (GetSchedule) and works out what to change, Terraform style.
//
// Seat allocation is greedy by rank: the first entry gets the first candidate
// that is reservable, not full, not already started, fits the day cap, and
// leaves enough time to walk to and from its neighbours. Everything already on
// the schedule that replan does not manage is treated as a fixed commitment.
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/awsfanboy/replan/internal/api"
	"github.com/awsfanboy/replan/internal/catalog"
	"github.com/awsfanboy/replan/internal/spec"
	"github.com/awsfanboy/replan/internal/venues"
)

type Input struct {
	Spec     *spec.Spec
	Catalog  *catalog.Catalog
	Schedule *api.Schedule
	Matrix   *venues.Matrix
	Now      time.Time
	// AssumeReservable treats every timed session as reservable. Some
	// public catalogs carry no reservation data; the simulator uses this.
	AssumeReservable bool
	// IgnoreSeatBands skips the "full" check; apply uses it because bands
	// can be stale and the server decides.
	IgnoreSeatBands bool
}

type ActionKind string

const (
	Reserve     ActionKind = "reserve"
	Cancel      ActionKind = "cancel"
	Favorite    ActionKind = "favorite"
	Unfavorite  ActionKind = "unfavorite"
	CreateBlock ActionKind = "create-block"
	UpdateBlock ActionKind = "update-block"
	DeleteBlock ActionKind = "delete-block"
)

type Action struct {
	Kind      ActionKind
	SessionID string
	Session   *catalog.Session // nil for unknown sessions
	Rank      int              // 1-based spec rank for reservations, 0 otherwise
	Block     *api.PersonalTimeInput
	BlockID   string // existing personalTimeId for update/delete
	BlockKey  string
	Reason    string
	GiveBack  bool // a cancellation that returns a seat you won't use
}

// Pick is the outcome for one ranked entry.
type Pick struct {
	Rank    int
	Want    spec.Want
	Chosen  *catalog.Session
	Kept    bool // already reserved (or, for a walk-up, already favorited)
	WalkUp  bool // takes no reservations: attend by walking up; tracked as a favorite
	Skipped []Skip
}

type Skip struct {
	Ref    string
	Reason string
}

type Plan struct {
	Event     string
	Picks     []Pick
	Actions   []Action
	Warnings  []string
	Itinerary []Slot // expected schedule after apply, sorted
}

// Slot is something occupying time: a session or a personal block.
type Slot struct {
	Start, End time.Time
	Venue      string
	Label      string
	Session    *catalog.Session
	Managed    bool // created/managed by replan (spec session or managed block)
	Travel     bool // a travel block
	Rank       int
}

func (p *Plan) Count(k ActionKind) int {
	n := 0
	for _, a := range p.Actions {
		if a.Kind == k {
			n++
		}
	}
	return n
}

func (p *Plan) Empty() bool { return len(p.Actions) == 0 }

// MatrixFor applies reinvent.yaml overrides to the default travel matrix.
func MatrixFor(r spec.Rules) *venues.Matrix {
	m := venues.ReInvent()
	for k, v := range r.TravelTimes {
		d, err := time.ParseDuration(v)
		if err != nil {
			continue
		}
		switch strings.ToLower(k) {
		case "default":
			m.Default = d
		case "samevenue":
			m.SameVenue = d
		default:
			parts := strings.Split(k, "<->")
			if len(parts) == 2 {
				m.Set(venues.Resolve(parts[0], ""), venues.Resolve(parts[1], ""), d)
			}
		}
	}
	return m
}

// Build computes the plan.
func Build(in Input) *Plan {
	sp, cat := in.Spec, in.Catalog
	if in.Matrix == nil {
		in.Matrix = MatrixFor(sp.Rules)
	}
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	sched := in.Schedule
	if sched == nil {
		sched = &api.Schedule{}
	}
	p := &Plan{Event: sp.Event}
	loc := cat.Location

	reserved := set(sched.Reserved)
	favorites := set(sched.Favorites)

	// Sessions the attendee is skipping: give held seats back first.
	skipped := map[string]bool{}
	for _, ref := range sp.Skip {
		if s, ok := cat.Lookup(ref); ok {
			skipped[s.SessionID] = true
			if reserved[s.SessionID] {
				p.Actions = append(p.Actions, Action{Kind: Cancel, SessionID: s.SessionID, Session: s, GiveBack: true,
					Reason: "you're skipping it: the seat goes back to the walk-up line"})
				delete(reserved, s.SessionID)
			}
		} else {
			p.Warnings = append(p.Warnings, fmt.Sprintf("skip %s: not in the catalog", ref))
		}
	}

	// Which reserved sessions does the spec mention at all?
	mentioned := map[string]bool{}
	for _, w := range sp.Sessions {
		for _, ref := range w.Candidates() {
			if s, ok := cat.Lookup(ref); ok {
				mentioned[s.SessionID] = true
			}
		}
	}

	a := &allocator{m: in.Matrix, loc: loc, maxPerDay: sp.Rules.MaxPerDay, early: sp.Rules.ArriveEarlyDuration(), now: in.Now, assume: in.AssumeReservable, ignoreFull: in.IgnoreSeatBands}

	// Fixed commitments: the attendee's own personal time and spec blocks.
	managedPT, ownPT := splitManaged(sched.PersonalTime)
	for _, pt := range ownPT {
		st, en, err := ParseUTC(pt.StartDateTime, pt.EndDateTime)
		if err != nil {
			continue
		}
		a.add(Slot{Start: st, End: en, Venue: venues.Resolve("", pt.Location), Label: "personal: " + pt.Title})
	}
	for _, b := range sp.Blocks {
		st, en, err := b.Times(loc)
		if err != nil {
			continue
		}
		a.add(Slot{Start: st, End: en, Venue: venues.Resolve("", b.Location), Label: "block: " + b.Title, Managed: true})
	}

	// Reservations replan doesn't manage stay put unless prune is on.
	for _, id := range sched.Reserved {
		if mentioned[id] || skipped[id] {
			continue
		}
		s, ok := cat.Lookup(id)
		if !ok {
			p.Warnings = append(p.Warnings, fmt.Sprintf("reserved session %s is not in the catalog (removed or renamed?)", id))
			if sp.Rules.Prune {
				p.Actions = append(p.Actions, Action{Kind: Cancel, SessionID: id, Reason: "not in reinvent.yaml (prune)"})
			}
			continue
		}
		if sp.Rules.Prune {
			p.Actions = append(p.Actions, Action{Kind: Cancel, SessionID: id, Session: s, Reason: "not in reinvent.yaml (prune)"})
			continue
		}
		a.add(Slot{Start: s.Start, End: s.End, Venue: s.Venue, Label: s.Code, Session: s})
	}

	// Wants that are already satisfied are placed first so lower-ranked
	// entries can't push them out.
	picks := make([]Pick, len(sp.Sessions))
	for i, w := range sp.Sessions {
		picks[i] = Pick{Rank: i + 1, Want: w}
		for _, ref := range w.Candidates() {
			s, ok := cat.Lookup(ref)
			if !ok || skipped[s.SessionID] {
				continue
			}
			if walkUp(s, in.AssumeReservable) && favorites[s.SessionID] && s.HasTime() && s.Start.After(in.Now) {
				picks[i].Chosen, picks[i].Kept, picks[i].WalkUp = s, true, true
				a.add(Slot{Start: s.Start, End: s.End, Venue: s.Venue, Label: s.Code, Session: s, Managed: true, Rank: i + 1})
				break
			}
			if reserved[s.SessionID] {
				picks[i].Chosen, picks[i].Kept = s, true
				a.add(Slot{Start: s.Start, End: s.End, Venue: s.Venue, Label: s.Code, Session: s, Managed: true, Rank: i + 1})
				break
			}
		}
	}
	// Extra reservations for the same entry (e.g. primary and fallback both held).
	for i, w := range sp.Sessions {
		for _, ref := range w.Candidates() {
			s, ok := cat.Lookup(ref)
			if ok && reserved[s.SessionID] && picks[i].Chosen != s {
				p.Actions = append(p.Actions, Action{Kind: Cancel, SessionID: s.SessionID, Session: s, Rank: i + 1,
					Reason: fmt.Sprintf("#%d already holds %s", i+1, picks[i].Chosen.Code)})
			}
		}
	}

	var walkUpFavs []*catalog.Session
	for i := range picks {
		pk := &picks[i]
		if pk.Kept {
			// Tell the attendee when a higher-ranked candidate has opened up.
			for _, ref := range pk.Want.Candidates() {
				s, ok := cat.Lookup(ref)
				if !ok {
					continue
				}
				if s == pk.Chosen {
					break
				}
				if s.Reservable() && s.SeatAvailability != api.SeatUnavailable && s.SeatAvailability != "" && s.Start.After(in.Now) {
					p.Warnings = append(p.Warnings, fmt.Sprintf("#%d: %s has seats now; you hold fallback %s", pk.Rank, s.Code, pk.Chosen.Code))
				}
			}
			continue
		}
		for _, ref := range pk.Want.Candidates() {
			s, ok := cat.Lookup(ref)
			if !ok {
				pk.Skipped = append(pk.Skipped, Skip{ref, "not in catalog"})
				continue
			}
			if skipped[s.SessionID] {
				pk.Skipped = append(pk.Skipped, Skip{s.Code, "you're skipping it"})
				continue
			}
			if walkUp(s, in.AssumeReservable) {
				// No reservation to make: plan it as a walk-up and favorite it
				// so it shows in the official app.
				if why := a.reject(s, false); why != "" {
					pk.Skipped = append(pk.Skipped, Skip{s.Code, why})
					continue
				}
				pk.Chosen, pk.WalkUp = s, true
				a.add(Slot{Start: s.Start, End: s.End, Venue: s.Venue, Label: s.Code, Session: s, Managed: true, Rank: pk.Rank})
				walkUpFavs = append(walkUpFavs, s)
				break
			}
			if why := a.reject(s, true); why != "" {
				pk.Skipped = append(pk.Skipped, Skip{s.Code, why})
				continue
			}
			pk.Chosen = s
			a.add(Slot{Start: s.Start, End: s.End, Venue: s.Venue, Label: s.Code, Session: s, Managed: true, Rank: pk.Rank})
			p.Actions = append(p.Actions, Action{Kind: Reserve, SessionID: s.SessionID, Session: s, Rank: pk.Rank, Reason: reserveReason(pk)})
			break
		}
	}
	p.Picks = picks

	// Favorites: the ones asked for, plus walk-up sessions in the plan.
	wantFav := map[string]bool{}
	var favAdds []Action
	for _, s := range walkUpFavs {
		wantFav[s.SessionID] = true
		if !favorites[s.SessionID] {
			favAdds = append(favAdds, Action{Kind: Favorite, SessionID: s.SessionID, Session: s,
				Reason: "walk-up session: no reservation needed; favorited so it shows in your app"})
		}
	}
	for _, ref := range sp.Favorites {
		s, ok := cat.Lookup(ref)
		if !ok {
			p.Warnings = append(p.Warnings, fmt.Sprintf("favorite %s is not in the catalog", ref))
			continue
		}
		if wantFav[s.SessionID] {
			continue
		}
		wantFav[s.SessionID] = true
		if !favorites[s.SessionID] {
			favAdds = append(favAdds, Action{Kind: Favorite, SessionID: s.SessionID, Session: s})
		}
	}
	p.Actions = append(p.Actions, favAdds...)
	if sp.Rules.Prune {
		for _, id := range sched.Favorites {
			if !wantFav[id] {
				s, _ := cat.Lookup(id)
				p.Actions = append(p.Actions, Action{Kind: Unfavorite, SessionID: id, Session: s, Reason: "not in reinvent.yaml (prune)"})
			}
		}
	}

	// Personal time: spec blocks plus travel between the final set of sessions.
	p.Itinerary = a.slots
	desired := DesiredBlocks(sp, loc, a.slots, in.Matrix)
	p.Actions = append(p.Actions, DiffBlocks(managedPT, desired)...)
	for _, d := range desired {
		if strings.HasPrefix(d.Key, "t:") {
			st, en, _ := ParseUTC(d.Input.StartDateTime, d.Input.EndDateTime)
			p.Itinerary = append(p.Itinerary, Slot{Start: st, End: en, Label: d.Input.Title, Travel: true, Managed: true})
		}
	}
	sort.SliceStable(p.Itinerary, func(i, j int) bool { return p.Itinerary[i].Start.Before(p.Itinerary[j].Start) })

	p.Warnings = append(p.Warnings, a.tightFixed()...)
	return p
}

func reserveReason(pk *Pick) string {
	if len(pk.Skipped) == 0 {
		return ""
	}
	return "fallback: " + skipSummary(pk.Skipped)
}

func skipSummary(sk []Skip) string {
	var parts []string
	for _, s := range sk {
		parts = append(parts, s.Ref+" "+s.Reason)
	}
	return strings.Join(parts, "; ")
}

// ---- allocation ----

type allocator struct {
	m          *venues.Matrix
	loc        *time.Location
	maxPerDay  int
	early      time.Duration
	now        time.Time
	assume     bool
	ignoreFull bool
	slots      []Slot
}

func (a *allocator) add(s Slot) { a.slots = append(a.slots, s) }

func (a *allocator) day(t time.Time) string { return t.In(a.loc).Format("2006-01-02") }

// reject explains why s can't be placed, or returns "".
func (a *allocator) reject(s *catalog.Session, checkSeats bool) string {
	if !s.HasTime() {
		return "has no scheduled time"
	}
	if !s.Start.After(a.now) {
		return "already started"
	}
	if checkSeats {
		if s.SeatAvailability == api.SeatWalkUp {
			return "walk-up only"
		}
		if !a.assume && !s.Reservable() {
			return "not reservable"
		}
		if !a.ignoreFull && s.SeatAvailability == api.SeatUnavailable {
			return "full"
		}
	}
	if a.maxPerDay > 0 {
		n := 0
		for _, o := range a.slots {
			if o.Session != nil && a.day(o.Start) == a.day(s.Start) {
				n++
			}
		}
		if n >= a.maxPerDay {
			return fmt.Sprintf("day already has %d sessions (maxPerDay)", n)
		}
	}
	cand := Slot{Start: s.Start, End: s.End, Venue: s.Venue, Label: s.Code, Session: s}
	for _, o := range a.slots {
		if why := a.clash(o, cand); why != "" {
			return why
		}
	}
	return ""
}

// clash checks two slots in either order.
func (a *allocator) clash(x, y Slot) string {
	if x.Start.Before(y.End) && y.Start.Before(x.End) {
		return "clashes with " + x.Label
	}
	first, second := x, y
	if y.End.Compare(x.Start) <= 0 {
		first, second = y, x
	}
	gap := second.Start.Sub(first.End)
	need := a.m.Travel(first.Venue, second.Venue)
	if second.Session != nil {
		need += a.early
	}
	if gap < need {
		return fmt.Sprintf("too tight with %s: needs %s%s, has %s", x.Label, fmtDur(need), route(first.Venue, second.Venue), fmtDur(gap))
	}
	return ""
}

// tightFixed warns about problems among commitments replan didn't choose.
func (a *allocator) tightFixed() []string {
	var out []string
	for i := 0; i < len(a.slots); i++ {
		for j := i + 1; j < len(a.slots); j++ {
			x, y := a.slots[i], a.slots[j]
			if x.Rank > 0 && y.Rank > 0 {
				continue // both chosen by replan: already validated
			}
			if a.day(x.Start) != a.day(y.Start) {
				continue
			}
			if why := a.clash(x, y); why != "" && (x.Session != nil || y.Session != nil) {
				out = append(out, fmt.Sprintf("%s %s", y.Label, why))
			}
		}
	}
	return out
}

func route(a, b string) string {
	if a == "" || b == "" || a == b {
		return ""
	}
	return " (" + a + " → " + b + ")"
}

func fmtDur(d time.Duration) string {
	if d < 0 {
		return "-" + fmtDur(-d)
	}
	h, m := int(d.Hours()), int(d.Minutes())%60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func set(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// ID fingerprints the plan's intended changes. Apply recomputes the plan and
// refuses to run if the fingerprint differs from the one the user approved,
// the same idea as a saved Terraform plan.
func (p *Plan) ID() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", p.Event)
	for _, pk := range p.Picks {
		c := ""
		if pk.Chosen != nil {
			c = pk.Chosen.SessionID
		}
		fmt.Fprintf(h, "pick %d %s %v\n", pk.Rank, c, pk.Kept)
	}
	for _, a := range p.Actions {
		fmt.Fprintf(h, "%s %s %s %s", a.Kind, a.SessionID, a.BlockID, a.BlockKey)
		if a.Block != nil {
			fmt.Fprintf(h, " %s %s %s %s %s", a.Block.StartDateTime, a.Block.EndDateTime, a.Block.Title, a.Block.Description, a.Block.Location)
		}
		fmt.Fprintln(h)
	}
	return "plan-" + hex.EncodeToString(h.Sum(nil))[:12]
}

// walkUp reports whether a session takes no reservations (breakouts and
// lightning talks at re:Invent 2026): you attend by walking up.
func walkUp(s *catalog.Session, assumeReservable bool) bool {
	return !assumeReservable && !s.Reservable()
}
