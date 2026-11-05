// Package catalog fetches, caches, normalizes and searches an event's session
// catalog. The API has no search or filter endpoint, so everything here runs
// locally against a full download.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/awsfanboy/replan/internal/api"
)

type Catalog struct {
	Event     api.Event
	Sessions  []*Session
	FetchedAt time.Time
	Location  *time.Location // event timezone (best effort)

	byID   map[string]*Session
	byCode map[string]*Session
}

// Fetcher is the subset of *api.Client that loading needs.
type Fetcher interface {
	GetEvent(ctx context.Context, eventID string) (*api.Event, error)
	ListSessions(ctx context.Context, eventID, locale string, includeAbstracts bool) ([]api.Session, error)
}

type cacheFile struct {
	FetchedAt time.Time     `json:"fetchedAt"`
	Event     api.Event     `json:"event"`
	Sessions  []api.Session `json:"sessions"`
}

type LoadOptions struct {
	CacheDir string        // default: user cache dir / replan
	MaxAge   time.Duration // refetch when the cache is older; 0 means always use cache if present
	Refresh  bool          // ignore the cache
	Offline  bool          // never touch the network
}

func DefaultCacheDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		return ".replan-cache"
	}
	return filepath.Join(d, "replan")
}

// Load returns the catalog, from cache when fresh enough.
func Load(ctx context.Context, f Fetcher, eventID string, o LoadOptions) (*Catalog, error) {
	if o.CacheDir == "" {
		o.CacheDir = DefaultCacheDir()
	}
	path := filepath.Join(o.CacheDir, "catalog-"+sanitize(eventID)+".json")
	var cf *cacheFile
	if !o.Refresh {
		if c, err := readCache(path); err == nil {
			if o.Offline || o.MaxAge == 0 || time.Since(c.FetchedAt) < o.MaxAge {
				cf = c
			}
		}
	}
	if cf == nil {
		if o.Offline {
			return nil, fmt.Errorf("no cached catalog for %s (looked in %s)", eventID, path)
		}
		ev, err := f.GetEvent(ctx, eventID)
		if err != nil {
			return nil, err
		}
		ss, err := f.ListSessions(ctx, eventID, "", true)
		if err != nil {
			return nil, err
		}
		cf = &cacheFile{FetchedAt: time.Now().UTC(), Event: *ev, Sessions: ss}
		// Record what changed since the previous download: the API has no
		// change feed of its own.
		if prev, err := readCache(path); err == nil {
			if oldCat, err := Build(prev.Event, prev.Sessions, prev.FetchedAt); err == nil {
				if newCat, err := Build(cf.Event, cf.Sessions, cf.FetchedAt); err == nil {
					_ = appendChanges(o.CacheDir, eventID, Diff(oldCat, newCat, cf.FetchedAt))
				}
			}
		}
		if err := writeCache(path, cf); err != nil {
			return nil, err
		}
	}
	return Build(cf.Event, cf.Sessions, cf.FetchedAt)
}

// LoadFile builds a catalog from a snapshot file (same format as the cache).
// Used for demos and tests.
func LoadFile(path string) (*Catalog, error) {
	cf, err := readCache(path)
	if err != nil {
		return nil, err
	}
	return Build(cf.Event, cf.Sessions, cf.FetchedAt)
}

// SaveFile writes a snapshot of the catalog.
func (c *Catalog) SaveFile(path string) error {
	raw := make([]api.Session, len(c.Sessions))
	for i, s := range c.Sessions {
		raw[i] = s.Session
	}
	return writeCache(path, &cacheFile{FetchedAt: c.FetchedAt, Event: c.Event, Sessions: raw})
}

