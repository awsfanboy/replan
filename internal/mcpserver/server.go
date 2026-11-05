// Package mcpserver exposes replan to AI agents over the Model Context
// Protocol.
//
// The design choice that matters: the agent never gets a raw "reserve" tool.
// It edits the desired state (reinvent.yaml), asks for a plan, and can only
// apply a plan the human approved. The approval is either a confirmation
// dialog the client shows to the human (MCP elicitation, which the model
// cannot answer), or, for clients without dialogs, an explicit confirmation
// plus the plan's fingerprint: if anything changed since the plan was shown,
// apply refuses. Every apply is verified against GetSchedule, so the agent
// cannot report "done" for a reservation that failed.
package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/awsfanboy/replan/internal/api"
	"github.com/awsfanboy/replan/internal/app"
	"github.com/awsfanboy/replan/internal/apply"
	"github.com/awsfanboy/replan/internal/catalog"
	"github.com/awsfanboy/replan/internal/plan"
	"github.com/awsfanboy/replan/internal/spec"
	"github.com/awsfanboy/replan/internal/ui"
)

const Version = "0.2.0"

const instructions = `replan manages the user's AWS re:Invent schedule as code.

The desired schedule lives in a plan file (reinvent.yaml): a RANKED list of sessions, each with
optional fallbacks, plus rules (max sessions per day, arrive-early buffer, walking blocks) and the
user's own blocks (meals, meetings). Each ranked entry reserves ONE seat: the first candidate that
is not full and leaves time to walk to and from its neighbours.

Workflow:
1. Understand the user's goals. Use search_sessions / get_session to find candidates. Prefer
   sessions that are not recorded (chalk talks, workshops, builders' sessions, code talks) when
   the user wants in-person value; recorded breakouts can be watched later.
2. Use edit_plan_file to rank sessions and give every entry a fallback (a repeat, or a
   same_slot_alternative from get_session), set rules and add blocks.
3. Call plan and show the user the result: what will be reserved, what was skipped and why, and
   the walking blocks that will be written into their official event app.
4. Only when the user explicitly approves, call apply with the plan_id. Never call apply on your
   own initiative. If the client shows a confirmation dialog, the user answers it there.
5. Report what apply returns. It is verified against the live schedule; do not claim more.

During the event (replan Live):
- "I'm late / stuck at X / what's next?" → whats_next (with at and late_minutes). For a session the
  user won't make, offer to give the seat back (edit_plan_file skip) and suggest a reachable
  alternative from the result.
- In the evening → tonight_check: seats for tomorrow the user can't realistically use. Giving them
  back lets someone in the walk-up line have them.
- "Anything changed?" → what_changed: moves, cancellations and new-launch sessions that matter to
  this user, and what each means for their day.
Giving seats back is good: suggest it whenever a reservation can't be used.

Nothing touches the real schedule except apply. Reserved seats are shared: never try to hoard
seats or loop on apply to grab seats.`

type Server struct {
	App *app.App
	mu  sync.Mutex // serializes plan-file edits and applies
}

func New(a *app.App) *mcp.Server {
	s := &Server{App: a}
	srv := mcp.NewServer(&mcp.Implementation{Name: "replan", Title: "replan: re:Invent schedule as code", Version: Version},
		&mcp.ServerOptions{Instructions: instructions, Capabilities: &mcp.ServerCapabilities{}})

	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(true)}

	mcp.AddTool(srv, &mcp.Tool{Name: "list_events", Title: "List AWS events",
		Description: "List AWS events (re:Invent and others) with their IDs and whether their catalog needs registration.",
		Annotations: ro}, s.listEvents)
	mcp.AddTool(srv, &mcp.Tool{Name: "search_sessions", Title: "Search sessions",
		Description: "Search an event's session catalog by free text and filters. Returns codes, times, venues, kind, and whether the session is likely recorded.",
		Annotations: ro}, s.searchSessions)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_session", Title: "Session details",
		Description: "Full details of one session (abstract, speakers, tags, seats), its repeats, and similar sessions in the same time slot. Repeats and same-slot alternatives are the fallbacks to put in the plan.",
		Annotations: ro}, s.getSession)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_plan_file", Title: "Read the plan file",
		Description: "Read the user's desired schedule (reinvent.yaml): ranked sessions with fallbacks, favorites, blocks and rules.",
		Annotations: ro}, s.getPlanFile)
	mcp.AddTool(srv, &mcp.Tool{Name: "edit_plan_file", Title: "Edit the plan file",
		Description: "Change the desired schedule: add or re-rank sessions, set fallbacks, remove sessions, manage favorites and personal blocks, change rules. Only edits the file; nothing on the real schedule changes. Creates the file if needed (then 'event' is required).",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)}}, s.editPlanFile)
	mcp.AddTool(srv, &mcp.Tool{Name: "plan", Title: "Plan changes",
		Description: "Compare the plan file with the user's real schedule and explain what apply would do: seats to reserve (with fallbacks chosen and skipped candidates explained), cancellations, favorites, and walking blocks for the official app. Read-only. Returns a plan_id to pass to apply after the user approves.",
		Annotations: ro}, s.plan)
	mcp.AddTool(srv, &mcp.Tool{Name: "apply", Title: "Apply the approved plan",
		Description: "Make the real schedule match the approved plan: reserve in rank order with automatic fallback, write walking blocks, then verify against the live schedule. Requires the plan_id from plan and the user's explicit approval; refuses if anything changed since the plan was shown. Call only after the user said yes.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: false, OpenWorldHint: ptr(true)}}, s.apply)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_agenda", Title: "Current schedule",
		Description: "The user's current real schedule, day by day: reserved sessions, walking blocks and personal time, plus favorites.",
		Annotations: ro}, s.getAgenda)

	s.addLiveTools(srv)

	srv.AddPrompt(&mcp.Prompt{Name: "im_running_late", Title: "I'm running late",
		Description: "During the event: check what the user can still make, give back seats they can't use, and suggest reachable alternatives.",
		Arguments: []*mcp.PromptArgument{
			{Name: "where", Description: "Where the user is now, e.g. MGM Grand"},
			{Name: "minutes", Description: "How late they are, in minutes"},
		}}, s.runningLate)

	srv.AddPrompt(&mcp.Prompt{Name: "plan_my_week", Title: "Plan my re:Invent week",
		Description: "Interview the user, build a ranked plan with fallbacks, show it, and apply it only after approval.",
		Arguments: []*mcp.PromptArgument{
			{Name: "goals", Description: "What the user wants to learn or do", Required: true},
			{Name: "event", Description: "Event ID, e.g. reinvent2026"},
			{Name: "constraints", Description: "Days, venues, meetings, pace (e.g. max 3 a day)"},
		}}, s.planMyWeek)
	return srv
}

