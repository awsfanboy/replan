// Package app wires configuration to the catalog, the schedule backend (the
// real API or the simulator) and sign-in. The CLI and the MCP server share it.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/awsfanboy/replan/internal/api"
	"github.com/awsfanboy/replan/internal/auth"
	"github.com/awsfanboy/replan/internal/catalog"
	"github.com/awsfanboy/replan/internal/sim"
	"github.com/awsfanboy/replan/internal/spec"
)

type Config struct {
	SpecPath    string  // reinvent.yaml
	Event       string  // overrides the spec's event
	Sim         bool    // use the simulator instead of the real schedule
	SimFullRate float64 // simulator: fraction of sessions that are full
	SimFull     []string
	SimReset    bool
	SimClosed   bool
	SimOpen     bool   // simulator: ignore the catalog's real seat bands
	CatalogFile string // snapshot instead of the API
	Refresh     bool
	// CompareFile is an older catalog snapshot to diff against for "what
	// changed" (demos); without it the recorded change history is used.
	CompareFile string
	// Now overrides the clock ("time travel"): plan and live checks behave
	// as if it were this moment. The simulator uses it too.
	Now time.Time
}

type App struct {
	Cfg Config

	mu       sync.Mutex
	client   *api.Client
	catalogs map[string]*catalog.Catalog
	sims     map[string]*sim.Sim
}

func New(cfg Config) *App {
	if cfg.SpecPath == "" {
		cfg.SpecPath = "reinvent.yaml"
	}
	return &App{Cfg: cfg, catalogs: map[string]*catalog.Catalog{}, sims: map[string]*sim.Sim{}}
}

// optionalTokens sends a token when signed in and nothing otherwise, so public
// catalogs work without an account.
type optionalTokens struct{ src *auth.Source }

func (o optionalTokens) Token(ctx context.Context, force bool) (string, error) {
	t, err := o.src.Token(ctx, force)
	if errors.Is(err, auth.ErrNotSignedIn) {
		return "", nil
	}
	return t, err
}

func TokenStore() (*auth.Store, error) { return auth.DefaultStore() }

func (a *App) Client() *api.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil {
		st, err := auth.DefaultStore()
		if err != nil {
			a.client = api.NewClient(nil)
		} else {
			a.client = api.NewClient(optionalTokens{&auth.Source{Store: st}})
		}
	}
	return a.client
}

// ResolveEvent picks the event: explicit argument, config, then the spec file.
func (a *App) ResolveEvent(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if a.Cfg.Event != "" {
		return a.Cfg.Event, nil
	}
	if sp, err := spec.Load(a.Cfg.SpecPath); err == nil && sp.Event != "" {
		return sp.Event, nil
	}
	return "", errors.New("no event given and no reinvent.yaml with an event; pass an event ID (e.g. reinvent2026)")
}

// Catalog loads (and memoizes) an event's catalog.
func (a *App) Catalog(ctx context.Context, event string) (*catalog.Catalog, error) {
	a.mu.Lock()
	c, ok := a.catalogs[event]
	a.mu.Unlock()
	if ok {
		return c, nil
	}
	var err error
	if a.Cfg.CatalogFile != "" {
		c, err = catalog.LoadFile(a.Cfg.CatalogFile)
	} else {
		c, err = catalog.Load(ctx, a.Client(), event, catalog.LoadOptions{MaxAge: time.Hour, Refresh: a.Cfg.Refresh})
	}
	if api.IsKind(err, api.KindAuthRequired) {
		return nil, fmt.Errorf("%s requires registration: run `replan login` first (%w)", event, err)
	}
	if api.IsKind(err, api.KindNotRegistered) {
		return nil, fmt.Errorf("you are signed in but not registered for %s (%w)", event, err)
	}
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.catalogs[event] = c
	a.mu.Unlock()
	return c, nil
}

// SetCatalog injects a catalog (tests).
func (a *App) SetCatalog(c *catalog.Catalog) {
	a.mu.Lock()
	a.catalogs[c.Event.EventID] = c
	a.mu.Unlock()
}

