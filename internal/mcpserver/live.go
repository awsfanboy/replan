package mcpserver

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/awsfanboy/replan/internal/app"
	"github.com/awsfanboy/replan/internal/catalog"
	"github.com/awsfanboy/replan/internal/live"
	"github.com/awsfanboy/replan/internal/plan"
	"github.com/awsfanboy/replan/internal/spec"
	"github.com/awsfanboy/replan/internal/ui"
)

func (s *Server) addLiveTools(srv *mcp.Server) {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(true)}
	mcp.AddTool(srv, &mcp.Tool{Name: "whats_next", Title: "What's next (during the event)",
		Description: "During the event: the user's remaining sessions today, when to leave for each, and whether they will make it from where they are (optionally running late). For sessions they won't make, lists alternatives they can still reach and advises giving the seat back.",
		Annotations: ro}, s.whatsNext)
	mcp.AddTool(srv, &mcp.Tool{Name: "tonight_check", Title: "Seats to give back",
		Description: "Check a day's reservations (default: the next event day) for seats the user can't realistically use: overlaps, walks that can't be made in time, a repeat held twice, clashes with their own meetings. Suggests which to give back; to act, add them to skip with edit_plan_file, then plan and apply.",
		Annotations: ro}, s.tonightCheck)
	mcp.AddTool(srv, &mcp.Tool{Name: "what_changed", Title: "What changed for me",
		Description: "Catalog changes that matter to the user (their reservations, plan, favorites, new-launch sessions and sessions matching their interests), with what each means for their schedule. The Events API has no change feed; replan records one each time it refreshes the catalog.",
		Annotations: ro}, s.whatChanged)
}

type liveIn struct {
	At          string `json:"at,omitempty" jsonschema:"where the user is now, e.g. Wynn, MGM Grand, Caesars Forum; default: inferred from their schedule"`
	LateMinutes int    `json:"late_minutes,omitempty" jsonschema:"minutes before the user can leave (running late)"`
	Now         string `json:"now,omitempty" jsonschema:"what-if time in event time, e.g. 'tue 10:40'; default: now"`
}

func (s *Server) liveInput(ctx context.Context, at string, late int, now string) (live.Input, *catalog.Catalog, error) {
	sp, err := spec.Load(s.App.Cfg.SpecPath)
	ev := ""
	if err == nil {
		ev = sp.Event
	}
	if ev, err = s.App.ResolveEvent(ev); err != nil {
		return live.Input{}, nil, err
	}
	cat, err := s.App.Catalog(ctx, ev)
	if err != nil {
		return live.Input{}, nil, err
	}
	svc, clock, err := s.App.Backend(cat)
	if err != nil {
		return live.Input{}, nil, err
	}
	if now != "" {
		if clock, err = app.ParseNow(now, cat); err != nil {
			return live.Input{}, nil, err
		}
	}
	sched, err := svc.GetSchedule(ctx, ev)
	if err != nil {
		return live.Input{}, nil, err
	}
	in := live.Input{Catalog: cat, Schedule: sched, Now: clock, At: at, Late: time.Duration(late) * time.Minute, IgnoreSeatBands: s.App.IgnoreSeatBands()}
	if sp != nil {
		in.Spec, in.Matrix = sp, plan.MatrixFor(sp.Rules)
	}
	return in, cat, nil
}

type LegOut struct {
	Session       SessionOut   `json:"session"`
	Status        string       `json:"status" jsonschema:"in-progress, missed (under way while the user is elsewhere), on-track, tight, seat-at-risk (arrives after reserved seats are released) or wont-make-it"`
	From          string       `json:"from,omitempty"`
	TravelMinutes int          `json:"travel_minutes,omitempty"`
	LeaveBy       string       `json:"leave_by,omitempty"`
	ArriveAt      string       `json:"arrive_at,omitempty"`
	Alternatives  []SessionOut `json:"alternatives"`
}
type WhatsNextOut struct {
	Now    string   `json:"now"`
	At     string   `json:"at"`
	Legs   []LegOut `json:"legs"`
	Advice []string `json:"advice"`
}

func render(cat *catalog.Catalog, f func(*ui.UI)) string {
	var b bytes.Buffer
	u := ui.New(&b)
	u.Loc = cat.Location
	f(u)
	return b.String()
}

