package apply_test

import (
	"context"
	"testing"

	"github.com/awsfanboy/replan/internal/api"
	"github.com/awsfanboy/replan/internal/apply"
	"github.com/awsfanboy/replan/internal/plan"
	"github.com/awsfanboy/replan/internal/sim"
	"github.com/awsfanboy/replan/internal/spec"
	tu "github.com/awsfanboy/replan/internal/testutil"
)

var campus = tu.Catalog(
	tu.S{Code: "AIM301", Room: "Wynn | Lafite 1", Clock: "09:00"},
	tu.S{Code: "AIM301-R", Room: "Wynn | Lafite 2", Clock: "14:00"},
	tu.S{Code: "DEV402", Room: "Caesars Forum | Academy 410", Clock: "11:00"},
	tu.S{Code: "SVS401", Room: "Encore | Bluethorn 1", Clock: "16:00"},
)

const yaml = `
event: reinvent2026
sessions:
  - reserve: AIM301
    fallbacks: [AIM301-R]
  - reserve: DEV402
favorites: [SVS401]
`

func run(t *testing.T, s *sim.Sim) (*apply.Result, []apply.Event) {
	t.Helper()
	sp, err := spec.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sched, _ := s.GetSchedule(ctx, "reinvent2026")
	in := plan.Input{Spec: sp, Catalog: campus, Schedule: sched, Now: tu.Now}
	var events []apply.Event
	r := &apply.Runner{API: s, Input: in, Progress: func(e apply.Event) { events = append(events, e) }}
	res, err := r.Run(ctx, plan.Build(in))
	if err != nil {
		t.Fatal(err)
	}
	// Whatever happened, a fresh plan must be clean or explain itself.
	sched, _ = s.GetSchedule(ctx, "reinvent2026")
	in.Schedule = sched
	if p := plan.Build(in); !p.Empty() && !s.ReservationsClosed {
		t.Errorf("plan after apply is not empty: %+v", p.Actions)
	}
	return res, events
}

func newSim(t *testing.T) *sim.Sim {
	s, err := sim.New(campus, "")
	if err != nil {
		t.Fatal(err)
	}
	s.Now = tu.Now
	return s
}

func TestFallsBackWhenServerSaysFull(t *testing.T) {
	s := newSim(t)
	s.Full["AIM301"] = true // the catalog still says "available": stale band
	res, _ := run(t, s)
	final := map[string]bool{}
	for _, id := range res.Final.Reserved {
		final[id] = true
	}
	if !final["id-AIM301-R"] || !final["id-DEV402"] || final["id-AIM301"] {
		t.Fatalf("reserved %v", res.Final.Reserved)
	}
	if len(res.Final.PersonalTime) == 0 {
		t.Fatal("expected travel blocks computed from the seats actually won")
	}
}

func TestLostReserveResponseIsReconciled(t *testing.T) {
	s := newSim(t)
	s.LoseResponse["ReserveSessions"] = 1 // first reservation succeeds, reply lost
	res, events := run(t, s)
	if len(res.Final.Reserved) != 2 {
		t.Fatalf("reserved %v", res.Final.Reserved)
	}
	reconciled := false
	for _, e := range events {
		if e.Kind == apply.EvInfo {
			reconciled = true
		}
	}
	if !reconciled {
		t.Error("expected a reconcile-from-GetSchedule event")
	}
}

func TestLostCreateIsNotDuplicated(t *testing.T) {
	s := newSim(t)
	s.LoseResponse["CreatePersonalTime"] = 1
	res, _ := run(t, s) // run() also asserts the follow-up plan is clean
	if n := len(res.Final.PersonalTime); n != 1 {
		t.Fatalf("want exactly 1 travel block, got %d", n)
	}
}

func TestReservationsClosedStillAppliesFavorites(t *testing.T) {
	s := newSim(t)
	s.ReservationsClosed = true
	res, _ := run(t, s)
	if !res.ReservationsOff {
		t.Fatal("want ReservationsOff")
	}
	if len(res.Final.Reserved) != 0 || len(res.Final.Favorites) != 1 {
		t.Fatalf("reserved %v favorites %v", res.Final.Reserved, res.Final.Favorites)
	}
}

var _ api.ScheduleService = (*sim.Sim)(nil)

// A session that can't be reserved (full, no fallback) must not get walking
// blocks to or from it.
func TestNoWalkingBlocksForUnreservedSessions(t *testing.T) {
	s := newSim(t)
	s.Full["DEV402"] = true
	sp, _ := spec.Parse([]byte("event: reinvent2026\nsessions:\n  - reserve: AIM301\n  - reserve: DEV402\n  - reserve: SVS401\n"))
	ctx := context.Background()
	sched, _ := s.GetSchedule(ctx, "reinvent2026")
	in := plan.Input{Spec: sp, Catalog: campus, Schedule: sched, Now: tu.Now}
	r := &apply.Runner{API: s, Input: in}
	res, err := r.Run(ctx, plan.Build(in))
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, pt := range res.Final.PersonalTime {
		titles = append(titles, pt.Title)
	}
	// AIM301 (Wynn) and SVS401 (Encore) are held; DEV402 (Caesars Forum) is not.
	if len(titles) != 1 || titles[0] != "Walk: Wynn → Encore" {
		t.Fatalf("walking blocks = %v, want only Wynn → Encore", titles)
	}
}