func ptr[T any](v T) *T { return &v }

// ---- shared shapes ----

type SessionOut struct {
	Code     string   `json:"code"`
	Title    string   `json:"title"`
	Kind     string   `json:"kind"`
	Level    string   `json:"level,omitempty"`
	Day      string   `json:"day,omitempty" jsonschema:"weekday and date in event time"`
	Start    string   `json:"start,omitempty" jsonschema:"HH:MM in event time"`
	End      string   `json:"end,omitempty"`
	Venue    string   `json:"venue,omitempty"`
	Room     string   `json:"room,omitempty"`
	Recorded bool     `json:"likely_recorded" jsonschema:"breakouts and keynotes are usually published later; chalk talks, workshops, builders' sessions and code talks are not"`
	Seats    string   `json:"seats,omitempty" jsonschema:"available, limited, veryLimited, unavailable or walkUp; bands can be stale"`
	Topics   []string `json:"topics,omitempty"`
	Speakers []string `json:"speakers,omitempty"`
}

func sessionOut(s *catalog.Session, loc *time.Location) SessionOut {
	o := SessionOut{Code: s.Code, Title: s.Title, Kind: s.Kind, Level: s.Level, Venue: s.Venue, Room: s.Room,
		Recorded: s.Recorded, Seats: s.Seats()}
	if s.HasTime() {
		st, en := s.Start.In(loc), s.End.In(loc)
		o.Day, o.Start, o.End = st.Format("Mon 2006-01-02"), st.Format("15:04"), en.Format("15:04")
	}
	o.Topics = append(append([]string{}, s.Topics...), s.Services...)
	if len(o.Topics) > 8 {
		o.Topics = o.Topics[:8]
	}
	for _, p := range s.People {
		o.Speakers = append(o.Speakers, strings.Trim(strings.Join([]string{p.Name, p.Company}, ", "), ", "))
	}
	return o
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

func errResult(format string, a ...any) *mcp.CallToolResult {
	r := textResult(fmt.Sprintf(format, a...))
	r.IsError = true
	return r
}

// ---- list_events ----

type ListEventsIn struct {
	IncludePast bool `json:"include_past,omitempty"`
}
type EventOut struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	Start                string `json:"start"`
	City                 string `json:"city,omitempty"`
	RegistrationRequired bool   `json:"registration_required"`
}
type ListEventsOut struct {
	Events []EventOut `json:"events"`
}

func (s *Server) listEvents(ctx context.Context, _ *mcp.CallToolRequest, in ListEventsIn) (*mcp.CallToolResult, ListEventsOut, error) {
	evs, err := s.App.Client().ListEvents(ctx, in.IncludePast)
	if err != nil {
		return errResult("listing events: %v", err), ListEventsOut{}, nil
	}
	out := ListEventsOut{Events: []EventOut{}}
	for _, e := range evs {
		city := ""
		if e.Address != nil {
			city = e.Address.City
		}
		out.Events = append(out.Events, EventOut{ID: e.EventID, Name: e.Name, Start: e.StartDate[:min(10, len(e.StartDate))], City: city, RegistrationRequired: e.AuthenticationRequired})
	}
	return nil, out, nil
}

// ---- search_sessions ----

type SearchIn struct {
	Query    string   `json:"query,omitempty" jsonschema:"free text: topics, AWS services, title words, session codes or speaker names"`
	Event    string   `json:"event,omitempty" jsonschema:"event ID; defaults to the plan file's event"`
	Day      string   `json:"day,omitempty" jsonschema:"YYYY-MM-DD or weekday (mon, tuesday, ...)"`
	Kinds    []string `json:"kinds,omitempty" jsonschema:"breakout, chalk-talk, workshop, builders-session, code-talk, lightning-talk, keynote, innovation-talk, dev-chat, gameday, jam"`
	Venue    string   `json:"venue,omitempty" jsonschema:"venue name contains, e.g. Wynn, Venetian, Caesars Forum"`
	Levels   []string `json:"levels,omitempty" jsonschema:"100, 200, 300, 400, 500"`
	LiveOnly bool     `json:"live_only,omitempty" jsonschema:"only sessions unlikely to be recorded"`
	Limit    int      `json:"limit,omitempty" jsonschema:"max results (default 20, max 50)"`
}
type SearchOut struct {
	Event    string       `json:"event"`
	Total    int          `json:"total"`
	Sessions []SessionOut `json:"sessions"`
}

