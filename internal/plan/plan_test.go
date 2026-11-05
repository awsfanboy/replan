package plan_test

import (
	"strings"
	"testing"

	"github.com/awsfanboy/replan/internal/api"
	"github.com/awsfanboy/replan/internal/plan"
	"github.com/awsfanboy/replan/internal/spec"
	tu "github.com/awsfanboy/replan/internal/testutil"
)

func mustSpec(t *testing.T, y string) *spec.Spec {
	t.Helper()
	sp, err := spec.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

var campus = tu.Catalog(
	tu.S{Code: "AIM301", Room: "Wynn | Level 2 | Lafite 1", Clock: "09:00"},
	tu.S{Code: "DEV402", Room: "Caesars Forum | Academy 410", Clock: "10:10"}, // 10 min after AIM301 ends
	tu.S{Code: "DEV402-R", Room: "Venetian | Level 5 | Palazzo I", Clock: "11:00"},
	tu.S{Code: "SVS401", Type: "Chalk talk", Room: "Encore | Bluethorn 1", Clock: "13:00", Seats: api.SeatUnavailable},
	tu.S{Code: "SVS401-R", Type: "Chalk talk", Room: "Encore | Bluethorn 2", Clock: "15:00"},
	tu.S{Code: "WALK1", Type: "Lightning talk", Room: "Venetian | Expo", Clock: "16:30", Seats: api.SeatWalkUp},
	tu.S{Code: "EXTRA", Room: "Wynn | Level 1", Clock: "17:00"},
)

func TestFallbacksTravelAndSeats(t *testing.T) {
	sp := mustSpec(t, `
event: reinvent2026
rules: {arriveEarly: 5m}
sessions:
  - reserve: AIM301
  - reserve: DEV402
    fallbacks: [DEV402-R]
  - reserve: SVS401
    fallbacks: [SVS401-R]
  - reserve: WALK1
`)
	p := plan.Build(plan.Input{Spec: sp, Catalog: campus, Schedule: &api.Schedule{}, Now: tu.Now})

	want := map[int]string{1: "AIM301", 2: "DEV402-R", 3: "SVS401-R", 4: "WALK1"}
	for _, pk := range p.Picks {
		got := ""
		if pk.Chosen != nil {
			got = pk.Chosen.Code
		}
		if got != want[pk.Rank] {
			t.Errorf("#%d: chose %q, want %q (skipped %v)", pk.Rank, got, want[pk.Rank], pk.Skipped)
		}
	}
	// DEV402 is 10 minutes after AIM301 but Wynn → Caesars Forum is 25+5.
	if r := p.Picks[1].Skipped[0].Reason; !strings.Contains(r, "too tight") || !strings.Contains(r, "Wynn → Caesars Forum") {
		t.Errorf("DEV402 skip reason = %q", r)
	}
	if r := p.Picks[2].Skipped[0].Reason; r != "full" {
		t.Errorf("SVS401 skip reason = %q", r)
	}
	// WALK1 takes no reservations: planned as a walk-up and favorited, never reserved.
	if !p.Picks[3].WalkUp {
		t.Errorf("WALK1 should be a walk-up pick: %+v", p.Picks[3])
	}
	fav := false
	for _, a := range p.Actions {
		if a.Kind == plan.Reserve && a.SessionID == "id-WALK1" {
			t.Error("a walk-up session must never be reserved")
		}
		fav = fav || (a.Kind == plan.Favorite && a.SessionID == "id-WALK1")
	}
	if !fav {
		t.Error("WALK1 should be favorited so it shows in the app")
	}

	// Travel blocks between each venue change, rounded to 5 minutes, in UTC.
	var travel []string
	for _, a := range p.Actions {
		if a.Kind == plan.CreateBlock {
			travel = append(travel, a.Block.Title+" "+a.Block.StartDateTime+"-"+a.Block.EndDateTime)
		}
	}
	wantTravel := []string{
		"Walk: Wynn → The Venetian 2026-12-01T18:00:00-2026-12-01T18:15:00",
		"Walk: The Venetian → Encore 2026-12-01T20:00:00-2026-12-01T20:15:00",
		"Walk: Encore → The Venetian 2026-12-02T00:00:00-2026-12-02T00:15:00",
	}
	if strings.Join(travel, "\n") != strings.Join(wantTravel, "\n") {
		t.Errorf("travel blocks:\n%s\nwant:\n%s", strings.Join(travel, "\n"), strings.Join(wantTravel, "\n"))
	}
}

func TestKeepsExistingAndLeavesUnmanagedAlone(t *testing.T) {
	sp := mustSpec(t, `
event: reinvent2026
rules: {travel: false}
sessions:
  - reserve: DEV402
    fallbacks: [DEV402-R]
`)
	sched := &api.Schedule{Reserved: []string{"id-DEV402-R", "id-EXTRA"}}
	p := plan.Build(plan.Input{Spec: sp, Catalog: campus, Schedule: sched, Now: tu.Now})
	if !p.Picks[0].Kept || p.Picks[0].Chosen.Code != "DEV402-R" {
		t.Fatalf("want kept DEV402-R, got %+v", p.Picks[0])
	}
	if !p.Empty() {
		t.Fatalf("want no actions, got %+v", p.Actions)
	}

	sp.Rules.Prune = true
	p = plan.Build(plan.Input{Spec: sp, Catalog: campus, Schedule: sched, Now: tu.Now})
	if p.Count(plan.Cancel) != 1 || p.Actions[0].SessionID != "id-EXTRA" {
		t.Fatalf("prune should cancel EXTRA only, got %+v", p.Actions)
	}
}

func TestMaxPerDay(t *testing.T) {
	sp := mustSpec(t, `
event: reinvent2026
rules: {maxPerDay: 1}
sessions:
  - reserve: AIM301
  - reserve: EXTRA
`)
	p := plan.Build(plan.Input{Spec: sp, Catalog: campus, Schedule: &api.Schedule{}, Now: tu.Now})
	if p.Picks[1].Chosen != nil || !strings.Contains(p.Picks[1].Skipped[0].Reason, "maxPerDay") {
		t.Fatalf("EXTRA should hit the day cap: %+v", p.Picks[1])
	}
}

func TestManagedBlocksReconcile(t *testing.T) {
	sp := mustSpec(t, `
event: reinvent2026
rules: {travel: false}
blocks:
  - {title: Lunch, day: 2026-12-01, from: "12:00", to: "12:58", location: Wynn}
`)
	p := plan.Build(plan.Input{Spec: sp, Catalog: campus, Schedule: &api.Schedule{}, Now: tu.Now})
	if p.Count(plan.CreateBlock) != 1 {
		t.Fatalf("want 1 create, got %+v", p.Actions)
	}
	b := p.Actions[0].Block
	if b.StartDateTime != "2026-12-01T20:00:00" || b.EndDateTime != "2026-12-01T21:00:00" {
		t.Errorf("block times = %s..%s (want UTC, rounded up to 5 minutes)", b.StartDateTime, b.EndDateTime)
	}

	// The same block twice (a lost create that was retried) plus an orphan
	// and one of the attendee's own entries.
	mine := api.PersonalTime{PersonalTimeID: "own", StartDateTime: "2026-12-01T16:00:00", EndDateTime: "2026-12-01T17:00:00", Title: "Customer call", Description: "not ours"}
	dup := api.PersonalTime{PersonalTimeID: "a", StartDateTime: b.StartDateTime, EndDateTime: b.EndDateTime, Title: b.Title, Description: b.Description, Location: b.Location}
	dup2 := dup
	dup2.PersonalTimeID = "b"
	orphan := api.PersonalTime{PersonalTimeID: "c", StartDateTime: "2026-12-02T20:00:00", EndDateTime: "2026-12-02T20:30:00", Title: "Old", Description: "x [replan:t:OLD>GONE]"}
	p = plan.Build(plan.Input{Spec: sp, Catalog: campus, Schedule: &api.Schedule{PersonalTime: []api.PersonalTime{mine, dup, dup2, orphan}}, Now: tu.Now})
	var deleted []string
	for _, a := range p.Actions {
		if a.Kind != plan.DeleteBlock {
			t.Errorf("unexpected action %s", a.Kind)
		}
		deleted = append(deleted, a.BlockID)
	}
	if strings.Join(deleted, ",") != "b,c" {
		t.Errorf("deleted %v, want duplicate b and orphan c (never the attendee's own)", deleted)
	}
}

func TestOwnPersonalTimeIsAHardConstraint(t *testing.T) {
	sp := mustSpec(t, `
event: reinvent2026
sessions:
  - reserve: AIM301
`)
	call := api.PersonalTime{PersonalTimeID: "own", StartDateTime: "2026-12-01T17:30:00", EndDateTime: "2026-12-01T18:00:00", Title: "Customer call", Description: "d"}
	p := plan.Build(plan.Input{Spec: sp, Catalog: campus, Schedule: &api.Schedule{PersonalTime: []api.PersonalTime{call}}, Now: tu.Now})
	if p.Picks[0].Chosen != nil || !strings.Contains(p.Picks[0].Skipped[0].Reason, "Customer call") {
		t.Fatalf("AIM301 overlaps the attendee's own call: %+v", p.Picks[0])
	}
}

// Before reserved seating opens the catalog carries no isReservable flag at
// all (re:Invent 2026, Oct 6). That must not read as "not reservable".
func TestMissingReservableFlagIsNotARefusal(t *testing.T) {
	c := tu.Catalog(tu.S{Code: "X1", Room: "Wynn", Clock: "09:00"})
	for _, s := range c.Sessions {
		s.IsReservable, s.SeatAvailability = nil, ""
	}
	sp := mustSpec(t, "event: reinvent2026\nsessions:\n  - reserve: X1\n")
	p := plan.Build(plan.Input{Spec: sp, Catalog: c, Schedule: &api.Schedule{}, Now: tu.Now})
	if p.Picks[0].Chosen == nil {
		t.Fatalf("X1 should be planned: %+v", p.Picks[0].Skipped)
	}
}

func TestSkipGivesTheSeatBack(t *testing.T) {
	sp := mustSpec(t, `
event: reinvent2026
rules: {travel: false}
sessions:
  - reserve: DEV402
    fallbacks: [DEV402-R]
skip: [DEV402]
`)
	sched := &api.Schedule{Reserved: []string{"id-DEV402"}}
	p := plan.Build(plan.Input{Spec: sp, Catalog: campus, Schedule: sched, Now: tu.Now})
	if p.Count(plan.Cancel) != 1 || !p.Actions[0].GiveBack {
		t.Fatalf("want the held DEV402 given back: %+v", p.Actions)
	}
	if p.Picks[0].Chosen == nil || p.Picks[0].Chosen.Code != "DEV402-R" {
		t.Fatalf("the fallback should be reserved instead: %+v", p.Picks[0])
	}
}
