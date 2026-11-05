package catalog

import (
	"testing"
	"time"

	"github.com/awsfanboy/replan/internal/api"
)

func TestNormalizeKind(t *testing.T) {
	for raw, want := range map[string]string{
		"Breakout session": KindBreakout, "Breakout Session": KindBreakout, "Breakout": KindBreakout,
		"Sesión grupal": KindBreakout, "브레이크아웃 세션": KindBreakout, "Chalk talk": KindChalkTalk,
		"Builders' session": KindBuilders, "Lightning Talk": KindLightning, "Charlas relámpago": KindLightning,
		"Code talk": KindCodeTalk, "Workshop": KindWorkshop, "Something new": KindOther,
	} {
		if got := NormalizeKind(raw); got != want {
			t.Errorf("NormalizeKind(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestNormalizeSession(t *testing.T) {
	// The event says Europe/Ulyanovsk (as one real event in the API does); the
	// session's own timezone must win.
	wrong, _ := time.LoadLocation("Europe/Ulyanovsk")
	s, err := Normalize(api.Session{
		SessionID: "x", Abbreviation: "AIM301-R1", Type: "Chalk talk", Room: "Caesars Forum | Level 1 | Academy 410",
		Speakers:    []api.Speaker{{Name: "Ruba Doe , Snr Technical Instructor, AWS"}, {Name: "Solo Name"}},
		SessionTime: &api.SessionTime{Date: "2026-09-30", Time: "09:00", Length: "70", Timezone: "Asia/Dubai"},
	}, wrong)
	if err != nil {
		t.Fatal(err)
	}
	if s.BaseCode != "AIM301" || s.Venue != "Caesars Forum" || s.Recorded {
		t.Errorf("base=%q venue=%q recorded=%v", s.BaseCode, s.Venue, s.Recorded)
	}
	if p := s.People[0]; p.Name != "Ruba Doe" || p.Title != "Snr Technical Instructor" || p.Company != "AWS" {
		t.Errorf("speaker = %+v", p)
	}
	if got := s.Start.UTC().Format(time.RFC3339); got != "2026-09-30T05:00:00Z" {
		t.Errorf("start = %s", got)
	}
	if s.End.Sub(s.Start) != 70*time.Minute {
		t.Errorf("length = %s", s.End.Sub(s.Start))
	}
}

func TestDiff(t *testing.T) {
	ev := api.Event{EventID: "e", Timezone: "America/Los_Angeles"}
	mk := func(id, room, clock string) api.Session {
		return api.Session{SessionID: id, Abbreviation: id, Title: "T " + id, Room: room,
			SessionTime: &api.SessionTime{Date: "2026-12-01", Time: clock, Length: "60"}}
	}
	old, _ := Build(ev, []api.Session{mk("A", "Wynn | 1", "09:00"), mk("B", "Encore | 2", "10:00"), mk("C", "Wynn | 3", "11:00")}, time.Now())
	n := mk("N", "Venetian | 4", "12:00")
	n.Title = "NEW LAUNCH: something"
	cur, _ := Build(ev, []api.Session{mk("A", "Wynn | 1", "10:30"), mk("B", "MGM Grand | 9", "10:00"), n}, time.Now())
	got := map[string]string{}
	for _, c := range Diff(old, cur, time.Now()) {
		got[c.Code+" "+c.Kind] = c.After
		if c.Code == "N" && !c.NewLaunch {
			t.Error("N should be flagged as a new launch")
		}
	}
	for _, k := range []string{"A rescheduled", "B relocated", "C removed", "N added"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing %s in %v", k, got)
		}
	}
}
