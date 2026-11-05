// Package ui renders plans, catalogs and apply progress for the terminal.
package ui

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/awsfanboy/replan/internal/apply"
	"github.com/awsfanboy/replan/internal/catalog"
	"github.com/awsfanboy/replan/internal/live"
	"github.com/awsfanboy/replan/internal/plan"
)

type UI struct {
	W     io.Writer
	Color bool
	Loc   *time.Location
}

func New(w io.Writer) *UI {
	color := false
	if f, ok := w.(*os.File); ok && os.Getenv("NO_COLOR") == "" {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			color = true
		}
	}
	return &UI{W: w, Color: color, Loc: time.UTC}
}

func (u *UI) c(code, s string) string {
	if !u.Color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (u *UI) Green(s string) string  { return u.c("32", s) }
func (u *UI) Red(s string) string    { return u.c("31", s) }
func (u *UI) Yellow(s string) string { return u.c("33", s) }
func (u *UI) Cyan(s string) string   { return u.c("36", s) }
func (u *UI) Bold(s string) string   { return u.c("1", s) }
func (u *UI) Dim(s string) string    { return u.c("2", s) }

func (u *UI) P(format string, a ...any) { fmt.Fprintf(u.W, format+"\n", a...) }

func (u *UI) when(s, e time.Time) string {
	if s.IsZero() {
		return "unscheduled"
	}
	s, e = s.In(u.Loc), e.In(u.Loc)
	return fmt.Sprintf("%s %s–%s", s.Format("Mon"), s.Format("15:04"), e.Format("15:04"))
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s + strings.Repeat(" ", n-len(r))
	}
	return string(r[:n-1]) + "…"
}

func (u *UI) Header(cat *catalog.Catalog, mode string) {
	age := strings.TrimSuffix(time.Since(cat.FetchedAt).Round(time.Minute).String(), "0s")
	if age == "" {
		age = "<1m"
	}
	u.P("%s %s %s", u.Bold("replan"), u.Dim("·"), fmt.Sprintf("%s (%s)", cat.Event.Name, cat.Event.EventID))
	u.P("%s", u.Dim(fmt.Sprintf("%d sessions · catalog %s old · times in %s · %s", len(cat.Sessions), age, cat.Location, mode)))
	u.P("")
}