func (s *Server) searchSessions(ctx context.Context, _ *mcp.CallToolRequest, in SearchIn) (*mcp.CallToolResult, SearchOut, error) {
	ev, err := s.App.ResolveEvent(in.Event)
	if err != nil {
		return errResult("%v", err), SearchOut{}, nil
	}
	cat, err := s.App.Catalog(ctx, ev)
	if err != nil {
		return errResult("loading catalog: %v", err), SearchOut{}, nil
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 50)
	hits := cat.Search(in.Query, catalog.Filter{Day: in.Day, Kinds: in.Kinds, Venue: in.Venue, Levels: in.Levels, OnlyLive: in.LiveOnly})
	out := SearchOut{Event: ev, Total: len(hits), Sessions: []SessionOut{}}
	for i, h := range hits {
		if i >= limit {
			break
		}
		out.Sessions = append(out.Sessions, sessionOut(h.Session, cat.Location))
	}
	return nil, out, nil
}

// ---- get_session ----

type GetSessionIn struct {
	Code  string `json:"code" jsonschema:"session code, e.g. AIM301"`
	Event string `json:"event,omitempty"`
}
type GetSessionOut struct {
	Session      SessionOut   `json:"session"`
	Abstract     string       `json:"abstract,omitempty"`
	Repeats      []SessionOut `json:"repeats" jsonschema:"other runs of the same session: the best fallbacks"`
	Alternatives []SessionOut `json:"same_slot_alternatives" jsonschema:"similar sessions at about the same time: good fallbacks when there is no repeat"`
}

func (s *Server) getSession(ctx context.Context, _ *mcp.CallToolRequest, in GetSessionIn) (*mcp.CallToolResult, GetSessionOut, error) {
	ev, err := s.App.ResolveEvent(in.Event)
	if err != nil {
		return errResult("%v", err), GetSessionOut{}, nil
	}
	cat, err := s.App.Catalog(ctx, ev)
	if err != nil {
		return errResult("loading catalog: %v", err), GetSessionOut{}, nil
	}
	ss, ok := cat.Lookup(in.Code)
	if !ok {
		return errResult("%s is not in the %s catalog; use search_sessions", in.Code, ev), GetSessionOut{}, nil
	}
	out := GetSessionOut{Session: sessionOut(ss, cat.Location), Abstract: ss.Abstract, Repeats: []SessionOut{}, Alternatives: []SessionOut{}}
	for _, r := range cat.Repeats(ss) {
		out.Repeats = append(out.Repeats, sessionOut(r, cat.Location))
	}
	for _, r := range cat.Alternatives(ss, 5) {
		out.Alternatives = append(out.Alternatives, sessionOut(r, cat.Location))
	}
	return nil, out, nil
}

// ---- plan file ----

type WantOut struct {
	Rank      int          `json:"rank"`
	Reserve   SessionOut   `json:"reserve"`
	Fallbacks []SessionOut `json:"fallbacks"`
	Why       string       `json:"why,omitempty"`
}
type BlockOut struct {
	Title    string `json:"title"`
	Day      string `json:"day"`
	From     string `json:"from"`
	To       string `json:"to"`
	Location string `json:"location,omitempty"`
}
type RulesOut struct {
	MaxPerDay   int    `json:"max_per_day"`
	ArriveEarly string `json:"arrive_early"`
	Travel      bool   `json:"travel_blocks"`
	Prune       bool   `json:"prune"`
}
type PlanFileOut struct {
	Skip      []string     `json:"skip"`
	Path      string       `json:"path"`
	Exists    bool         `json:"exists"`
	Event     string       `json:"event,omitempty"`
	Rules     RulesOut     `json:"rules"`
	Sessions  []WantOut    `json:"sessions"`
	Favorites []SessionOut `json:"favorites"`
	Blocks    []BlockOut   `json:"blocks"`
}

func refOut(cat *catalog.Catalog, ref string) SessionOut {
	if s, ok := cat.Lookup(ref); ok {
		return sessionOut(s, cat.Location)
	}
	return SessionOut{Code: ref, Title: "(not in catalog)"}
}

func (s *Server) fileOut(ctx context.Context, sp *spec.Spec) (PlanFileOut, error) {
	out := PlanFileOut{Path: s.App.Cfg.SpecPath, Exists: true, Event: sp.Event, Sessions: []WantOut{}, Favorites: []SessionOut{}, Blocks: []BlockOut{}, Skip: append([]string{}, sp.Skip...),
		Rules: RulesOut{MaxPerDay: sp.Rules.MaxPerDay, ArriveEarly: sp.Rules.ArriveEarly, Travel: sp.Rules.TravelEnabled(), Prune: sp.Rules.Prune}}
	cat, err := s.App.Catalog(ctx, sp.Event)
	if err != nil {
		return out, err
	}
	for i, w := range sp.Sessions {
		wo := WantOut{Rank: i + 1, Reserve: refOut(cat, w.Reserve), Why: w.Why, Fallbacks: []SessionOut{}}
		for _, f := range w.Fallbacks {
			wo.Fallbacks = append(wo.Fallbacks, refOut(cat, f))
		}
		out.Sessions = append(out.Sessions, wo)
	}
	for _, f := range sp.Favorites {
		out.Favorites = append(out.Favorites, refOut(cat, f))
	}
	for _, b := range sp.Blocks {
		out.Blocks = append(out.Blocks, BlockOut{b.Title, b.Day, b.From, b.To, b.Location})
	}
	return out, nil
}

func (s *Server) getPlanFile(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, PlanFileOut, error) {
	sp, err := spec.Load(s.App.Cfg.SpecPath)
	if errors.Is(err, os.ErrNotExist) {
		return textResult(fmt.Sprintf("No plan file yet at %s. Create one with edit_plan_file (pass event, e.g. reinvent2026).", s.App.Cfg.SpecPath)),
			PlanFileOut{Path: s.App.Cfg.SpecPath, Sessions: []WantOut{}, Favorites: []SessionOut{}, Blocks: []BlockOut{}}, nil
	}
	if err != nil {
		return errResult("reading plan file: %v", err), PlanFileOut{}, nil
	}
	out, err := s.fileOut(ctx, sp)
	if err != nil {
		return errResult("%v", err), PlanFileOut{}, nil
	}
	return nil, out, nil
}