// EventStart parses the event start date.
func EventStart(cat *catalog.Catalog) time.Time {
	for _, layout := range []string{"2006-01-02T15:04:05.000Z07:00", time.RFC3339} {
		if t, err := time.Parse(layout, cat.Event.StartDate); err == nil {
			return t
		}
	}
	for _, s := range cat.Sessions {
		if s.HasTime() {
			return s.Start
		}
	}
	return time.Now()
}

// Backend returns the schedule API (real or simulated) and the clock to plan with.
func (a *App) Backend(cat *catalog.Catalog) (api.ScheduleService, time.Time, error) {
	if !a.Cfg.Sim {
		if !a.Cfg.Now.IsZero() {
			return a.Client(), a.Cfg.Now, nil
		}
		return a.Client(), time.Now(), nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ev := cat.Event.EventID
	if s, ok := a.sims[ev]; ok {
		return s, s.Now, nil
	}
	path := filepath.Join(filepath.Dir(a.Cfg.SpecPath), ".replan", "sim-"+ev+".json")
	if a.Cfg.SimReset {
		_ = os.Remove(path)
	}
	s, err := sim.New(cat, path)
	if err != nil {
		return nil, time.Time{}, err
	}
	// Pretend it's the day before the event so every session is in the future.
	s.Now = EventStart(cat).Add(-24 * time.Hour)
	if !a.Cfg.Now.IsZero() {
		s.Now = a.Cfg.Now
	}
	s.FullRate = a.Cfg.SimFullRate
	s.ReservationsClosed = a.Cfg.SimClosed
	s.OpenSeats = a.Cfg.SimOpen
	for _, c := range a.Cfg.SimFull {
		if c = strings.TrimSpace(c); c != "" {
			s.Full[strings.ToUpper(c)] = true
		}
	}
	a.sims[ev] = s
	return s, s.Now, nil
}

// Sim returns the simulator for an event if one is in use (tests).
func (a *App) Sim(event string) *sim.Sim {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sims[event]
}

func (a *App) Mode() string {
	if a.Cfg.Sim {
		return "SIMULATOR: nothing real is changed"
	}
	return "live schedule"
}

// ParseNow reads a --now value in the event's timezone: "2026-12-01T10:40",
// "2026-12-01 10:40" or "tue 10:40" (weekday of the event week).
func ParseNow(v string, cat *catalog.Catalog) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	for _, l := range []string{"2006-01-02T15:04", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(l, v, cat.Location); err == nil {
			return t, nil
		}
	}
	parts := strings.Fields(strings.ToLower(v))
	if len(parts) == 2 {
		clock, err := time.Parse("15:04", parts[1])
		if err == nil {
			start := EventStart(cat).In(cat.Location)
			for d := 0; d < 8; d++ {
				day := start.AddDate(0, 0, d)
				if strings.HasPrefix(strings.ToLower(day.Weekday().String()), parts[0][:min(3, len(parts[0]))]) {
					return time.Date(day.Year(), day.Month(), day.Day(), clock.Hour(), clock.Minute(), 0, 0, cat.Location), nil
				}
			}
		}
	}
	return time.Time{}, fmt.Errorf("bad time %q: use 2026-12-01T10:40 or \"tue 10:40\"", v)
}

// Refresh refetches an event's catalog (recording what changed since the
// last download) and replaces the memoized copy. With a snapshot file it is
// a no-op.
func (a *App) Refresh(ctx context.Context, event string) (*catalog.Catalog, error) {
	if a.Cfg.CatalogFile != "" {
		return a.Catalog(ctx, event)
	}
	c, err := catalog.Load(ctx, a.Client(), event, catalog.LoadOptions{Refresh: true})
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.catalogs[event] = c
	a.mu.Unlock()
	return c, nil
}

// IgnoreSeatBands reports whether planning should ignore the catalog's seat
// bands (the simulator with open seats).
func (a *App) IgnoreSeatBands() bool { return a.Cfg.Sim && a.Cfg.SimOpen }