// Plan prints a plan in the spirit of `terraform plan`.
func (u *UI) Plan(p *plan.Plan) {
	actions := map[string]plan.Action{}
	for _, a := range p.Actions {
		if a.Kind == plan.Reserve || a.Kind == plan.Cancel {
			actions[string(a.Kind)+a.SessionID] = a
		}
	}
	if len(p.Picks) > 0 {
		u.P(u.Bold("Ranked sessions"))
		for _, pk := range p.Picks {
			rank := fmt.Sprintf("#%-2d", pk.Rank)
			switch {
			case pk.Kept:
				s := pk.Chosen
				u.P("  %s %s %s %s  %s  %s  %s", rank, u.Dim("="), cut(s.Code, 11), cut(s.Title, 44), cut(u.when(s.Start, s.End), 16), cut(s.Venue, 14), u.Dim(map[bool]string{true: "walk-up, favorited", false: "already reserved"}[pk.WalkUp]))
			case pk.Chosen != nil:
				s := pk.Chosen
				extra := ""
				if !s.Recorded {
					extra = u.Cyan(" ◆ not recorded")
				}
				u.P("  %s %s %s %s  %s  %s  %s%s", rank, u.Green("+"), u.Green(cut(s.Code, 11)), cut(s.Title, 44), cut(u.when(s.Start, s.End), 16), cut(s.Venue, 14), u.Green(map[bool]string{true: "walk-up: favorite", false: "reserve"}[pk.WalkUp]), extra)
			case allSkipped(pk):
				u.P("  %s %s %s %s", rank, u.Dim("–"), u.Dim(cut(pk.Want.Reserve, 11)), u.Dim("skipping: seat given back"))
				continue
			default:
				u.P("  %s %s %s %s", rank, u.Red("!"), u.Red(cut(pk.Want.Reserve, 11)), u.Red("nothing reservable fits"))
			}
			for _, sk := range pk.Skipped {
				u.P("       %s", u.Dim("↳ skipped "+sk.Ref+": "+sk.Reason))
			}
		}
		u.P("")
	}

	var cancels, favs, blocks []plan.Action
	for _, a := range p.Actions {
		switch a.Kind {
		case plan.Cancel:
			cancels = append(cancels, a)
		case plan.Favorite, plan.Unfavorite:
			favs = append(favs, a)
		case plan.CreateBlock, plan.UpdateBlock, plan.DeleteBlock:
			blocks = append(blocks, a)
		}
	}
	if len(cancels) > 0 {
		u.P(u.Bold("Cancellations"))
		for _, a := range cancels {
			title := ""
			if a.Session != nil {
				title = a.Session.Title
			}
			u.P("  %s %s %s %s", u.Red("-"), u.Red(cut(label(a), 11)), cut(title, 44), u.Dim(a.Reason))
		}
		u.P("")
	}
	if len(favs) > 0 {
		u.P(u.Bold("Favorites"))
		for _, a := range favs {
			title := ""
			if a.Session != nil {
				title = a.Session.Title
			}
			sym := u.Green("+")
			if a.Kind == plan.Unfavorite {
				sym = u.Red("-")
			}
			u.P("  %s %s %s", sym, cut(label(a), 11), title)
		}
		u.P("")
	}
	if len(blocks) > 0 {
		u.P(u.Bold("Personal time") + u.Dim("  (shows in the official event app)"))
		for _, a := range blocks {
			st, en, _ := plan.ParseUTC(a.Block.StartDateTime, a.Block.EndDateTime)
			sym, why := u.Green("+"), ""
			switch a.Kind {
			case plan.UpdateBlock:
				sym = u.Yellow("~")
			case plan.DeleteBlock:
				sym, why = u.Red("-"), u.Dim(a.Reason)
			}
			u.P("  %s %s  %s %s", sym, cut(a.Block.Title, 44), cut(u.when(st, en), 16), why)
		}
		u.P("")
	}
	if len(p.Warnings) > 0 {
		u.P(u.Bold("Warnings"))
		for _, w := range p.Warnings {
			u.P("  %s %s", u.Yellow("!"), w)
		}
		u.P("")
	}
	if p.Empty() {
		u.P(u.Green("No changes.") + " Your schedule matches reinvent.yaml.")
		return
	}
	blk := p.Count(plan.CreateBlock) + p.Count(plan.UpdateBlock) + p.Count(plan.DeleteBlock)
	u.P("%s %d to reserve, %d to cancel, %d to favorite, %d personal-time changes.",
		u.Bold("Plan:"), p.Count(plan.Reserve), p.Count(plan.Cancel), p.Count(plan.Favorite)+p.Count(plan.Unfavorite), blk)
}

// Agenda prints the expected schedule day by day.
func (u *UI) Agenda(slots []plan.Slot) {
	if len(slots) == 0 {
		return
	}
	u.P(u.Bold("Your week"))
	byDay := map[string][]plan.Slot{}
	var days []string
	for _, s := range slots {
		d := s.Start.In(u.Loc).Format("2006-01-02 Monday")
		if _, ok := byDay[d]; !ok {
			days = append(days, d)
		}
		byDay[d] = append(byDay[d], s)
	}
	sort.Strings(days)
	for _, d := range days {
		u.P("  %s", u.Cyan(d))
		for _, s := range byDay[d] {
			st, en := s.Start.In(u.Loc), s.End.In(u.Loc)
			tm := fmt.Sprintf("%s–%s", st.Format("15:04"), en.Format("15:04"))
			switch {
			case s.Travel:
				u.P("    %s  %s", u.Dim(tm), u.Dim("🚶 "+strings.TrimPrefix(s.Label, "Walk: ")))
			case s.Session != nil:
				tag := ""
				if !s.Managed {
					tag = u.Dim(" (not in reinvent.yaml)")
				}
				u.P("    %s  %s %s %s%s", tm, cut(s.Session.Code, 11), cut(s.Session.Title, 46), u.Dim(s.Session.Venue), tag)
			default:
				u.P("    %s  %s", tm, u.Yellow("■ "+s.Label))
			}
		}
	}
	u.P("")
}

func label(a plan.Action) string {
	if a.Session != nil {
		return a.Session.Code
	}
	return a.SessionID
}

// Progress renders one apply event.
func (u *UI) Progress(e apply.Event) {
	sym := map[apply.EventKind]string{
		apply.EvOK: u.Green("✓"), apply.EvFail: u.Red("✗"), apply.EvSkip: u.Yellow("↷"),
		apply.EvInfo: u.Cyan("i"), apply.EvWarning: u.Yellow("!"),
	}[e.Kind]
	u.P("  %s %-13s %-12s %s", sym, e.Action, e.Subject, u.Dim(e.Detail))
}