type WantIn struct {
	Reserve   string   `json:"reserve" jsonschema:"session code to reserve"`
	Fallbacks []string `json:"fallbacks,omitempty" jsonschema:"codes to try, in order, if the first is full or doesn't fit; repeats of the same session are ideal"`
	Why       string   `json:"why,omitempty" jsonschema:"short note on why the user wants it"`
	Rank      int      `json:"rank,omitempty" jsonschema:"1-based position in the ranking; default: last. If the session is already listed, it is updated and moved."`
}
type MoveIn struct {
	Code string `json:"code"`
	Rank int    `json:"rank" jsonschema:"new 1-based position"`
}
type BlockIn struct {
	Title    string `json:"title"`
	Day      string `json:"day" jsonschema:"YYYY-MM-DD"`
	From     string `json:"from" jsonschema:"HH:MM, event-local time"`
	To       string `json:"to" jsonschema:"HH:MM, event-local time"`
	Location string `json:"location,omitempty"`
}
type RulesIn struct {
	MaxPerDay   *int    `json:"max_per_day,omitempty" jsonschema:"sessions per day; 0 = no cap"`
	ArriveEarly *string `json:"arrive_early,omitempty" jsonschema:"extra buffer before each session, e.g. 10m"`
	Travel      *bool   `json:"travel_blocks,omitempty" jsonschema:"write a walking block into the official schedule for every venue change"`
	Prune       *bool   `json:"prune,omitempty" jsonschema:"cancel reservations and favorites that are not in the plan file"`
}
type EditIn struct {
	Event           string    `json:"event,omitempty" jsonschema:"event ID; required when creating the plan file"`
	Add             []WantIn  `json:"add,omitempty" jsonschema:"sessions to add (or update) in the ranking"`
	Remove          []string  `json:"remove,omitempty" jsonschema:"session codes to remove (a fallback code removes just that fallback)"`
	Move            []MoveIn  `json:"move,omitempty"`
	AddFavorites    []string  `json:"add_favorites,omitempty"`
	RemoveFavorites []string  `json:"remove_favorites,omitempty"`
	AddBlocks       []BlockIn `json:"add_blocks,omitempty" jsonschema:"personal time: meals, meetings, travel days"`
	RemoveBlocks    []string  `json:"remove_blocks,omitempty" jsonschema:"titles of blocks to remove"`
	Skip            []string  `json:"skip,omitempty" jsonschema:"sessions the user will not attend: a held seat is cancelled by the next apply (given back to the walk-up line) and never re-booked"`
	Unskip          []string  `json:"unskip,omitempty"`
	Rules           *RulesIn  `json:"rules,omitempty"`
}
type EditOut struct {
	Changes []string    `json:"changes"`
	File    PlanFileOut `json:"file"`
	Next    string      `json:"next"`
}