func Build(ev api.Event, raw []api.Session, fetched time.Time) (*Catalog, error) {
	c := &Catalog{Event: ev, FetchedAt: fetched, byID: map[string]*Session{}, byCode: map[string]*Session{}}
	if ev.Timezone != "" {
		if l, err := time.LoadLocation(ev.Timezone); err == nil {
			c.Location = l
		}
	}
	for _, s := range raw {
		n, err := Normalize(s, c.Location)
		if err != nil {
			// Keep the session; it just has no usable time.
			n.Start, n.End = time.Time{}, time.Time{}
		}
		sp := &n
		c.Sessions = append(c.Sessions, sp)
		c.byID[s.SessionID] = sp
		c.byCode[strings.ToUpper(sp.Code)] = sp
	}
	// Prefer a session-provided timezone for the event location: event-level
	// zones are occasionally wrong.
	for _, s := range c.Sessions {
		if s.HasTime() && s.SessionTime.Timezone != "" {
			c.Location = s.Start.Location()
			break
		}
	}
	if c.Location == nil {
		c.Location = time.UTC
	}
	sort.SliceStable(c.Sessions, func(i, j int) bool {
		a, b := c.Sessions[i], c.Sessions[j]
		if a.HasTime() != b.HasTime() {
			return a.HasTime() // unscheduled sessions last
		}
		if !a.Start.Equal(b.Start) {
			return a.Start.Before(b.Start)
		}
		return a.Code < b.Code
	})
	return c, nil
}

// Lookup finds a session by sessionId or public code (case-insensitive).
func (c *Catalog) Lookup(ref string) (*Session, bool) {
	if s, ok := c.byID[ref]; ok {
		return s, true
	}
	s, ok := c.byCode[strings.ToUpper(strings.TrimSpace(ref))]
	return s, ok
}

// Repeats returns other sessions sharing this session's base code or title.
func (c *Catalog) Repeats(s *Session) []*Session {
	var out []*Session
	for _, o := range c.Sessions {
		if o == s {
			continue
		}
		if o.BaseCode == s.BaseCode || strings.EqualFold(o.Title, s.Title) {
			out = append(out, o)
		}
	}
	return out
}