func (s *Server) whatsNext(ctx context.Context, _ *mcp.CallToolRequest, in liveIn) (*mcp.CallToolResult, WhatsNextOut, error) {
	li, cat, err := s.liveInput(ctx, in.At, in.LateMinutes, in.Now)
	if err != nil {
		return errResult("%v", err), WhatsNextOut{}, nil
	}
	r := live.Next(li)
	loc := cat.Location
	out := WhatsNextOut{Now: r.Now.In(loc).Format("Mon 2006-01-02 15:04"), At: r.At, Legs: []LegOut{}, Advice: r.Advice}
	for _, lg := range r.Legs {
		lo := LegOut{Session: sessionOut(lg.Session, loc), Status: lg.Status, From: lg.From, TravelMinutes: int(lg.Travel.Minutes()), Alternatives: []SessionOut{}}
		if lg.Status != live.InProgress && lg.Status != live.Missed {
			lo.LeaveBy, lo.ArriveAt = lg.LeaveBy.In(loc).Format("15:04"), lg.ArriveAt.In(loc).Format("15:04")
		}
		for _, a := range lg.Alternatives {
			lo.Alternatives = append(lo.Alternatives, sessionOut(a, loc))
		}
		out.Legs = append(out.Legs, lo)
	}
	if out.Advice == nil {
		out.Advice = []string{}
	}
	text := render(cat, func(u *ui.UI) { u.LiveNext(r, s.App.Mode()) })
	return textResult(text + "\nTo give a seat back: edit_plan_file with skip, then plan, then apply after the user approves."), out, nil
}

type TonightIn struct {
	Day string `json:"day,omitempty" jsonschema:"YYYY-MM-DD; default: the next event day with reservations"`
	Now string `json:"now,omitempty" jsonschema:"what-if time in event time, e.g. 'mon 20:00'"`
}
type TonightOut struct {
	Day    string       `json:"day"`
	Issues []live.Issue `json:"issues"`
	Next   string       `json:"next"`
}

func (s *Server) tonightCheck(ctx context.Context, _ *mcp.CallToolRequest, in TonightIn) (*mcp.CallToolResult, TonightOut, error) {
	li, cat, err := s.liveInput(ctx, "", 0, in.Now)
	if err != nil {
		return errResult("%v", err), TonightOut{}, nil
	}
	day := in.Day
	if day == "" {
		day = live.NextEventDay(li)
	}
	issues := live.Tonight(li, day)
	out := TonightOut{Day: day, Issues: issues, Next: "Nothing to give back."}
	if out.Issues == nil {
		out.Issues = []live.Issue{}
	}
	if len(issues) > 0 {
		var codes []string
		for _, is := range issues {
			codes = append(codes, is.Code)
		}
		out.Next = fmt.Sprintf("Ask the user whether to give back %s. If yes: edit_plan_file {skip: %q}, then plan, then apply.", strings.Join(codes, ", "), codes)
	}
	return textResult(render(cat, func(u *ui.UI) { u.Tonight(day, issues) })), out, nil
}

type ChangedIn struct {
	SinceHours int  `json:"since_hours,omitempty" jsonschema:"how far back to look (default 72)"`
	Refresh    bool `json:"refresh,omitempty" jsonschema:"download the catalog now to pick up the latest changes"`
}
type ChangedOut struct {
	Total     int           `json:"total_changes"`
	Unrelated int           `json:"unrelated"`
	Impacts   []live.Impact `json:"impacts"`
}

func (s *Server) whatChanged(ctx context.Context, _ *mcp.CallToolRequest, in ChangedIn) (*mcp.CallToolResult, ChangedOut, error) {
	li, cat, err := s.liveInput(ctx, "", 0, "")
	if err != nil {
		return errResult("%v", err), ChangedOut{}, nil
	}
	var changes []catalog.Change
	if s.App.Cfg.CompareFile != "" {
		old, err := catalog.LoadFile(s.App.Cfg.CompareFile)
		if err != nil {
			return errResult("%v", err), ChangedOut{}, nil
		}
		changes = catalog.Diff(old, cat, time.Now())
	} else {
		if in.Refresh {
			if cat, err = s.App.Refresh(ctx, cat.Event.EventID); err != nil {
				return errResult("refreshing catalog: %v", err), ChangedOut{}, nil
			}
			li.Catalog = cat
		}
		h := in.SinceHours
		if h <= 0 {
			h = 72
		}
		if changes, err = catalog.History("", cat.Event.EventID, time.Now().Add(-time.Duration(h)*time.Hour)); err != nil {
			return errResult("%v", err), ChangedOut{}, nil
		}
	}
	imps, unrelated := live.Impacts(li, changes)
	out := ChangedOut{Total: len(changes), Unrelated: unrelated, Impacts: imps}
	if out.Impacts == nil {
		out.Impacts = []live.Impact{}
	}
	return textResult(render(cat, func(u *ui.UI) { u.Changes(imps, unrelated, len(changes)) })), out, nil
}
