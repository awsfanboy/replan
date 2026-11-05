package catalog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The Events API has no change feed: no updatedAt, no deltas. replan builds
// one locally by comparing each fresh download with the previous one, and
// keeps the history in changes-<event>.jsonl next to the catalog cache.

const (
	Added       = "added"
	Removed     = "removed"
	Rescheduled = "rescheduled"
	Relocated   = "relocated"
	Retitled    = "retitled"
)

type Change struct {
	SeenAt    time.Time `json:"seenAt"`
	Kind      string    `json:"kind"`
	SessionID string    `json:"sessionId"`
	Code      string    `json:"code"`
	Title     string    `json:"title"`
	Before    string    `json:"before,omitempty"`
	After     string    `json:"after,omitempty"`
	NewLaunch bool      `json:"newLaunch,omitempty"` // a session announcing something launched at the event
}

func when(s *Session, loc *time.Location) string {
	if !s.HasTime() {
		return "unscheduled"
	}
	return s.Start.In(loc).Format("Mon 01/02 15:04") + "–" + s.End.In(loc).Format("15:04")
}

func where(s *Session) string {
	room := s.Room
	if i := strings.LastIndex(room, "|"); i >= 0 {
		room = strings.TrimSpace(room[i+1:])
	}
	switch {
	case s.Venue == "":
		return room
	case room == "" || strings.Contains(room, s.Venue):
		return s.Venue
	}
	return s.Venue + " · " + room
}

// IsNewLaunch spots sessions about launches announced at the event. AWS adds
// these after the keynotes; the catalog marks them in the title or abstract.
func IsNewLaunch(s *Session) bool {
	t := strings.ToLower(s.Title + " " + s.Abstract)
	return strings.Contains(t, "new launch") || strings.Contains(t, "launched at re:invent") || strings.HasPrefix(strings.ToLower(s.Title), "[new]")
}

// Diff lists what changed from old to cur.
func Diff(old, cur *Catalog, at time.Time) []Change {
	loc := cur.Location
	var out []Change
	for _, s := range cur.Sessions {
		o, ok := old.byID[s.SessionID]
		if !ok {
			out = append(out, Change{SeenAt: at, Kind: Added, SessionID: s.SessionID, Code: s.Code, Title: s.Title,
				After: when(s, loc) + " · " + where(s), NewLaunch: IsNewLaunch(s)})
			continue
		}
		if !o.Start.Equal(s.Start) || !o.End.Equal(s.End) {
			out = append(out, Change{SeenAt: at, Kind: Rescheduled, SessionID: s.SessionID, Code: s.Code, Title: s.Title,
				Before: when(o, loc), After: when(s, loc)})
		}
		if where(o) != where(s) {
			out = append(out, Change{SeenAt: at, Kind: Relocated, SessionID: s.SessionID, Code: s.Code, Title: s.Title,
				Before: where(o), After: where(s)})
		}
		if o.Title != s.Title {
			out = append(out, Change{SeenAt: at, Kind: Retitled, SessionID: s.SessionID, Code: s.Code, Title: s.Title,
				Before: o.Title, After: s.Title})
		}
	}
	for _, o := range old.Sessions {
		if _, ok := cur.byID[o.SessionID]; !ok {
			out = append(out, Change{SeenAt: at, Kind: Removed, SessionID: o.SessionID, Code: o.Code, Title: o.Title,
				Before: when(o, loc) + " · " + where(o)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

func changesPath(dir, event string) string {
	return filepath.Join(dir, "changes-"+sanitize(event)+".jsonl")
}

func appendChanges(dir, event string, cs []Change) error {
	if len(cs) == 0 {
		return nil
	}
	f, err := os.OpenFile(changesPath(dir, event), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, c := range cs {
		if err := enc.Encode(c); err != nil {
			return err
		}
	}
	return nil
}

// History returns recorded changes for an event seen at or after since.
func History(dir, event string, since time.Time) ([]Change, error) {
	if dir == "" {
		dir = DefaultCacheDir()
	}
	f, err := os.Open(changesPath(dir, event))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Change
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var c Change
		if json.Unmarshal(sc.Bytes(), &c) == nil && !c.SeenAt.Before(since) {
			out = append(out, c)
		}
	}
	return out, sc.Err()
}
