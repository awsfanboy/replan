package catalog

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/awsfanboy/replan/internal/api"
	"github.com/awsfanboy/replan/internal/venues"
)

// Session is a catalog session with the API's rough edges smoothed out.
type Session struct {
	api.Session

	Code     string    // public code (abbreviation), falling back to sessionId
	BaseCode string    // code without repeat/sponsor suffixes, to link repeats
	Kind     string    // canonical session type, see kinds below
	Venue    string    // canonical venue, recovered from room when needed
	Start    time.Time // absolute instant
	End      time.Time
	Recorded bool     // likely to be published as a recording
	People   []Person // speakers split into name/title/company
}

type Person struct {
	Name, Title, Company string
}

// Canonical kinds. The raw `type` field varies in case, wording and language
// ("Breakout session", "Breakout Session", "Breakout", "Sesión grupal", ...).
const (
	KindBreakout   = "breakout"
	KindChalkTalk  = "chalk-talk"
	KindWorkshop   = "workshop"
	KindBuilders   = "builders-session"
	KindCodeTalk   = "code-talk"
	KindLightning  = "lightning-talk"
	KindKeynote    = "keynote"
	KindInnovation = "innovation-talk"
	KindDevChat    = "dev-chat"
	KindGameDay    = "gameday"
	KindJam        = "jam"
	KindHackathon  = "hackathon"
	KindLab        = "lab"
	KindGamified   = "gamified-learning"
	KindBootcamp   = "bootcamp"
	KindExamPrep   = "exam-prep"
	KindOther      = "other"
)

var kindRules = []struct {
	match []string
	kind  string
}{
	{[]string{"chalk"}, KindChalkTalk},
	{[]string{"workshop", "taller", "워크숍"}, KindWorkshop},
	{[]string{"builder"}, KindBuilders},
	{[]string{"code talk", "code-talk"}, KindCodeTalk},
	{[]string{"lightning", "relámpago", "relampago", "라이트닝"}, KindLightning},
	{[]string{"keynote", "기조"}, KindKeynote},
	{[]string{"innovation talk"}, KindInnovation},
	{[]string{"dev chat"}, KindDevChat},
	{[]string{"gameday"}, KindGameDay},
	{[]string{"jam"}, KindJam},
	{[]string{"hackathon"}, KindHackathon},
	{[]string{"gamified"}, KindGamified},
	{[]string{"bootcamp"}, KindBootcamp},
	{[]string{"exam prep"}, KindExamPrep},
	{[]string{"lab"}, KindLab},
	{[]string{"breakout", "sesión grupal", "sesion grupal", "sessão", "브레이크아웃", "ブレイクアウト"}, KindBreakout},
}

func NormalizeKind(raw string) string {
	l := strings.ToLower(raw)
	for _, r := range kindRules {
		for _, m := range r.match {
			if strings.Contains(l, m) {
				return r.kind
			}
		}
	}
	return KindOther
}

// Recorded kinds: at re:Invent, breakouts, keynotes and innovation talks are
// generally published; chalk talks, workshops, builders' sessions, code talks
// and similar interactive formats are not. This is a heuristic, not API data.
func likelyRecorded(kind string) bool {
	switch kind {
	case KindBreakout, KindKeynote, KindInnovation:
		return true
	}
	return false
}

var suffixRE = regexp.MustCompile(`(?i)-(R\d*|S|NEW)$`)

func baseCode(code string) string {
	for {
		n := suffixRE.ReplaceAllString(code, "")
		if n == code {
			return strings.ToUpper(n)
		}
		code = n
	}
}

// Speaker names arrive as one string such as
// "Jane Doe, Principal SA, AWS" (sometimes with stray spaces before commas).
func parseSpeaker(s string) Person {
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	p := Person{Name: parts[0]}
	switch {
	case len(parts) == 2:
		p.Company = parts[1]
	case len(parts) >= 3:
		p.Title = strings.Join(parts[1:len(parts)-1], ", ")
		p.Company = parts[len(parts)-1]
	}
	return p
}

// Normalize turns a raw session into a Session. eventTZ is the fallback when
// the session carries no timezone of its own. Session timezones win because
// event-level zones are sometimes wrong (e.g. a Dubai event listed as
// Europe/Ulyanovsk while its sessions say Asia/Dubai).
func Normalize(s api.Session, eventTZ *time.Location) (Session, error) {
	n := Session{Session: s}
	n.Code = s.Abbreviation
	if n.Code == "" {
		n.Code = s.SessionID
	}
	n.BaseCode = baseCode(n.Code)
	n.Kind = NormalizeKind(s.Type)
	n.Recorded = likelyRecorded(n.Kind)
	n.Venue = venues.Resolve(s.Venue, s.Room)
	for _, sp := range s.Speakers {
		if sp.Name != "" {
			n.People = append(n.People, parseSpeaker(sp.Name))
		}
	}
	if s.SessionTime == nil || s.SessionTime.Date == "" {
		return n, nil
	}
	loc := eventTZ
	if tz := s.SessionTime.Timezone; tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	if loc == nil {
		loc = time.UTC
	}
	clock := s.SessionTime.Time
	if clock == "" || s.IsAllDaySession {
		clock = "00:00"
	}
	start, err := time.ParseInLocation("2006-01-02 15:04", s.SessionTime.Date+" "+clock, loc)
	if err != nil {
		return n, fmt.Errorf("session %s: bad time %q %q: %w", s.SessionID, s.SessionTime.Date, clock, err)
	}
	mins, _ := strconv.Atoi(strings.TrimSpace(s.SessionTime.Length))
	if s.IsAllDaySession && mins == 0 {
		mins = 24 * 60
	}
	n.Start = start
	n.End = start.Add(time.Duration(mins) * time.Minute)
	return n, nil
}

// HasTime reports whether the session has a scheduled slot.
func (s *Session) HasTime() bool { return !s.Start.IsZero() }

// Overlaps reports whether two sessions overlap in time.
func (s *Session) Overlaps(o *Session) bool {
	return s.HasTime() && o.HasTime() && s.Start.Before(o.End) && o.Start.Before(s.End)
}

func (s *Session) Seats() string {
	if s.IsReservable != nil && !*s.IsReservable && s.SeatAvailability == "" {
		return ""
	}
	return s.SeatAvailability
}
