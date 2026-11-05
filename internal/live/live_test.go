package live

import (
	"strings"
	"testing"
	"time"

	"github.com/awsfanboy/replan/internal/api"
	"github.com/awsfanboy/replan/internal/catalog"
	"github.com/awsfanboy/replan/internal/spec"
	tu "github.com/awsfanboy/replan/internal/testutil"
)

var day = tu.Catalog(
	tu.S{Code: "A1", Room: "Wynn | Lafite 1", Clock: "09:00"},
	tu.S{Code: "B1", Room: "Encore | Bluethorn 1", Clock: "11:00"},
	tu.S{Code: "C1", Room: "Caesars Forum | Academy 410", Clock: "13:00"},
	tu.S{Code: "C2", Room: "MGM Grand | Room 116", Clock: "13:30"}, // reachable alternative from MGM
	tu.S{Code: "C3", Room: "Caesars Forum | Academy 411", Clock: "13:00"},
	tu.S{Code: "A1-R", Room: "Wynn | Lafite 2", Clock: "15:00"},
)

func init() {
	for _, s := range day.Sessions {
		s.Topics = []string{"Serverless"}
	}
}

func at(clock string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02 15:04", "2026-12-01 "+clock, day.Location)
	return t
}

func in(reserved ...string) Input {
	var ids []string
	for _, c := range reserved {
		s, _ := day.Lookup(c)
		ids = append(ids, s.SessionID)
	}
	return Input{Catalog: day, Schedule: &api.Schedule{Reserved: ids}}
}

func TestNextWhenRunningLate(t *testing.T) {
	i := in("A1", "C1")
	i.Now, i.At, i.Late = at("12:30"), "MGM Grand", 15*time.Minute
	r := Next(i)
	if len(r.Legs) != 1 || r.Legs[0].Session.Code != "C1" || r.Legs[0].Status != WontMake {
		t.Fatalf("legs: %+v", r.Legs)
	}
	// 12:45 + 35m from MGM = 13:20 > 13:00. C2 at MGM 13:30 is reachable; C3 at the Forum is not.
	alts := r.Legs[0].Alternatives
	if len(alts) == 0 || alts[0].Code != "C2" {
		t.Fatalf("alternatives: %v", codes(alts))
	}
	for _, a := range alts {
		if a.Code == "C3" {
			t.Fatal("C3 is not reachable in time")
		}
	}
	if !strings.Contains(strings.Join(r.Advice, " "), "skip C1") {
		t.Fatalf("advice should suggest giving the seat back: %v", r.Advice)
	}
}

func TestNextOnTrackAndInProgress(t *testing.T) {
	i := in("A1", "B1")
	i.Now = at("09:30")
	r := Next(i)
	if r.Legs[0].Status != InProgress || r.Legs[1].Status != OnTrack || r.At != "Wynn" {
		t.Fatalf("legs: %+v at %q", r.Legs, r.At)
	}
	// Leave by: 11:00 - 10m seat release - 5m Wynn→Encore = 10:45.
	if got := r.Legs[1].LeaveBy.In(day.Location).Format("15:04"); got != "10:45" {
		t.Fatalf("leave by %s", got)
	}
}

func TestTonightFindsSeatsToGiveBack(t *testing.T) {
	i := in("A1", "A1-R", "B1", "C1", "C3")
	i.Spec = &spec.Spec{Sessions: []spec.Want{{Reserve: "C1"}, {Reserve: "A1"}}}
	issues := Tonight(i, "2026-12-01")
	got := map[string]string{}
	for _, is := range issues {
		got[is.Code] = is.Problem
	}
	if !strings.Contains(got["A1-R"], "another run") {
		t.Errorf("A1-R is a repeat of A1: %v", got)
	}
	if !strings.Contains(got["C3"], "overlaps C1") {
		t.Errorf("C3 overlaps the higher-ranked C1: %v", got)
	}
	if _, ok := got["C1"]; ok {
		t.Errorf("C1 is ranked #1 and must be kept: %v", got)
	}
}

func TestImpactsPersonalize(t *testing.T) {
	i := in("A1", "B1")
	cur := day
	changes := []catalog.Change{
		{Kind: catalog.Rescheduled, SessionID: "id-B1", Code: "B1"},
		{Kind: catalog.Added, SessionID: "id-C2", Code: "C2"},
		{Kind: catalog.Removed, SessionID: "id-gone", Code: "ZZ9"},
	}
	imps, unrelated := Impacts(Input{Catalog: cur, Schedule: i.Schedule}, changes)
	if len(imps) != 2 || imps[0].Relevance != YourReservation || imps[1].Relevance != YourInterests || unrelated != 1 {
		t.Fatalf("impacts %+v unrelated %d", imps, unrelated)
	}
}

func codes(ss []*catalog.Session) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.Code)
	}
	return out
}

func TestNextMissedWhenElsewhere(t *testing.T) {
	i := in("A1", "B1")
	i.Now, i.At = at("09:20"), "MGM Grand" // A1 is under way at the Wynn
	r := Next(i)
	if r.Legs[0].Status != Missed || r.Legs[1].Status != OnTrack {
		t.Fatalf("legs: %+v", r.Legs)
	}
}