func (s *Server) editPlanFile(ctx context.Context, _ *mcp.CallToolRequest, in EditIn) (*mcp.CallToolResult, EditOut, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.App.Cfg.SpecPath
	sp, err := spec.Load(path)
	var changes []string
	switch {
	case errors.Is(err, os.ErrNotExist):
		ev := in.Event
		if ev == "" {
			ev = s.App.Cfg.Event
		}
		if ev == "" {
			return errResult("no plan file yet: pass event (e.g. reinvent2026) to create %s", path), EditOut{}, nil
		}
		sp = &spec.Spec{Event: ev, Rules: spec.Rules{MaxPerDay: 4, ArriveEarly: "10m"}}
		changes = append(changes, "created "+path+" for "+ev)
	case err != nil:
		return errResult("reading plan file: %v", err), EditOut{}, nil
	case in.Event != "" && in.Event != sp.Event:
		return errResult("the plan file is for %s, not %s; edit it by hand to switch events", sp.Event, in.Event), EditOut{}, nil
	}
	cat, err := s.App.Catalog(ctx, sp.Event)
	if err != nil {
		return errResult("loading catalog: %v", err), EditOut{}, nil
	}
	canon := func(ref string) (string, error) {
		if ss, ok := cat.Lookup(ref); ok {
			return ss.Code, nil
		}
		return "", fmt.Errorf("%s is not in the %s catalog (use search_sessions to find the code)", ref, sp.Event)
	}
	var problems []string
	find := func(code string) int {
		for i, w := range sp.Sessions {
			if strings.EqualFold(w.Reserve, code) {
				return i
			}
		}
		return -1
	}
	place := func(w spec.Want, rank int) {
		if rank <= 0 || rank > len(sp.Sessions) {
			sp.Sessions = append(sp.Sessions, w)
			return
		}
		sp.Sessions = append(sp.Sessions[:rank-1], append([]spec.Want{w}, sp.Sessions[rank-1:]...)...)
	}

	for _, r := range in.Remove {
		code, err := canon(r)
		if err != nil {
			code = r
		}
		if i := find(code); i >= 0 {
			sp.Sessions = append(sp.Sessions[:i], sp.Sessions[i+1:]...)
			changes = append(changes, "removed "+code)
			continue
		}
		removed := false
		for i := range sp.Sessions {
			fb := sp.Sessions[i].Fallbacks[:0:0]
			for _, f := range sp.Sessions[i].Fallbacks {
				if strings.EqualFold(f, code) {
					removed = true
					continue
				}
				fb = append(fb, f)
			}
			sp.Sessions[i].Fallbacks = fb
		}
		if removed {
			changes = append(changes, "removed fallback "+code)
		} else {
			problems = append(problems, code+" is not in the plan")
		}
	}
	for _, a := range in.Add {
		code, err := canon(a.Reserve)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		w := spec.Want{Reserve: code, Why: a.Why}
		bad := false
		for _, f := range a.Fallbacks {
			fc, err := canon(f)
			if err != nil {
				problems = append(problems, err.Error())
				bad = true
				continue
			}
			w.Fallbacks = append(w.Fallbacks, fc)
		}
		if bad {
			continue
		}
		if i := find(code); i >= 0 {
			old := sp.Sessions[i]
			if len(w.Fallbacks) == 0 {
				w.Fallbacks = old.Fallbacks
			}
			if w.Why == "" {
				w.Why = old.Why
			}
			sp.Sessions = append(sp.Sessions[:i], sp.Sessions[i+1:]...)
			rank := a.Rank
			if rank == 0 {
				rank = i + 1
			}
			place(w, rank)
			changes = append(changes, fmt.Sprintf("updated %s", code))
			continue
		}
		place(w, a.Rank)
		changes = append(changes, "added "+code)
	}
	for _, m := range in.Move {
		code, _ := canon(m.Code)
		i := find(code)
		if i < 0 {
			problems = append(problems, m.Code+" is not a ranked entry")
			continue
		}
		w := sp.Sessions[i]
		sp.Sessions = append(sp.Sessions[:i], sp.Sessions[i+1:]...)
		place(w, m.Rank)
		changes = append(changes, fmt.Sprintf("moved %s to #%d", code, m.Rank))
	}
	for _, f := range in.AddFavorites {
		code, err := canon(f)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if !containsFold(sp.Favorites, code) {
			sp.Favorites = append(sp.Favorites, code)
			changes = append(changes, "favorited "+code)
		}
	}
	for _, f := range in.RemoveFavorites {
		out := sp.Favorites[:0:0]
		for _, x := range sp.Favorites {
			if !strings.EqualFold(x, f) {
				out = append(out, x)
			}
		}
		if len(out) != len(sp.Favorites) {
			changes = append(changes, "unfavorited "+f)
		}
		sp.Favorites = out
	}
	for _, b := range in.AddBlocks {
		sp.Blocks = append(sp.Blocks, spec.Block{Title: b.Title, Day: b.Day, From: b.From, To: b.To, Location: b.Location})
		changes = append(changes, fmt.Sprintf("added block %q %s %s-%s", b.Title, b.Day, b.From, b.To))
	}
	for _, t := range in.RemoveBlocks {
		out := sp.Blocks[:0:0]
		for _, b := range sp.Blocks {
			if !strings.EqualFold(b.Title, t) {
				out = append(out, b)
			}
		}
		if len(out) != len(sp.Blocks) {
			changes = append(changes, "removed block "+t)
		}
		sp.Blocks = out
	}
	for _, x := range in.Skip {
		code, err := canon(x)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if !containsFold(sp.Skip, code) {
			sp.Skip = append(sp.Skip, code)
			changes = append(changes, "skipping "+code+" (apply gives the seat back)")
		}
	}
	for _, x := range in.Unskip {
		out := sp.Skip[:0:0]
		for _, v := range sp.Skip {
			if !strings.EqualFold(v, x) {
				out = append(out, v)
			}
		}
		if len(out) != len(sp.Skip) {
			changes = append(changes, "no longer skipping "+x)
		}
		sp.Skip = out
	}
	if r := in.Rules; r != nil {
		if r.MaxPerDay != nil {
			sp.Rules.MaxPerDay = *r.MaxPerDay
			changes = append(changes, fmt.Sprintf("maxPerDay = %d", *r.MaxPerDay))
		}
		if r.ArriveEarly != nil {
			sp.Rules.ArriveEarly = *r.ArriveEarly
			changes = append(changes, "arriveEarly = "+*r.ArriveEarly)
		}
		if r.Travel != nil {
			sp.Rules.Travel = r.Travel
			changes = append(changes, fmt.Sprintf("travel blocks = %v", *r.Travel))
		}
		if r.Prune != nil {
			sp.Rules.Prune = *r.Prune
			changes = append(changes, fmt.Sprintf("prune = %v", *r.Prune))
		}
	}
	if len(problems) > 0 {
		return errResult("Nothing was saved. Fix these and call edit_plan_file again:\n- %s", strings.Join(problems, "\n- ")), EditOut{}, nil
	}
	if err := sp.Save(path); err != nil {
		return errResult("saving plan file: %v", err), EditOut{}, nil
	}
	fo, err := s.fileOut(ctx, sp)
	if err != nil {
		return errResult("%v", err), EditOut{}, nil
	}
	if changes == nil {
		changes = []string{}
	}
	return nil, EditOut{Changes: changes, File: fo, Next: "Call plan to see what this means for the real schedule."}, nil
}

func containsFold(xs []string, x string) bool {
	for _, v := range xs {
		if strings.EqualFold(v, x) {
			return true
		}
	}
	return false
}

// ---- plan ----

type SkipOut struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}
type PickOut struct {
	Rank     int         `json:"rank"`
	Wanted   string      `json:"wanted"`
	Status   string      `json:"status" jsonschema:"already-reserved, will-reserve, walk-up (takes no reservations: favorited, attend by walking up) or no-fit"`
	Session  *SessionOut `json:"session,omitempty"`
	Fallback bool        `json:"is_fallback"`
	Skipped  []SkipOut   `json:"skipped"`
}
type ChangeOut struct {
	Change string `json:"change" jsonschema:"reserve, cancel, favorite, unfavorite, add-block, update-block, remove-block"`
	What   string `json:"what"`
	When   string `json:"when,omitempty"`
	Reason string `json:"reason,omitempty"`
}
type AgendaItem struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Kind  string `json:"kind" jsonschema:"session, walk or block"`
	What  string `json:"what"`
	Venue string `json:"venue,omitempty"`
}
type AgendaDay struct {
	Day   string       `json:"day"`
	Items []AgendaItem `json:"items"`
}
type PlanOut struct {
	PlanID   string      `json:"plan_id"`
	Event    string      `json:"event"`
	Mode     string      `json:"mode"`
	NoChange bool        `json:"no_changes"`
	Ranked   []PickOut   `json:"ranked"`
	Changes  []ChangeOut `json:"changes"`
	Warnings []string    `json:"warnings"`
	Agenda   []AgendaDay `json:"agenda_after_apply"`
	Next     string      `json:"next"`
}