// Alternatives returns up to n other sessions starting within 30 minutes of s
// that share at least one topic, service or interest area, most similar
// first. They make good fallbacks when s has no repeat.
func (c *Catalog) Alternatives(s *Session, n int) []*Session {
	if !s.HasTime() {
		return nil
	}
	tags := map[string]bool{}
	for _, t := range concat(s.Topics, s.Services, s.AreasOfInterest) {
		tags[strings.ToLower(t)] = true
	}
	type scored struct {
		s     *Session
		score int
	}
	var out []scored
	for _, o := range c.Sessions {
		if o == s || o.BaseCode == s.BaseCode || !o.HasTime() {
			continue
		}
		d := o.Start.Sub(s.Start)
		if d < -30*time.Minute || d > 30*time.Minute {
			continue
		}
		score := 0
		for _, t := range concat(o.Topics, o.Services, o.AreasOfInterest) {
			if tags[strings.ToLower(t)] {
				score++
			}
		}
		if score == 0 {
			continue
		}
		if o.Kind == s.Kind {
			score++
		}
		out = append(out, scored{o, score})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	var res []*Session
	for i := 0; i < len(out) && i < n; i++ {
		res = append(res, out[i].s)
	}
	return res
}

// Filter narrows a search.
type Filter struct {
	Day             string // YYYY-MM-DD in event time, or weekday name
	Kinds           []string
	Venue           string
	Levels          []string // "200", "300", ...
	Topic           string   // contained in a topic, service, interest area or track
	OnlyLive        bool     // exclude sessions likely to be recorded
	HideUnscheduled bool
}

type Hit struct {
	Session *Session
	Score   int
}

// Search scores sessions against free text. Title and code matches weigh most.
func (c *Catalog) Search(query string, f Filter) []Hit {
	terms := strings.Fields(strings.ToLower(query))
	var hits []Hit
	for _, s := range c.Sessions {
		if !f.match(s, c.Location) {
			continue
		}
		score := 0
		if len(terms) == 0 {
			score = 1
		}
		title := strings.ToLower(s.Title)
		code := strings.ToLower(s.Code)
		abstract := strings.ToLower(s.Abstract)
		tags := strings.ToLower(strings.Join(concat(s.Topics, s.Services, s.AreasOfInterest, s.Tracks, s.Roles), " | "))
		var people []string
		for _, p := range s.People {
			people = append(people, p.Name, p.Company)
		}
		ppl := strings.ToLower(strings.Join(people, " | "))
		all := true
		for _, t := range terms {
			ts := 0
			if strings.Contains(code, t) {
				ts += 10
			}
			if strings.Contains(title, t) {
				ts += 6
			}
			if strings.Contains(tags, t) {
				ts += 3
			}
			if strings.Contains(ppl, t) {
				ts += 3
			}
			if strings.Contains(abstract, t) {
				ts += 1
			}
			if ts == 0 {
				all = false
			}
			score += ts
		}
		if score > 0 && (all || len(terms) > 2) {
			hits = append(hits, Hit{s, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	return hits
}

func (f Filter) match(s *Session, loc *time.Location) bool {
	if f.Day != "" {
		if !s.HasTime() {
			return false
		}
		d := s.Start.In(loc)
		if !strings.EqualFold(d.Format("2006-01-02"), f.Day) && !strings.EqualFold(d.Weekday().String(), f.Day) &&
			!strings.EqualFold(d.Weekday().String()[:3], f.Day) {
			return false
		}
	}
	if len(f.Kinds) > 0 && !contains(f.Kinds, s.Kind) {
		return false
	}
	if f.Venue != "" && !strings.Contains(strings.ToLower(s.Venue), strings.ToLower(f.Venue)) {
		return false
	}
	if len(f.Levels) > 0 {
		ok := false
		for _, l := range f.Levels {
			if strings.HasPrefix(strings.TrimSpace(s.Level), l) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if f.OnlyLive && s.Recorded {
		return false
	}
	if f.HideUnscheduled && !s.HasTime() {
		return false
	}
	if f.Topic != "" {
		t := strings.ToLower(f.Topic)
		ok := false
		for _, x := range concat(s.Topics, s.Services, s.AreasOfInterest, s.Tracks) {
			if strings.Contains(strings.ToLower(x), t) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// Facet is one value of a dimension and how many sessions have it.
type Facet struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// Facets summarizes sessions by day, venue, kind, level and topic, so a
// browser of the catalog can show what is there before searching.
type Facets struct {
	Days   []Facet `json:"days"`
	Venues []Facet `json:"venues"`
	Kinds  []Facet `json:"kinds"`
	Levels []Facet `json:"levels"`
	Topics []Facet `json:"topics"`
}

func (c *Catalog) Facets(ss []*Session) Facets {
	count := func(key func(*Session) []string, byCount bool) []Facet {
		m := map[string]int{}
		var order []string
		for _, s := range ss {
			for _, k := range key(s) {
				if k == "" {
					continue
				}
				if _, ok := m[k]; !ok {
					order = append(order, k)
				}
				m[k]++
			}
		}
		out := make([]Facet, 0, len(order))
		for _, k := range order {
			out = append(out, Facet{k, m[k]})
		}
		if byCount {
			sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
		} else {
			sort.SliceStable(out, func(i, j int) bool { return out[i].Value < out[j].Value })
		}
		return out
	}
	return Facets{
		Days: count(func(s *Session) []string {
			if !s.HasTime() {
				return []string{"unscheduled"}
			}
			return []string{s.Start.In(c.Location).Format("2006-01-02 Mon")}
		}, false),
		Venues: count(func(s *Session) []string { return []string{s.Venue} }, true),
		Kinds:  count(func(s *Session) []string { return []string{s.Kind} }, true),
		Levels: count(func(s *Session) []string {
			if len(s.Level) >= 3 && s.Level[0] >= '1' && s.Level[0] <= '9' {
				return []string{s.Level[:3]}
			}
			return nil
		}, false),
		Topics: count(func(s *Session) []string { return concat(s.Topics) }, true),
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if strings.EqualFold(v, x) {
			return true
		}
	}
	return false
}

func concat(xs ...[]string) []string {
	var out []string
	for _, x := range xs {
		out = append(out, x...)
	}
	return out
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == '.' {
			return '_'
		}
		return r
	}, s)
}

func readCache(path string) (*cacheFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c cacheFile
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.Event.EventID == "" {
		return nil, errors.New("cache file has no event")
	}
	return &c, nil
}

func writeCache(path string, c *cacheFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