// Sessions prints search hits.
func (u *UI) Sessions(hits []catalog.Hit, limit int) {
	for i, h := range hits {
		if limit > 0 && i >= limit {
			u.P(u.Dim(fmt.Sprintf("  … %d more", len(hits)-limit)))
			break
		}
		s := h.Session
		live := ""
		if !s.Recorded {
			live = u.Cyan("◆")
		}
		seats := s.Seats()
		if seats != "" {
			seats = "[" + seats + "]"
		}
		u.P("  %s %s %s  %s  %s %s %s", cut(s.Code, 11), cut(s.Title, 50), cut(u.when(s.Start, s.End), 16), cut(s.Venue, 14), cut(s.Kind, 16), live, u.Dim(seats))
	}
}

func (u *UI) Session(s *catalog.Session, repeats []*catalog.Session) {
	u.P("%s  %s", u.Bold(s.Code), u.Bold(s.Title))
	u.P("  %s · %s · %s · %s", u.when(s.Start, s.End), orDash(s.Venue), orDash(s.Room), orDash(s.Level))
	rec := "likely recorded"
	if !s.Recorded {
		rec = "not recorded: see it live"
	}
	u.P("  %s · %s · seats: %s", s.Kind, rec, orDash(s.Seats()))
	for _, p := range s.People {
		u.P("  %s %s", u.Dim("speaker"), strings.Trim(strings.Join([]string{p.Name, p.Title, p.Company}, " · "), " ·"))
	}
	if len(s.Topics)+len(s.Services) > 0 {
		u.P("  %s %s", u.Dim("tags"), strings.Join(append(append([]string{}, s.Topics...), s.Services...), ", "))
	}
	if s.Abstract != "" {
		u.P("")
		u.P("  %s", wrap(s.Abstract, 88, "  "))
	}
	if len(repeats) > 0 {
		u.P("")
		u.P("  %s", u.Dim("Also runs:"))
		for _, r := range repeats {
			u.P("    %s %s %s", cut(r.Code, 11), cut(u.when(r.Start, r.End), 16), r.Venue)
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

func wrap(s string, w int, indent string) string {
	var out strings.Builder
	n := 0
	for _, word := range strings.Fields(s) {
		if n+len(word) > w && n > 0 {
			out.WriteString("\n" + indent)
			n = 0
		}
		if n > 0 {
			out.WriteString(" ")
			n++
		}
		out.WriteString(word)
		n += len(word)
	}
	return out.String()
}

// Overview summarizes a catalog instead of dumping it.
func (u *UI) Overview(cat *catalog.Catalog, f catalog.Facets) {
	u.P("%s %s", u.Bold(cat.Event.Name), u.Dim(fmt.Sprintf("· %d sessions", len(cat.Sessions))))
	u.P("")
	row := func(title string, fs []catalog.Facet, max int) {
		var parts []string
		for i, x := range fs {
			if max > 0 && i >= max {
				parts = append(parts, u.Dim(fmt.Sprintf("+%d more", len(fs)-max)))
				break
			}
			parts = append(parts, fmt.Sprintf("%s %s", x.Value, u.Dim(fmt.Sprint(x.Count))))
		}
		u.P("  %s  %s", u.Cyan(cut(title, 7)), strings.Join(parts, u.Dim("  ·  ")))
	}
	row("days", f.Days, 0)
	row("venues", f.Venues, 0)
	row("kinds", f.Kinds, 8)
	row("levels", f.Levels, 0)
	row("topics", f.Topics, 8)
	u.P("")
	u.P(u.Bold("Try"))
	for _, ex := range []string{
		"replan search agents --day tue --live          # not recorded: chalk talks, workshops, code talks",
		"replan search --topic serverless --kind workshop,builders-session",
		"replan search --venue \"caesars forum\" --day wed --level 300,400",
		"replan search bedrock -n 0                      # every match",
		"replan ui                                       # browse and build your plan in the browser",
	} {
		u.P("  %s", ex)
	}
}

// LiveNext renders `replan now`.
func (u *UI) LiveNext(r live.Report, mode string) {
	at := r.At
	if at == "" {
		at = "unknown (use --at)"
	}
	u.P("%s %s", u.Bold("replan now"), u.Dim(fmt.Sprintf("· %s %s · you're at %s · %s", r.Day, r.Now.In(u.Loc).Format("15:04"), at, mode)))
	u.P("")
	for _, lg := range r.Legs {
		s := lg.Session
		tm := fmt.Sprintf("%s–%s", s.Start.In(u.Loc).Format("15:04"), s.End.In(u.Loc).Format("15:04"))
		var st string
		switch lg.Status {
		case live.InProgress:
			st = u.Cyan("● now")
		case live.Missed:
			st = u.Dim("missed: under way elsewhere")
		case live.OnTrack:
			st = u.Green(fmt.Sprintf("✓ leave by %s", lg.LeaveBy.In(u.Loc).Format("15:04")))
		case live.Tight:
			st = u.Yellow(fmt.Sprintf("! leave by %s", lg.LeaveBy.In(u.Loc).Format("15:04")))
		case live.SeatRisk:
			st = u.Yellow("! seat released before you arrive")
		case live.WontMake:
			st = u.Red("✗ you won't make it")
		}
		u.P("  %s  %s %s %s  %s", tm, cut(s.Code, 11), cut(s.Title, 42), cut(s.Venue, 14), st)
		if lg.Status != live.InProgress && lg.Status != live.Missed && lg.From != "" && lg.From != s.Venue {
			u.P("               %s", u.Dim(fmt.Sprintf("%s walk from %s, arrive %s", fmtMin(lg.Travel), lg.From, lg.ArriveAt.In(u.Loc).Format("15:04"))))
		}
		for _, a := range lg.Alternatives {
			u.P("               %s %s %s %s %s", u.Cyan("↳ instead:"), cut(a.Code, 11), cut(a.Title, 38), a.Start.In(u.Loc).Format("15:04"), u.Dim(a.Venue))
		}
	}
	if len(r.Advice) > 0 {
		u.P("")
		for _, a := range r.Advice {
			u.P("  %s %s", u.Yellow("→"), a)
		}
	}
}

func fmtMin(d time.Duration) string { return fmt.Sprintf("%dm", int(d.Minutes())) }

// Tonight renders `replan tonight`.
func (u *UI) Tonight(day string, issues []live.Issue) {
	u.P("%s %s", u.Bold("replan tonight"), u.Dim("· reservations for "+day+" you can't realistically keep"))
	u.P("")
	if len(issues) == 0 {
		u.P("  %s Every reservation on %s is reachable. Nothing to give back.", u.Green("✓"), day)
		return
	}
	var codes []string
	for _, is := range issues {
		u.P("  %s %s %s", u.Yellow("!"), u.Bold(cut(is.Code, 11)), is.Problem)
		codes = append(codes, is.Code)
	}
	u.P("")
	u.P("  Reserved seats you won't use leave the walk-up line waiting. Give these back tonight:")
	u.P("    replan skip %s && replan plan", strings.Join(codes, " "))
}

// Changes renders `replan changes`.
func (u *UI) Changes(imps []live.Impact, unrelated, total int) {
	u.P("%s %s", u.Bold("replan changes"), u.Dim(fmt.Sprintf("· %d catalog changes, %d matter to you", total, len(imps))))
	u.P("")
	if total == 0 {
		u.P("  No changes recorded yet. replan records them each time it refreshes the catalog.")
		return
	}
	last, shown, hidden := "", 0, 0
	for _, im := range imps {
		if im.Relevance != last {
			u.P("  %s", u.Cyan(strings.ToUpper(im.Relevance)))
			last, shown = im.Relevance, 0
		}
		if (im.Relevance == live.YourInterests && shown >= 8) || (im.Relevance == live.NewLaunch && shown >= 5) {
			hidden++
			continue
		}
		shown++
		u.P("    %s", im.Summary())
		u.P("      %s", u.Dim(im.Effect))
	}
	if hidden > 0 {
		u.P("    %s", u.Dim(fmt.Sprintf("+%d more new sessions (replan search to browse them)", hidden)))
	}
	if unrelated > 0 {
		u.P("")
		noun := "changes don't"
		if unrelated == 1 {
			noun = "change doesn't"
		}
		u.P("  %s", u.Dim(fmt.Sprintf("%d other %s touch your plan or interests.", unrelated, noun)))
	}
}

func allSkipped(pk plan.Pick) bool {
	if pk.Chosen != nil || len(pk.Skipped) == 0 {
		return false
	}
	for _, s := range pk.Skipped {
		if s.Reason != "you're skipping it" {
			return false
		}
	}
	return true
}
