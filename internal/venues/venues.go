// Package venues knows the re:Invent campus: which venue a room string belongs
// to, and how long it takes to get from one venue to another.
//
// The API's `venue` field is almost never populated (1 of 7,571 sessions across
// all public catalogs in Sept 2026), so the venue is recovered from the room
// string. Travel times are door-to-room estimates in minutes, including finding
// the room in a very large hotel; every value can be overridden in reinvent.yaml.
package venues

import (
	"sort"
	"strings"
	"time"
)

const (
	Venetian      = "The Venetian"
	Wynn          = "Wynn"
	Encore        = "Encore"
	CaesarsForum  = "Caesars Forum"
	CaesarsPalace = "Caesars Palace"
	MGMGrand      = "MGM Grand"
	MandalayBay   = "Mandalay Bay"
)

// aliases are matched in order against the lower-cased venue/room text, so
// more specific names come first ("encore" before "wynn", "caesars forum"
// before "caesars").
var aliases = []struct {
	match string
	venue string
}{
	{"caesars forum", CaesarsForum},
	{"caesars palace", CaesarsPalace},
	{"encore", Encore},
	{"wynn", Wynn},
	{"venetian", Venetian},
	{"palazzo", Venetian},
	{"mgm", MGMGrand},
	{"mandalay", MandalayBay},
	{"caesars", CaesarsPalace},
}

// Resolve returns the canonical venue for a session, or "" if unknown (for a
// single-venue event, "" simply means "the venue").
func Resolve(venue, room string) string {
	for _, s := range []string{venue, room} {
		l := strings.ToLower(s)
		if l == "" {
			continue
		}
		for _, a := range aliases {
			if strings.Contains(l, a.match) {
				return a.venue
			}
		}
	}
	return strings.TrimSpace(venue)
}

// Matrix gives travel times between venues.
type Matrix struct {
	SameVenue time.Duration // buffer between two sessions in the same venue
	Default   time.Duration // between two different venues with no specific entry
	pairs     map[[2]string]time.Duration
}

func key(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

func (m *Matrix) Set(a, b string, d time.Duration) {
	if m.pairs == nil {
		m.pairs = map[[2]string]time.Duration{}
	}
	m.pairs[key(a, b)] = d
}

// Travel returns the time needed to get from venue a to venue b. Two unknown
// venues ("") are treated as the same place.
func (m *Matrix) Travel(a, b string) time.Duration {
	if a == b || a == "" || b == "" {
		return m.SameVenue
	}
	if d, ok := m.pairs[key(a, b)]; ok {
		return d
	}
	return m.Default
}

// Known lists venues that have explicit entries, sorted.
func (m *Matrix) Known() []string {
	seen := map[string]bool{}
	for k := range m.pairs {
		seen[k[0]], seen[k[1]] = true, true
	}
	var out []string
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// ReInvent returns estimates for the 2026 campus (Wynn, Encore, The Venetian,
// Caesars Forum, Caesars Palace), plus MGM/Mandalay for older catalogs.
func ReInvent() *Matrix {
	m := &Matrix{SameVenue: 10 * time.Minute, Default: 25 * time.Minute}
	min := func(n int) time.Duration { return time.Duration(n) * time.Minute }
	m.Set(Wynn, Encore, min(5))
	m.Set(Venetian, Wynn, min(15))
	m.Set(Venetian, Encore, min(15))
	m.Set(Venetian, CaesarsForum, min(15))
	m.Set(Venetian, CaesarsPalace, min(15))
	m.Set(CaesarsForum, CaesarsPalace, min(10))
	m.Set(Wynn, CaesarsForum, min(25))
	m.Set(Encore, CaesarsForum, min(25))
	m.Set(Wynn, CaesarsPalace, min(25))
	m.Set(Encore, CaesarsPalace, min(25))
	for _, v := range []string{Venetian, Wynn, Encore, CaesarsForum, CaesarsPalace} {
		m.Set(v, MGMGrand, min(35))
		m.Set(v, MandalayBay, min(45))
	}
	m.Set(MGMGrand, MandalayBay, min(20))
	return m
}
