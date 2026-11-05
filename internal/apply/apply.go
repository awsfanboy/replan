// Package apply executes a plan against the schedule API and then verifies the
// result from GetSchedule, because a 200 from a bulk write doesn't mean it
// worked and no write carries an idempotency key.
//
// Order of operations:
//  1. cancellations (frees time for what comes next)
//  2. reservations, one session per request in rank order, falling back to the
//     next candidate on any refusal; seats are contested, so the plan's choice
//     is re-checked against what is actually held at that moment
//  3. favorites, in batches
//  4. personal time, recomputed from the schedule as it now is, so travel
//     blocks reflect the seats actually won
//  5. a final GetSchedule to verify
package apply

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/awsfanboy/replan/internal/api"
	"github.com/awsfanboy/replan/internal/catalog"
	"github.com/awsfanboy/replan/internal/plan"
	"github.com/awsfanboy/replan/internal/spec"
)

type EventKind string

const (
	EvOK      EventKind = "ok"
	EvFail    EventKind = "fail"
	EvSkip    EventKind = "skip"
	EvInfo    EventKind = "info"
	EvWarning EventKind = "warn"
)

// Event is progress reported to the UI.
type Event struct {
	Kind    EventKind
	Action  plan.ActionKind
	Subject string
	Detail  string
}

type Runner struct {
	API      api.ScheduleService
	Input    plan.Input // same input the plan was built from
	Progress func(Event)
}

type Result struct {
	Reserved        []string // session IDs newly reserved
	Released        []string // session IDs given back (skipped sessions cancelled)
	Failed          int
	ReservationsOff bool // reservations closed (409); the rest still ran
	Final           *api.Schedule
	Unverified      []string // things we expected but GetSchedule doesn't show
}

func (r *Runner) emit(e Event) {
	if r.Progress != nil {
		r.Progress(e)
	}
}