type planned struct {
	in   plan.Input
	p    *plan.Plan
	cat  *catalog.Catalog
	svc  api.ScheduleService
	mode string
}

func (s *Server) buildPlan(ctx context.Context) (*planned, error) {
	sp, err := spec.Load(s.App.Cfg.SpecPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no plan file at %s yet; create one with edit_plan_file", s.App.Cfg.SpecPath)
	}
	if err != nil {
		return nil, err
	}
	if s.App.Cfg.Event != "" {
		sp.Event = s.App.Cfg.Event
	}
	cat, err := s.App.Catalog(ctx, sp.Event)
	if err != nil {
		return nil, err
	}
	svc, now, err := s.App.Backend(cat)
	if err != nil {
		return nil, err
	}
	sched, err := svc.GetSchedule(ctx, sp.Event)
	if err != nil {
		if api.IsKind(err, api.KindAuthRequired) {
			return nil, fmt.Errorf("reading the schedule needs sign-in: the user must run `replan login` in a terminal (%w)", err)
		}
		return nil, err
	}
	in := plan.Input{Spec: sp, Catalog: cat, Schedule: sched, Matrix: plan.MatrixFor(sp.Rules), Now: now, AssumeReservable: s.App.Cfg.Sim, IgnoreSeatBands: s.App.IgnoreSeatBands()}
	return &planned{in: in, p: plan.Build(in), cat: cat, svc: svc, mode: s.App.Mode()}, nil
}

func (s *Server) planOut(pl *planned) PlanOut {
	p, loc := pl.p, pl.cat.Location
	out := PlanOut{PlanID: p.ID(), Event: p.Event, Mode: pl.mode, NoChange: p.Empty(), Ranked: []PickOut{}, Changes: []ChangeOut{}, Warnings: p.Warnings}
	if out.Warnings == nil {
		out.Warnings = []string{}
	}
	for _, pk := range p.Picks {
		po := PickOut{Rank: pk.Rank, Wanted: pk.Want.Reserve, Skipped: []SkipOut{}}
		for _, sk := range pk.Skipped {
			po.Skipped = append(po.Skipped, SkipOut{sk.Ref, sk.Reason})
		}
		switch {
		case pk.Kept:
			po.Status = "already-reserved"
			if pk.WalkUp {
				po.Status = "walk-up"
			}
		case pk.Chosen != nil:
			po.Status = "will-reserve"
			if pk.WalkUp {
				po.Status = "walk-up"
			}
		default:
			po.Status = "no-fit"
		}
		if pk.Chosen != nil {
			so := sessionOut(pk.Chosen, loc)
			po.Session = &so
			po.Fallback = !strings.EqualFold(pk.Chosen.Code, pk.Want.Reserve)
		}
		out.Ranked = append(out.Ranked, po)
	}
	for _, a := range p.Actions {
		c := ChangeOut{Reason: a.Reason}
		switch a.Kind {
		case plan.Reserve, plan.Cancel, plan.Favorite, plan.Unfavorite:
			c.Change = string(a.Kind)
			c.What = a.SessionID
			if a.Session != nil {
				c.What = a.Session.Code + " " + a.Session.Title
				if a.Session.HasTime() {
					c.When = a.Session.Start.In(loc).Format("Mon 15:04")
				}
			}
		default:
			c.Change = map[plan.ActionKind]string{plan.CreateBlock: "add-block", plan.UpdateBlock: "update-block", plan.DeleteBlock: "remove-block"}[a.Kind]
			c.What = a.Block.Title
			if st, en, err := plan.ParseUTC(a.Block.StartDateTime, a.Block.EndDateTime); err == nil {
				c.When = st.In(loc).Format("Mon 15:04") + "–" + en.In(loc).Format("15:04")
			}
		}
		out.Changes = append(out.Changes, c)
	}
	out.Agenda = agenda(p.Itinerary, loc)
	if p.Empty() {
		out.Next = "No changes: the real schedule already matches the plan file."
	} else {
		out.Next = "Show this plan to the user. If they approve, call apply with plan_id " + out.PlanID + "."
	}
	return out
}

func agenda(slots []plan.Slot, loc *time.Location) []AgendaDay {
	sort.SliceStable(slots, func(i, j int) bool { return slots[i].Start.Before(slots[j].Start) })
	var days []AgendaDay
	for _, sl := range slots {
		d := sl.Start.In(loc).Format("Mon 2006-01-02")
		if len(days) == 0 || days[len(days)-1].Day != d {
			days = append(days, AgendaDay{Day: d, Items: []AgendaItem{}})
		}
		it := AgendaItem{From: sl.Start.In(loc).Format("15:04"), To: sl.End.In(loc).Format("15:04"), What: sl.Label, Kind: "block"}
		switch {
		case sl.Travel:
			it.Kind = "walk"
		case sl.Session != nil:
			it.Kind, it.What, it.Venue = "session", sl.Session.Code+" "+sl.Session.Title, sl.Session.Venue
		}
		days[len(days)-1].Items = append(days[len(days)-1].Items, it)
	}
	if days == nil {
		days = []AgendaDay{}
	}
	return days
}

