// Package spec reads reinvent.yaml, the desired state of an attendee's
// schedule: a ranked list of sessions (each with fallbacks), favorites, and
// personal blocks such as meals and meetings.
package spec

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Spec struct {
	Event     string   `yaml:"event"`
	Rules     Rules    `yaml:"rules,omitempty"`
	Sessions  []Want   `yaml:"sessions,omitempty"`
	Favorites []string `yaml:"favorites,omitempty"`
	Blocks    []Block  `yaml:"blocks,omitempty"`
	// Skip lists sessions you will not attend. A held reservation for one is
	// cancelled, so the seat goes back to the walk-up line, and it is never
	// picked again, not even as a fallback.
	Skip []string `yaml:"skip,omitempty,flow"`
}

type Rules struct {
	// MaxPerDay caps reserved sessions per day (0 = no cap).
	MaxPerDay int `yaml:"maxPerDay,omitempty"`
	// Travel adds a personal-time block for every venue change, so the walk
	// shows up in the official re:Invent app.
	Travel *bool `yaml:"travel,omitempty"`
	// Prune cancels reservations and favorites that are not in this file.
	// Off by default: replan leaves anything it doesn't manage alone.
	Prune bool `yaml:"prune,omitempty"`
	// TravelTimes overrides the venue travel matrix. Keys are "default",
	// "sameVenue", or "Venue A <-> Venue B".
	TravelTimes map[string]string `yaml:"travelTimes,omitempty"`
	// ArriveEarly is extra slack before each session on top of travel time
	// (reserved seats are released ~10 minutes before start).
	ArriveEarly string `yaml:"arriveEarly,omitempty"`
}

// Want is one ranked slot: reserve the first of Reserve+Fallbacks that is
// available and fits.
type Want struct {
	Reserve   string   `yaml:"reserve"`
	Fallbacks []string `yaml:"fallbacks,omitempty,flow"`
	Why       string   `yaml:"why,omitempty"`
}

func (w Want) Candidates() []string {
	return append([]string{w.Reserve}, w.Fallbacks...)
}

// Block is personal time in event-local time.
type Block struct {
	Title    string `yaml:"title"`
	Day      string `yaml:"day"`  // YYYY-MM-DD
	From     string `yaml:"from"` // HH:MM
	To       string `yaml:"to"`   // HH:MM
	Location string `yaml:"location,omitempty"`
	Note     string `yaml:"note,omitempty"`
}

func (r Rules) TravelEnabled() bool { return r.Travel == nil || *r.Travel }

func (r Rules) ArriveEarlyDuration() time.Duration {
	if r.ArriveEarly == "" {
		return 0
	}
	d, _ := time.ParseDuration(r.ArriveEarly)
	return d
}

func Load(path string) (*Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(b []byte) (*Spec, error) {
	var s Spec
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("reinvent.yaml: %w", err)
	}
	return &s, s.Validate()
}

func (s *Spec) Validate() error {
	var errs []error
	if s.Event == "" {
		errs = append(errs, errors.New("event is required (e.g. reinvent2026)"))
	}
	seen := map[string]int{}
	for i, w := range s.Sessions {
		if strings.TrimSpace(w.Reserve) == "" {
			errs = append(errs, fmt.Errorf("sessions[%d]: reserve is required", i))
		}
		for _, c := range w.Candidates() {
			k := strings.ToUpper(c)
			if j, dup := seen[k]; dup && j != i {
				errs = append(errs, fmt.Errorf("sessions[%d]: %s already listed in sessions[%d]", i, c, j))
			}
			seen[k] = i
		}
	}
	for i, b := range s.Blocks {
		if b.Title == "" || b.Day == "" || b.From == "" || b.To == "" {
			errs = append(errs, fmt.Errorf("blocks[%d]: title, day, from and to are required", i))
			continue
		}
		if _, _, err := b.Times(time.UTC); err != nil {
			errs = append(errs, fmt.Errorf("blocks[%d]: %w", i, err))
		}
	}
	for k, v := range s.Rules.TravelTimes {
		if _, err := time.ParseDuration(v); err != nil {
			errs = append(errs, fmt.Errorf("rules.travelTimes[%q]: %w", k, err))
		}
	}
	if s.Rules.ArriveEarly != "" {
		if _, err := time.ParseDuration(s.Rules.ArriveEarly); err != nil {
			errs = append(errs, fmt.Errorf("rules.arriveEarly: %w", err))
		}
	}
	return errors.Join(errs...)
}

// Times resolves a block to absolute instants in loc.
func (b Block) Times(loc *time.Location) (time.Time, time.Time, error) {
	start, err := time.ParseInLocation("2006-01-02 15:04", b.Day+" "+b.From, loc)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("bad day/from: %w", err)
	}
	end, err := time.ParseInLocation("2006-01-02 15:04", b.Day+" "+b.To, loc)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("bad day/to: %w", err)
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, errors.New("to must be after from")
	}
	return start, end, nil
}

// Starter returns a commented starter file for `replan init`.
func Starter(event string, examples []string) string {
	var ex strings.Builder
	for i, e := range examples {
		if i == 0 {
			fmt.Fprintf(&ex, "  - reserve: %s\n    why: \"top priority\"\n", e)
			continue
		}
		fmt.Fprintf(&ex, "  - reserve: %s\n", e)
	}
	if ex.Len() == 0 {
		ex.WriteString("  - reserve: AIM301\n    fallbacks: [AIM301-R]\n    why: \"top priority\"\n")
	}
	return fmt.Sprintf(`# replan: your re:Invent schedule as code.
#   replan plan    shows what would change
#   replan apply   makes it so (asks first)
event: %s

rules:
  maxPerDay: 4        # sessions per day, leave room to breathe
  travel: true        # add walking blocks to your official schedule
  arriveEarly: 10m    # reserved seats are released ~10 min before start
  prune: false        # true = cancel reservations not listed here

# Ranked: first is most important. Each entry reserves ONE seat: the first of
# reserve/fallbacks that is not full and fits around higher-ranked entries.
sessions:
%s
# Interest only; favorites never hold a seat.
favorites: []

# Personal time in event-local time. Shows up in the official app.
blocks: []
#  - title: Lunch
#    day: 2026-12-01
#    from: "12:00"
#    to: "13:00"
#    location: Wynn
`, event, ex.String())
}

// Save writes the spec, keeping the previous file as <path>.bak. Comments in
// the previous file are not carried over (the backup keeps them).
func (s *Spec) Save(path string) error {
	if err := s.Validate(); err != nil {
		return err
	}
	var buf strings.Builder
	buf.WriteString("# replan: your re:Invent schedule as code (see `replan plan`).\n")
	buf.WriteString("# sessions are ranked: #1 matters most. Each entry reserves ONE seat.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(s); err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".bak", old, 0o644); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(buf.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