func (r *Runner) Run(ctx context.Context, p *plan.Plan) (*Result, error) {
	res := &Result{}
	ev := p.Event
	in := r.Input

	// 1. cancellations
	for _, a := range p.Actions {
		if a.Kind != plan.Cancel {
			continue
		}
		err := r.API.CancelReservation(ctx, ev, a.SessionID)
		if api.IsUnknownOutcome(err) {
			// A cancel of something already gone is a 404 = done, so one retry is safe.
			err = r.API.CancelReservation(ctx, ev, a.SessionID)
		}
		if err != nil {
			if fatal(err) {
				return res, err
			}
			res.Failed++
			r.emit(Event{EvFail, plan.Cancel, label(a), err.Error()})
			continue
		}
		if a.GiveBack {
			res.Released = append(res.Released, a.SessionID)
			r.emit(Event{EvOK, plan.Cancel, label(a), "seat given back to the walk-up line"})
			continue
		}
		r.emit(Event{EvOK, plan.Cancel, label(a), "cancelled"})
	}

	// 2. reservations
	sched, err := r.API.GetSchedule(ctx, ev)
	if err != nil {
		return res, err
	}
	in.Schedule = sched
	held := setOf(sched.Reserved)
	for _, pk := range p.Picks {
		// Apply does what the plan showed: only entries with a planned
		// reservation, starting at the planned pick and moving down the
		// fallbacks if the server refuses it.
		if pk.Kept || pk.Chosen == nil || pk.WalkUp { // walk-ups are favorited below, never reserved
			continue
		}
		if res.ReservationsOff {
			break
		}
		won := false
		for _, ref := range fromChosen(pk, in.Catalog) {
			s, ok := in.Catalog.Lookup(ref)
			if !ok {
				continue
			}
			if held[s.SessionID] {
				won = true
				break
			}
			if !in.AssumeReservable && !s.Reservable() {
				// A fallback that takes no reservations: attend by walking up.
				if _, err := r.API.AssociateFavorites(ctx, ev, []string{s.SessionID}); err != nil && fatal(err) {
					return res, err
				}
				r.emit(Event{EvOK, plan.Favorite, s.Code, fmt.Sprintf("#%d walk-up fallback: no reservation needed, favorited", pk.Rank)})
				won = true
				break
			}
			// Re-check against what is actually held now, ignoring the
			// (possibly stale) seat band: the server is the judge of "full".
			if why := r.fits(s, held); why != "" {
				r.emit(Event{EvSkip, plan.Reserve, s.Code, why})
				continue
			}
			ok, code, err := r.reserveOne(ctx, ev, s.SessionID)
			if err != nil {
				if api.IsKind(err, api.KindOperationClosed) {
					res.ReservationsOff = true
					r.emit(Event{EvWarning, plan.Reserve, s.Code, "reservations are closed right now (409); favorites and personal time will still be applied"})
					break
				}
				if fatal(err) {
					return res, err
				}
				r.emit(Event{EvFail, plan.Reserve, s.Code, err.Error()})
				continue
			}
			if ok {
				held[s.SessionID] = true
				res.Reserved = append(res.Reserved, s.SessionID)
				detail := fmt.Sprintf("#%d reserved", pk.Rank)
				if s != pk.Chosen && pk.Chosen != nil {
					detail += " (fallback, planned " + pk.Chosen.Code + ")"
				}
				r.emit(Event{EvOK, plan.Reserve, s.Code, detail})
				won = true
				break
			}
			r.emit(Event{EvSkip, plan.Reserve, s.Code, "refused: " + code + tryNext(pk, s)})
		}
		if !won && !res.ReservationsOff {
			res.Failed++
			r.emit(Event{EvFail, plan.Reserve, pk.Want.Reserve, fmt.Sprintf("#%d: no candidate could be reserved", pk.Rank)})
		}
	}

	// 3. favorites
	var favIDs []string
	for _, a := range p.Actions {
		if a.Kind == plan.Favorite {
			favIDs = append(favIDs, a.SessionID)
		}
	}
	if len(favIDs) > 0 {
		br, err := r.API.AssociateFavorites(ctx, ev, favIDs)
		switch {
		case err != nil && fatal(err):
			return res, err
		case err != nil:
			r.emit(Event{EvWarning, plan.Favorite, fmt.Sprintf("%d sessions", len(favIDs)), err.Error() + " (will verify)"})
		default:
			for _, id := range br.Successful {
				r.emit(Event{EvOK, plan.Favorite, code(in.Catalog, id), "favorited"})
			}
			for _, f := range br.Failed {
				if f.Code == api.CodeAlreadyFavorited {
					continue
				}
				res.Failed++
				r.emit(Event{EvFail, plan.Favorite, code(in.Catalog, f.SessionID), f.Code})
			}
		}
	}
	for _, a := range p.Actions {
		if a.Kind != plan.Unfavorite {
			continue
		}
		if err := r.API.DisassociateFavorite(ctx, ev, a.SessionID); err != nil && !api.IsUnknownOutcome(err) {
			if fatal(err) {
				return res, err
			}
			res.Failed++
			r.emit(Event{EvFail, plan.Unfavorite, label(a), err.Error()})
			continue
		}
		r.emit(Event{EvOK, plan.Unfavorite, label(a), "removed"})
	}

	// 4. personal time, from the schedule as it is now
	sched, err = r.API.GetSchedule(ctx, ev)
	if err != nil {
		return res, err
	}
	in.Schedule = sched
	fresh := plan.Build(Held(in))
	for _, a := range fresh.Actions {
		switch a.Kind {
		case plan.DeleteBlock:
			err = r.API.DeletePersonalTime(ctx, ev, a.BlockID)
		case plan.UpdateBlock:
			err = r.API.UpdatePersonalTime(ctx, ev, a.BlockID, *a.Block)
		case plan.CreateBlock:
			err = r.API.CreatePersonalTime(ctx, ev, *a.Block)
		default:
			continue
		}
		if err != nil {
			if fatal(err) {
				return res, err
			}
			if api.IsUnknownOutcome(err) {
				// Never re-send a create: it would duplicate. The marker lets
				// the next plan find and fix whatever actually happened.
				r.emit(Event{EvWarning, a.Kind, a.Block.Title, "outcome unknown; not retried (next `replan plan` will reconcile)"})
				continue
			}
			res.Failed++
			r.emit(Event{EvFail, a.Kind, a.Block.Title, err.Error()})
			continue
		}
		r.emit(Event{EvOK, a.Kind, a.Block.Title, blockWhen(a.Block, in.Catalog.Location)})
	}

	// 5. verify
	final, err := r.API.GetSchedule(ctx, ev)
	if err != nil {
		return res, err
	}
	res.Final = final
	finalHeld := setOf(final.Reserved)
	for _, id := range res.Reserved {
		if !finalHeld[id] {
			res.Unverified = append(res.Unverified, "reservation "+code(in.Catalog, id))
		}
	}
	finalFav := setOf(final.Favorites)
	for _, id := range favIDs {
		if !finalFav[id] {
			res.Unverified = append(res.Unverified, "favorite "+code(in.Catalog, id))
		}
	}
	return res, nil
}