func renderPlan(pl *planned) string {
	var buf bytes.Buffer
	u := ui.New(&buf)
	u.Loc = pl.cat.Location
	u.Header(pl.cat, pl.mode)
	u.Plan(pl.p)
	u.P("")
	u.Agenda(pl.p.Itinerary)
	u.P("plan_id: %s", pl.p.ID())
	return buf.String()
}

func (s *Server) plan(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, PlanOut, error) {
	pl, err := s.buildPlan(ctx)
	if err != nil {
		return errResult("%v", err), PlanOut{}, nil
	}
	return textResult(renderPlan(pl)), s.planOut(pl), nil
}

// ---- apply ----

type ApplyIn struct {
	PlanID        string `json:"plan_id" jsonschema:"the plan_id returned by plan and shown to the user"`
	UserConfirmed bool   `json:"user_confirmed,omitempty" jsonschema:"set true ONLY if the user explicitly approved this exact plan in the conversation; used when the client cannot show a confirmation dialog"`
}
type ApplyOut struct {
	Applied            bool        `json:"applied"`
	Mode               string      `json:"mode"`
	ConfirmedVia       string      `json:"confirmed_via,omitempty" jsonschema:"dialog (the user answered a confirmation dialog) or conversation"`
	Reserved           []string    `json:"reserved"`
	Log                []string    `json:"log"`
	Problems           int         `json:"problems"`
	ReservationsClosed bool        `json:"reservations_closed"`
	Unverified         []string    `json:"unverified"`
	Agenda             []AgendaDay `json:"agenda"`
	Next               string      `json:"next"`
}

func (s *Server) apply(ctx context.Context, req *mcp.CallToolRequest, in ApplyIn) (*mcp.CallToolResult, ApplyOut, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pl, err := s.buildPlan(ctx)
	if err != nil {
		return errResult("%v", err), ApplyOut{}, nil
	}
	if id := pl.p.ID(); id != in.PlanID {
		return errResult("The plan changed since %s was shown (now %s): the plan file, the catalog or the schedule moved. Call plan again, show the user the new plan, and ask again. Nothing was changed.", in.PlanID, id), ApplyOut{}, nil
	}
	if pl.p.Empty() {
		return textResult("Nothing to apply: the schedule already matches the plan."), ApplyOut{Mode: pl.mode, Reserved: []string{}, Log: []string{}, Unverified: []string{}, Agenda: []AgendaDay{}}, nil
	}

	// Approval. If the client can show a dialog, the human must answer it:
	// the model cannot fill in a dialog, and user_confirmed is ignored. Only
	// clients without dialogs fall back to the conversation's approval.
	via := ""
	if supportsDialog(req) {
		resp, asked := req.Params.InputResponses["approve"]
		if !asked {
			return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{"approve": &mcp.ElicitParams{
				Message: confirmMessage(pl),
				RequestedSchema: &jsonschema.Schema{
					Type: "object",
					Properties: map[string]*jsonschema.Schema{
						"approve": {Type: "boolean", Title: "Apply these changes to my schedule"},
					},
					Required: []string{"approve"},
				},
			}}}, ApplyOut{}, nil
		}
		er, _ := resp.(*mcp.ElicitResult)
		if er == nil || er.Action != "accept" || er.Content["approve"] != true {
			return textResult("The user declined in the confirmation dialog. Nothing was changed."),
				ApplyOut{Mode: pl.mode, Reserved: []string{}, Log: []string{}, Unverified: []string{}, Agenda: []AgendaDay{}}, nil
		}
		via = "dialog"
	} else if in.UserConfirmed {
		via = "conversation"
	} else {
		return errResult("Approval needed. This client cannot show a confirmation dialog, so: show the user the plan (plan_id %s), ask them to approve it, and only after an explicit yes call apply again with user_confirmed=true. Nothing was changed.", in.PlanID), ApplyOut{}, nil
	}

	var log []string
	r := &apply.Runner{API: pl.svc, Input: pl.in, Progress: func(e apply.Event) {
		sym := map[apply.EventKind]string{apply.EvOK: "✓", apply.EvFail: "✗", apply.EvSkip: "↷", apply.EvInfo: "i", apply.EvWarning: "!"}[e.Kind]
		log = append(log, fmt.Sprintf("%s %s %s: %s", sym, e.Action, e.Subject, e.Detail))
	}}
	result, err := r.Run(ctx, pl.p)
	if err != nil {
		return errResult("apply stopped: %v\nSteps completed before stopping:\n%s", err, strings.Join(log, "\n")), ApplyOut{}, nil
	}
	out := ApplyOut{Applied: true, Mode: pl.mode, ConfirmedVia: via, Log: log, Problems: result.Failed, ReservationsClosed: result.ReservationsOff,
		Reserved: []string{}, Unverified: result.Unverified}
	if out.Log == nil {
		out.Log = []string{}
	}
	if out.Unverified == nil {
		out.Unverified = []string{}
	}
	for _, id := range result.Reserved {
		if ss, ok := pl.cat.Lookup(id); ok {
			out.Reserved = append(out.Reserved, ss.Code)
		}
	}
	out.Agenda = []AgendaDay{}
	if result.Final != nil {
		in := pl.in
		in.Schedule = result.Final
		out.Agenda = agenda(plan.Build(apply.Held(in)).Itinerary, pl.cat.Location)
	}
	switch {
	case result.ReservationsOff:
		out.Next = "Reservations are closed right now; favorites and blocks were applied. Try apply again (via plan) once reserved seating opens."
	case result.Failed > 0:
		out.Next = "Some steps failed; tell the user exactly which (see log). Call plan to see what is still missing."
	default:
		out.Next = "Done and verified against the live schedule. Summarize the reserved sessions and walking blocks for the user."
	}
	text := fmt.Sprintf("Apply complete (%s, confirmed via %s): %d reserved, %d seat(s) given back, %d problems.\n%s", pl.mode, via, len(out.Reserved), len(result.Released), out.Problems, strings.Join(log, "\n"))
	return textResult(text), out, nil
}

