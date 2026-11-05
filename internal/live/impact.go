package live

import (
	"fmt"
	"sort"
	"strings"

	"github.com/awsfanboy/replan/internal/catalog"
)

// Relevance of a catalog change to you, most important first.
const (
	YourReservation = "your reservation"
	InYourPlan      = "in your plan"
	YourFavorite    = "your favorite"
	NewLaunch       = "new launch"
	YourInterests   = "matches your interests"
)

type Impact struct {
	catalog.Change
	Relevance string `json:"relevance"`
	Effect    string `json:"effect"`
}

// Impacts keeps the changes that matter to you and says what each means for
// your schedule. Unrelated changes are counted, not listed.
func Impacts(in Input, changes []catalog.Change) (out []Impact, unrelated int) {
	held := map[string]bool{}
	for _, id := range in.Schedule.Reserved {
		held[id] = true
	}
	fav := map[string]bool{}
	for _, id := range in.Schedule.Favorites {
		fav[id] = true
	}
	planned := map[string]bool{}
	interests := map[string]bool{}
	addTags := func(s *catalog.Session) {
		for t := range tagSet(s) {
			interests[t] = true
		}
	}
	if in.Spec != nil {
		for _, w := range in.Spec.Sessions {
			for _, ref := range w.Candidates() {
				if s, ok := in.Catalog.Lookup(ref); ok {
					planned[s.SessionID] = true
					addTags(s)
				}
			}
		}
	}
	for id := range held {
		if s, ok := in.Catalog.Lookup(id); ok {
			addTags(s)
		}
	}

	for _, c := range changes {
		im := Impact{Change: c}
		s, inCatalog := in.Catalog.Lookup(c.SessionID)
		switch {
		case held[c.SessionID]:
			im.Relevance = YourReservation
		case planned[c.SessionID]:
			im.Relevance = InYourPlan
		case fav[c.SessionID]:
			im.Relevance = YourFavorite
		case c.Kind == catalog.Added && c.NewLaunch:
			im.Relevance = NewLaunch
		case c.Kind == catalog.Added && inCatalog && shares(s, interests):
			im.Relevance = YourInterests
		default:
			unrelated++
			continue
		}
		im.Effect = in.effect(c, s, inCatalog, held)
		out = append(out, im)
	}
	order := map[string]int{YourReservation: 0, InYourPlan: 1, YourFavorite: 2, NewLaunch: 3, YourInterests: 4}
	sort.SliceStable(out, func(i, j int) bool { return order[out[i].Relevance] < order[out[j].Relevance] })
	return out, unrelated
}

func shares(s *catalog.Session, interests map[string]bool) bool {
	n := 0
	for t := range tagSet(s) {
		if interests[t] {
			n++
		}
	}
	return n >= 2 || (n >= 1 && len(tagSet(s)) <= 2)
}

func (in Input) effect(c catalog.Change, s *catalog.Session, inCatalog bool, held map[string]bool) string {
	switch c.Kind {
	case catalog.Removed:
		if held[c.SessionID] {
			return "Cancelled. Your seat is gone: pick a fallback (run plan) or ask for alternatives."
		}
		return "Cancelled: remove it from your plan."
	case catalog.Added:
		if !inCatalog || !s.HasTime() {
			return "New in the catalog."
		}
		for _, h := range in.held() {
			if h.Overlaps(s) {
				return fmt.Sprintf("New. Overlaps your %s; worth it? Swap it in your plan.", h.Code)
			}
		}
		return "New, and it fits around your reservations: add it to your plan."
	case catalog.Rescheduled, catalog.Relocated:
		if !held[c.SessionID] || !inCatalog {
			return "Changed: run plan to re-check your schedule."
		}
		day := s.Start.In(in.loc()).Format("2006-01-02")
		for _, is := range Tonight(in, day) {
			if is.Code == s.Code || is.Other == s.Code {
				return "Now a problem: " + is.Code + " " + is.Problem + ". Consider: " + is.Suggest + "."
			}
		}
		return "Still fits your day. Walking blocks will be updated by the next plan/apply."
	case catalog.Retitled:
		return "Renamed."
	}
	return ""
}

// Summary is a one-line description of a change for terminals and agents.
func (im Impact) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", im.Code, im.Kind)
	if im.Before != "" && im.After != "" {
		fmt.Fprintf(&b, ": %s → %s", im.Before, im.After)
	} else if im.After != "" {
		fmt.Fprintf(&b, ": %s", im.After)
	} else if im.Before != "" {
		fmt.Fprintf(&b, " (was %s)", im.Before)
	}
	return b.String()
}