// reserveOne reserves a single session and interprets the per-session result.
// After a lost response it reads the schedule instead of re-sending, because a
// re-sent reservation reports "alreadyScheduled" as a failure and hides what
// actually happened.
func (r *Runner) reserveOne(ctx context.Context, ev, id string) (bool, string, error) {
	br, err := r.API.ReserveSessions(ctx, ev, []string{id})
	if api.IsUnknownOutcome(err) {
		sched, gerr := r.API.GetSchedule(ctx, ev)
		if gerr != nil {
			return false, "", errors.Join(err, gerr)
		}
		r.emit(Event{EvInfo, plan.Reserve, id, "response lost; reconciled from GetSchedule"})
		return setOf(sched.Reserved)[id], "unknown (not on schedule)", nil
	}
	if err != nil {
		return false, "", err
	}
	for _, s := range br.Successful {
		if s == id {
			return true, "", nil
		}
	}
	for _, f := range br.Failed {
		if f.SessionID != id {
			continue
		}
		if f.Code == api.CodeAlreadyScheduled {
			return true, "", nil
		}
		msg := f.Code
		if len(f.ConflictsWith) > 0 {
			var codes []string
			for _, c := range f.ConflictsWith {
				codes = append(codes, code(r.Input.Catalog, c))
			}
			msg += fmt.Sprintf(" with %v", codes)
		}
		return false, msg, nil
	}
	return false, "no result for session", nil
}

// fits re-runs the planner's placement rules for s against the sessions
// actually held right now. Seat bands are ignored: they may be stale, and the
// server is the judge of "full".
func (r *Runner) fits(s *catalog.Session, held map[string]bool) string {
	in := r.Input
	sp := *in.Spec
	sp.Sessions = []spec.Want{{Reserve: s.SessionID}}
	sp.Favorites = nil
	sp.Rules.Prune = false
	sched := &api.Schedule{Reserved: keys(held)}
	if in.Schedule != nil {
		sched.PersonalTime = in.Schedule.PersonalTime
	}
	probe := plan.Build(plan.Input{Spec: &sp, Catalog: in.Catalog, Schedule: sched, Matrix: in.Matrix,
		Now: in.Now, AssumeReservable: in.AssumeReservable, IgnoreSeatBands: true})
	if pk := probe.Picks[0]; pk.Chosen == nil && len(pk.Skipped) > 0 {
		return pk.Skipped[0].Reason
	}
	return ""
}

// fromChosen lists the entry's candidates from the planned pick onwards.
func fromChosen(pk plan.Pick, c *catalog.Catalog) []string {
	cands := pk.Want.Candidates()
	for i, ref := range cands {
		if s, ok := c.Lookup(ref); ok && s == pk.Chosen {
			return cands[i:]
		}
	}
	return cands
}

func fatal(err error) bool {
	return api.IsKind(err, api.KindAuthRequired) || api.IsKind(err, api.KindNotRegistered) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func tryNext(pk plan.Pick, s *catalog.Session) string {
	c := pk.Want.Candidates()
	for i, ref := range c {
		if (ref == s.Code || ref == s.SessionID) && i+1 < len(c) {
			return "; trying " + c[i+1]
		}
	}
	return ""
}

func label(a plan.Action) string {
	if a.Session != nil {
		return a.Session.Code
	}
	return a.SessionID
}

func code(c *catalog.Catalog, id string) string {
	if s, ok := c.Lookup(id); ok {
		return s.Code
	}
	return id
}

func blockWhen(b *api.PersonalTimeInput, loc *time.Location) string {
	st, en, err := plan.ParseUTC(b.StartDateTime, b.EndDateTime)
	if err != nil {
		return b.StartDateTime + "Z → " + b.EndDateTime + "Z"
	}
	st, en = st.In(loc), en.In(loc)
	return st.Format("Mon 15:04") + "–" + en.Format("15:04")
}

func setOf(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func keys(m map[string]bool) []string {
	var out []string
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	return out
}

// Held narrows the input to the seats actually held: ranked entries with no
// reservation are dropped, so walking blocks and agendas are computed from
// reality, never for a session that could not be reserved.
func Held(in plan.Input) plan.Input {
	held := map[string]bool{}
	if in.Schedule != nil {
		for _, id := range in.Schedule.Reserved {
			held[id] = true
		}
		// A walk-up session you favorited is one you plan to attend.
		for _, id := range in.Schedule.Favorites {
			if s, ok := in.Catalog.Lookup(id); ok && !in.AssumeReservable && !s.Reservable() {
				held[id] = true
			}
		}
	}
	sp := *in.Spec
	sp.Sessions = nil
	for _, w := range in.Spec.Sessions {
		for _, ref := range w.Candidates() {
			if s, ok := in.Catalog.Lookup(ref); ok && held[s.SessionID] {
				sp.Sessions = append(sp.Sessions, w)
				break
			}
		}
	}
	in.Spec = &sp
	return in
}