func supportsDialog(req *mcp.CallToolRequest) bool {
	ip := req.Session.InitializeParams()
	return ip != nil && ip.Capabilities != nil && ip.Capabilities.Elicitation != nil
}

func confirmMessage(pl *planned) string {
	var b strings.Builder
	fmt.Fprintf(&b, "replan wants to change your %s schedule [%s]:\n", pl.cat.Event.Name, pl.mode)
	loc := pl.cat.Location
	for _, a := range pl.p.Actions {
		switch a.Kind {
		case plan.Reserve, plan.Cancel:
			when := ""
			if a.Session != nil && a.Session.HasTime() {
				when = strings.TrimSpace(" " + a.Session.Start.In(loc).Format("Mon 15:04") + " " + a.Session.Venue)
				when = " " + when
			}
			code := a.SessionID
			if a.Session != nil {
				code = a.Session.Code
			}
			fmt.Fprintf(&b, "• %s %s%s\n", a.Kind, code, when)
		}
	}
	p := pl.p
	blk := p.Count(plan.CreateBlock) + p.Count(plan.UpdateBlock) + p.Count(plan.DeleteBlock)
	fmt.Fprintf(&b, "Plus %d favorite changes and %d personal-time blocks (walking time, meals).\nIf a session is full, its listed fallbacks are tried in order.", p.Count(plan.Favorite)+p.Count(plan.Unfavorite), blk)
	return b.String()
}

// ---- agenda ----

type AgendaOut struct {
	Event     string       `json:"event"`
	Mode      string       `json:"mode"`
	Days      []AgendaDay  `json:"days"`
	Favorites []SessionOut `json:"favorites"`
}

func (s *Server) getAgenda(ctx context.Context, _ *mcp.CallToolRequest, in struct {
	Event string `json:"event,omitempty"`
}) (*mcp.CallToolResult, AgendaOut, error) {
	ev, err := s.App.ResolveEvent(in.Event)
	if err != nil {
		return errResult("%v", err), AgendaOut{}, nil
	}
	cat, err := s.App.Catalog(ctx, ev)
	if err != nil {
		return errResult("loading catalog: %v", err), AgendaOut{}, nil
	}
	svc, _, err := s.App.Backend(cat)
	if err != nil {
		return errResult("%v", err), AgendaOut{}, nil
	}
	sched, err := svc.GetSchedule(ctx, ev)
	if err != nil {
		return errResult("reading schedule: %v", err), AgendaOut{}, nil
	}
	var slots []plan.Slot
	for _, id := range sched.Reserved {
		if ss, ok := cat.Lookup(id); ok {
			slots = append(slots, plan.Slot{Start: ss.Start, End: ss.End, Venue: ss.Venue, Label: ss.Code, Session: ss})
		}
	}
	for _, pt := range sched.PersonalTime {
		st, en, err := plan.ParseUTC(pt.StartDateTime, pt.EndDateTime)
		if err != nil {
			continue
		}
		_, managed := plan.MarkerKey(pt)
		slots = append(slots, plan.Slot{Start: st, End: en, Label: pt.Title, Travel: managed && strings.HasPrefix(pt.Title, "Walk:")})
	}
	out := AgendaOut{Event: ev, Mode: s.App.Mode(), Days: agenda(slots, cat.Location), Favorites: []SessionOut{}}
	for _, id := range sched.Favorites {
		out.Favorites = append(out.Favorites, refOut(cat, id))
	}
	return nil, out, nil
}

// ---- prompt ----

func (s *Server) planMyWeek(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	args := req.Params.Arguments
	ev := args["event"]
	if ev == "" {
		ev = "the event in my plan file (or reinvent2026)"
	}
	text := fmt.Sprintf(`Help me plan my week at %s with replan.

My goals: %s
Constraints: %s

1. Ask me up to three short questions if my goals are vague (days attending, level, pace).
2. Search the catalog. Favour sessions that won't be recorded when the value is being in the room.
3. Build a ranked plan with edit_plan_file: most important first, a fallback for each (repeats are
   ideal), and blocks for anything I mentioned (meals, meetings).
4. Call plan and walk me through it: what gets reserved, what was skipped and why, and the walking
   blocks that will appear in my official event app.
5. Ask me before applying. Only call apply after I say yes, then tell me what was verified.`,
		ev, orDefault(args["goals"], "(ask me)"), orDefault(args["constraints"], "none given"))
	return &mcp.GetPromptResult{
		Description: "Plan a re:Invent week with replan",
		Messages:    []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}},
	}, nil
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

// SessionView is the shape used for sessions everywhere replan talks JSON
// (MCP tools and the local web UI).
func SessionView(s *catalog.Session, loc *time.Location) SessionOut { return sessionOut(s, loc) }

func (s *Server) runningLate(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	a := req.Params.Arguments
	text := fmt.Sprintf(`I'm at re:Invent and running late. I'm at %s and can't leave for %s minutes.

1. Call whats_next with where I am and how late I am.
2. Tell me plainly what I'll still make and what I won't.
3. For a session I won't make: offer to give the seat back (edit_plan_file skip) so someone in the
   walk-up line gets it, and suggest the best reachable alternative (add it to the plan if I want).
4. Show me the plan, and apply only after I say yes.`, orDefault(a["where"], "(ask me)"), orDefault(a["minutes"], "0"))
	return &mcp.GetPromptResult{Description: "Running late at re:Invent",
		Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}}}, nil
}
