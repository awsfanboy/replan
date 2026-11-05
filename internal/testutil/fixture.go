// Package testutil builds a small re:Invent-shaped catalog for tests: several
// venues, repeats, full sessions and walk-ups, on 2026-12-01 in Las Vegas.
package testutil

import (
	"time"

	"github.com/awsfanboy/replan/internal/api"
	"github.com/awsfanboy/replan/internal/catalog"
)

type S struct {
	Code, Type, Room, Clock, Seats string
	Minutes                        int
	Day                            string // default 2026-12-01
}

func Catalog(ss ...S) *catalog.Catalog {
	ev := api.Event{EventID: "reinvent2026", Name: "re:Invent 2026", Timezone: "America/Los_Angeles",
		StartDate: "2026-11-30T05:30:00.000-08:00", AuthenticationRequired: true}
	var raw []api.Session
	for _, s := range ss {
		day := s.Day
		if day == "" {
			day = "2026-12-01"
		}
		mins := s.Minutes
		if mins == 0 {
			mins = 60
		}
		typ := s.Type
		if typ == "" {
			typ = "Breakout session"
		}
		seats := s.Seats
		if seats == "" {
			seats = api.SeatAvailable
		}
		raw = append(raw, api.Session{
			SessionID: "id-" + s.Code, Abbreviation: s.Code, Title: "Session " + s.Code, Type: typ, Room: s.Room,
			IsReservable: ptr(seats != api.SeatWalkUp), SeatAvailability: seats,
			SessionTime: &api.SessionTime{Date: day, Time: s.Clock, Length: itoa(mins), Timezone: "America/Los_Angeles"},
		})
	}
	c, err := catalog.Build(ev, raw, time.Now())
	if err != nil {
		panic(err)
	}
	return c
}

// Now is the day before the fixture event.
var Now = time.Date(2026, 11, 29, 12, 0, 0, 0, time.UTC)

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func ptr[T any](v T) *T { return &v }
