package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/awsfanboy/replan/internal/api"
	"github.com/awsfanboy/replan/internal/spec"
	"github.com/awsfanboy/replan/internal/venues"
)

// CreatePersonalTime returns no ID and has no idempotency key, so replan tags
// every entry it manages with a marker at the end of the description. The
// marker is how later runs recognise (and update or remove) their own blocks
// without ever touching the attendee's.
var markerRE = regexp.MustCompile(`\[replan:([^\]]+)\]\s*$`)

const utcLayout = "2006-01-02T15:04:05"

// DesiredBlock is a personal-time entry replan wants to exist.
type DesiredBlock struct {
	Key   string
	Input api.PersonalTimeInput
}

func MarkerKey(pt api.PersonalTime) (string, bool) {
	m := markerRE.FindStringSubmatch(pt.Description)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func splitManaged(pts []api.PersonalTime) (managed, own []api.PersonalTime) {
	for _, pt := range pts {
		if _, ok := MarkerKey(pt); ok {
			managed = append(managed, pt)
		} else {
			own = append(own, pt)
		}
	}
	return
}

func FormatUTC(t time.Time) string { return t.UTC().Truncate(time.Minute).Format(utcLayout) }

func ParseUTC(start, end string) (time.Time, time.Time, error) {
	s, err := time.ParseInLocation(utcLayout, start, time.UTC)
	if err != nil {
		return s, s, err
	}
	e, err := time.ParseInLocation(utcLayout, end, time.UTC)
	return s, e, err
}

func describe(text, key string) string {
	suffix := " [replan:" + key + "]"
	max := 250 - len(suffix)
	if len(text) > max {
		text = text[:max-1] + "…"
	}
	return text + suffix
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func round5Up(d time.Duration) time.Duration {
	const f = 5 * time.Minute
	return (d + f - 1) / f * f
}

func round5Down(d time.Duration) time.Duration {
	const f = 5 * time.Minute
	return d / f * f
}

func shortHash(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:4])
}

// DesiredBlocks returns spec blocks plus a travel block for every venue change
// between consecutive sessions on the same day.
func DesiredBlocks(sp *spec.Spec, loc *time.Location, slots []Slot, m *venues.Matrix) []DesiredBlock {
	var out []DesiredBlock
	for _, b := range sp.Blocks {
		st, en, err := b.Times(loc)
		if err != nil {
			continue
		}
		en = st.Add(round5Up(en.Sub(st)))
		key := "b:" + shortHash(b.Day, b.From, b.Title)
		desc := b.Note
		if desc == "" {
			desc = b.Title
		}
		out = append(out, DesiredBlock{Key: key, Input: api.PersonalTimeInput{
			StartDateTime: FormatUTC(st),
			EndDateTime:   FormatUTC(en),
			Title:         truncate(b.Title, 128),
			Description:   describe(desc, key),
			Location:      truncate(b.Location, 255),
		}})
	}
	if !sp.Rules.TravelEnabled() {
		return out
	}
	var sessions []Slot
	for _, s := range slots {
		if s.Session != nil {
			sessions = append(sessions, s)
		}
	}
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].Start.Before(sessions[j].Start) })
	for i := 0; i+1 < len(sessions); i++ {
		a, b := sessions[i], sessions[i+1]
		if a.Start.In(loc).YearDay() != b.Start.In(loc).YearDay() {
			continue
		}
		if a.Venue == "" || b.Venue == "" || a.Venue == b.Venue {
			continue
		}
		gap := b.Start.Sub(a.End)
		length := round5Up(m.Travel(a.Venue, b.Venue))
		if length > gap {
			length = round5Down(gap)
		}
		if length <= 0 {
			continue
		}
		start := a.End.Truncate(time.Minute)
		key := fmt.Sprintf("t:%s>%s", a.Session.Code, b.Session.Code)
		out = append(out, DesiredBlock{Key: key, Input: api.PersonalTimeInput{
			StartDateTime: FormatUTC(start),
			EndDateTime:   FormatUTC(start.Add(length)),
			Title:         truncate(fmt.Sprintf("Walk: %s → %s", a.Venue, b.Venue), 128),
			Description:   describe(fmt.Sprintf("Travel from %s to %s for %s.", a.Session.Code, b.Venue, b.Session.Code), key),
			Location:      truncate(a.Venue+" → "+b.Venue, 255),
		}})
	}
	return out
}

// DiffBlocks reconciles managed entries with the desired set. Duplicates of a
// key (e.g. from a create whose response was lost) are removed.
func DiffBlocks(existing []api.PersonalTime, desired []DesiredBlock) []Action {
	byKey := map[string][]api.PersonalTime{}
	for _, pt := range existing {
		k, _ := MarkerKey(pt)
		byKey[k] = append(byKey[k], pt)
	}
	var creates, updates, deletes []Action
	want := map[string]bool{}
	for _, d := range desired {
		d := d
		want[d.Key] = true
		have := byKey[d.Key]
		if len(have) == 0 {
			creates = append(creates, Action{Kind: CreateBlock, Block: &d.Input, BlockKey: d.Key})
			continue
		}
		if !same(have[0], d.Input) {
			updates = append(updates, Action{Kind: UpdateBlock, Block: &d.Input, BlockID: have[0].PersonalTimeID, BlockKey: d.Key})
		}
		for _, dup := range have[1:] {
			deletes = append(deletes, Action{Kind: DeleteBlock, BlockID: dup.PersonalTimeID, BlockKey: d.Key, Reason: "duplicate",
				Block: &api.PersonalTimeInput{Title: dup.Title, StartDateTime: dup.StartDateTime, EndDateTime: dup.EndDateTime}})
		}
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if want[k] {
			continue
		}
		for _, pt := range byKey[k] {
			deletes = append(deletes, Action{Kind: DeleteBlock, BlockID: pt.PersonalTimeID, BlockKey: k, Reason: "no longer needed",
				Block: &api.PersonalTimeInput{Title: pt.Title, StartDateTime: pt.StartDateTime, EndDateTime: pt.EndDateTime}})
		}
	}
	return append(append(deletes, updates...), creates...)
}

func same(pt api.PersonalTime, in api.PersonalTimeInput) bool {
	return pt.StartDateTime == in.StartDateTime && pt.EndDateTime == in.EndDateTime &&
		pt.Title == in.Title && pt.Description == in.Description && pt.Location == in.Location
}
