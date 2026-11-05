// Command replan manages your re:Invent schedule as code.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/awsfanboy/replan/internal/api"
	"github.com/awsfanboy/replan/internal/app"
	"github.com/awsfanboy/replan/internal/apply"
	"github.com/awsfanboy/replan/internal/auth"
	"github.com/awsfanboy/replan/internal/catalog"
	"github.com/awsfanboy/replan/internal/mcpserver"
	"github.com/awsfanboy/replan/internal/plan"
	"github.com/awsfanboy/replan/internal/setup"
	"github.com/awsfanboy/replan/internal/spec"
	"github.com/awsfanboy/replan/internal/ui"
	"github.com/awsfanboy/replan/internal/web"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const defaultFullRate = 0.15

const usage = `replan: your re:Invent schedule as code

Usage:
  replan login                       sign in with AWS Builder ID
  replan logout                      revoke and forget your tokens
  replan whoami                      show who is signed in
  replan events [--past]             list AWS events
  replan search [terms] [flags]      search the catalog (--day --kind --venue --level --topic --live --page)
  replan ui [--sim] [--port N]       browse, filter and build your plan in the browser
  replan show CODE                   session details and repeats
  replan init [--event ID]           write a starter reinvent.yaml
  replan plan                        show what would change
  replan apply [--auto-approve]      make the schedule match reinvent.yaml
  replan agenda                      your current schedule, day by day

During the event (replan Live):
  replan now [--late 20m] [--at Wynn]   what's next, when to leave, will you make it, alternatives
  replan tonight [--day 2026-12-02]     reservations you can't realistically keep: give them back
  replan changes                        what changed in the catalog that matters to you
  replan skip CODE / unskip CODE        mark a session you won't attend (plan/apply gives the seat back)
  --now "tue 10:40"                     time travel, for demos and what-ifs (any command)
  replan snapshot -o FILE            save the catalog for offline demos
  replan mcp [-f FILE] [--sim]       run as an MCP server for AI agents (stdio)
  replan setup kiro|claude [--sim]   connect Kiro CLI or Claude Code to replan

Common flags:
  -f FILE         spec file (default reinvent.yaml)
  --event ID      event (default: from reinvent.yaml)
  --sim           use a local simulator instead of your real schedule
  --catalog FILE  use a saved catalog snapshot instead of the API
  --refresh       refetch the catalog even if the cache is fresh
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "login":
		err = cmdLogin(ctx)
	case "logout":
		err = cmdLogout(ctx)
	case "whoami":
		err = cmdWhoami()
	case "events":
		err = cmdEvents(ctx, args)
	case "search":
		err = cmdSearch(ctx, args)
	case "show":
		err = cmdShow(ctx, args)
	case "init":
		err = cmdInit(ctx, args)
	case "plan":
		err = cmdPlan(ctx, args, false)
	case "apply":
		err = cmdPlan(ctx, args, true)
	case "agenda":
		err = cmdAgenda(ctx, args)
	case "snapshot":
		err = cmdSnapshot(ctx, args)
	case "mcp":
		err = cmdMCP(ctx, args)
	case "setup":
		err = cmdSetup(args)
	case "ui":
		err = cmdUI(ctx, args)
	case "now":
		err = cmdNow(ctx, args)
	case "tonight":
		err = cmdTonight(ctx, args)
	case "changes":
		err = cmdChanges(ctx, args)
	case "skip", "unskip":
		err = cmdSkip(cmd, args)
	case "sim":
		err = cmdSimTool(ctx, args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// ---- shared options ----

type opts struct {
	file                    string
	event                   string
	sim                     bool
	simFull                 float64
	simFullList             string
	simReset                bool
	simClosed               bool
	simOpen                 bool
	catalog                 string
	refresh                 bool
	autoApprove             bool
	past                    bool
	day, kind, venue, level string
	live                    bool
	limit                   int
	out                     string
	topic                   string
	page                    int
	asJSON                  bool
	noPager                 bool
	now, at, since, compare string
	late                    time.Duration
}

func flags(name string, o *opts) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.StringVar(&o.file, "f", "reinvent.yaml", "spec file")
	fs.StringVar(&o.event, "event", "", "event ID (default: from spec file)")
	fs.BoolVar(&o.sim, "sim", false, "use the local simulator instead of your real schedule")
	fs.Float64Var(&o.simFull, "sim-full-rate", defaultFullRate, "simulator: fraction of sessions that are randomly full (0 when --sim-full is given)")
	fs.StringVar(&o.simFullList, "sim-full", "", "simulator: comma-separated session codes that are full")
	fs.BoolVar(&o.simReset, "sim-reset", false, "simulator: start from an empty schedule")
	fs.BoolVar(&o.simClosed, "sim-closed", false, "simulator: reservations closed (409)")
	fs.BoolVar(&o.simOpen, "sim-open-seats", false, "simulator: ignore the catalog's real seat bands (for demos once sessions are full)")
	fs.StringVar(&o.catalog, "catalog", "", "catalog snapshot file")
	fs.BoolVar(&o.refresh, "refresh", false, "refetch the catalog")
	fs.BoolVar(&o.autoApprove, "auto-approve", false, "apply without asking")
	fs.BoolVar(&o.past, "past", false, "include past events")
	fs.StringVar(&o.day, "day", "", "day: YYYY-MM-DD or weekday")
	fs.StringVar(&o.kind, "kind", "", "session kind(s), comma-separated: breakout,chalk-talk,workshop,builders-session,code-talk,...")
	fs.StringVar(&o.venue, "venue", "", "venue name contains")
	fs.StringVar(&o.level, "level", "", "level(s), comma-separated: 200,300,400")
	fs.BoolVar(&o.live, "live", false, "only sessions unlikely to be recorded")
	fs.IntVar(&o.limit, "n", 25, "results per page (0 = all)")
	fs.StringVar(&o.topic, "topic", "", "topic, AWS service or interest area contains, e.g. serverless, bedrock, security")
	fs.IntVar(&o.page, "page", 1, "page of results")
	fs.BoolVar(&o.asJSON, "json", false, "print results as JSON")
	fs.BoolVar(&o.noPager, "no-pager", false, "don't page long output through less")
	fs.StringVar(&o.now, "now", "", "time travel: act as if it were this time, e.g. \"tue 10:40\" or 2026-12-01T10:40 (event time)")
	fs.StringVar(&o.at, "at", "", "where you are now, e.g. Wynn, \"Caesars Forum\" (default: inferred from your schedule)")
	fs.DurationVar(&o.late, "late", 0, "you can't leave for this long, e.g. 20m")
	fs.StringVar(&o.since, "since", "72h", "changes: how far back to look")
	fs.StringVar(&o.compare, "compare", "", "changes: compare against this catalog snapshot instead of the recorded history")
	fs.StringVar(&o.out, "o", "", "output file")
	return fs
}

// parse allows flags before and after positional arguments.
func parse(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		_ = fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func (o *opts) resolveEvent() error {
	if o.event != "" {
		return nil
	}
	if sp, err := spec.Load(o.file); err == nil {
		o.event = sp.Event
		return nil
	}
	return errors.New("no event: pass --event ID or create reinvent.yaml with `replan init --event ID`")
}

func tokenStore() *auth.Store {
	st, err := auth.DefaultStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	return st
}

// newApp builds the shared application from command-line options.
func newApp(o *opts) *app.App {
	var full []string
	rate := o.simFull
	if o.simFullList != "" {
		full = strings.Split(o.simFullList, ",")
		if rate == defaultFullRate {
			rate = 0 // named full sessions make the demo deterministic
		}
	}
	return app.New(app.Config{SpecPath: o.file, Event: o.event, Sim: o.sim, SimFullRate: rate, SimFull: full,
		SimReset: o.simReset, SimClosed: o.simClosed, SimOpen: o.simOpen, CatalogFile: o.catalog, Refresh: o.refresh, CompareFile: o.compare})
}

func client() *api.Client { return app.New(app.Config{}).Client() }

func loadCatalog(ctx context.Context, o *opts) (*catalog.Catalog, error) {
	return newApp(o).Catalog(ctx, o.event)
}

func backend(cat *catalog.Catalog, o *opts) (api.ScheduleService, time.Time, error) {
	a := newApp(o)
	if o.now != "" {
		t, err := app.ParseNow(o.now, cat)
		if err != nil {
			return nil, time.Time{}, err
		}
		a.Cfg.Now = t
	}
	return a.Backend(cat)
}

// ---- commands ----

func cmdLogin(ctx context.Context) error {
	st := tokenStore()
	t, err := auth.Login(ctx, http.DefaultClient, func(s string) { fmt.Println(s) })
	if err != nil {
		return err
	}
	if err := st.Save(t); err != nil {
		return err
	}
	who := t.Email()
	if who == "" {
		who = "your Builder ID"
	}
	fmt.Printf("Signed in as %s. Tokens saved to %s (readable only by you).\n", who, st.Path)
	return nil
}

func cmdLogout(ctx context.Context) error {
	st := tokenStore()
	t, err := st.Load()
	if err == nil && t.RefreshToken != "" {
		if err := auth.Revoke(ctx, http.DefaultClient, t.RefreshToken); err != nil {
			fmt.Fprintln(os.Stderr, "warning: revoking refresh token:", err)
		}
	}
	if err := st.Delete(); err != nil {
		return err
	}
	fmt.Println("Signed out of replan. To end your Builder ID browser session too, visit https://profile.aws.amazon.com")
	return nil
}

func cmdWhoami() error {
	t, err := tokenStore().Load()
	if err != nil {
		fmt.Println("Not signed in. Run `replan login`.")
		return nil
	}
	fmt.Printf("%s (access token valid until %s)\n", t.Email(), t.Expiry.Local().Format(time.Kitchen))
	return nil
}

func cmdEvents(ctx context.Context, args []string) error {
	var o opts
	parse(flags("events", &o), args)
	evs, err := client().ListEvents(ctx, o.past)
	if err != nil {
		return err
	}
	u := ui.New(os.Stdout)
	for _, e := range evs {
		lock := "public"
		if e.AuthenticationRequired {
			lock = "registration required"
		}
		city := ""
		if e.Address != nil {
			city = e.Address.City
		}
		u.P("  %-36s %-44s %s  %-14s %s", e.EventID, e.Name, e.StartDate[:min(10, len(e.StartDate))], city, u.Dim(lock))
	}
	return nil
}

func cmdSearch(ctx context.Context, args []string) error {
	var o opts
	pos := parse(flags("search", &o), args)
	if err := o.resolveEvent(); err != nil {
		return err
	}
	cat, err := loadCatalog(ctx, &o)
	if err != nil {
		return err
	}
	f := catalog.Filter{Day: o.day, Venue: o.venue, OnlyLive: o.live, Topic: o.topic}
	if o.kind != "" {
		f.Kinds = strings.Split(o.kind, ",")
	}
	if o.level != "" {
		f.Levels = strings.Split(o.level, ",")
	}
	query := strings.Join(pos, " ")
	hits := cat.Search(query, f)

	if o.asJSON {
		type row struct {
			Code, Title, Kind, Level, Venue, Room, Day, Start, End string
			Recorded                                               bool
			Topics                                                 []string
		}
		out := []row{}
		for _, h := range hits {
			s := h.Session
			r := row{Code: s.Code, Title: s.Title, Kind: s.Kind, Level: s.Level, Venue: s.Venue, Room: s.Room, Recorded: s.Recorded, Topics: s.Topics}
			if s.HasTime() {
				st := s.Start.In(cat.Location)
				r.Day, r.Start, r.End = st.Format("2006-01-02"), st.Format("15:04"), s.End.In(cat.Location).Format("15:04")
			}
			out = append(out, r)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	w, done := pager(o.noPager)
	defer done()
	u := ui.New(w)
	u.Color = isTerminal()
	u.Loc = cat.Location

	browsing := query == "" && o.day == "" && o.kind == "" && o.venue == "" && o.level == "" && o.topic == "" && !o.live
	if browsing && o.page == 1 {
		u.Overview(cat, cat.Facets(cat.Sessions))
		return nil
	}
	per := o.limit
	if per <= 0 {
		per = len(hits)
	}
	from := (max(o.page, 1) - 1) * per
	to := min(from+per, len(hits))
	u.P(u.Dim(fmt.Sprintf("%d matches in %s", len(hits), cat.Event.Name)))
	if from >= len(hits) {
		u.P("No results on page %d.", o.page)
		return nil
	}
	u.Sessions(hits[from:to], 0)
	if to < len(hits) {
		u.P("")
		u.P(u.Dim(fmt.Sprintf("Showing %d–%d of %d. Next: add --page %d  ·  all: -n 0  ·  narrow: --day --venue --kind --topic --level --live  ·  browse visually: replan ui",
			from+1, to, len(hits), o.page+1)))
	}
	return nil
}

func isTerminal() bool {
	st, err := os.Stdout.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == ""
}

// pager pipes output through less when stdout is a terminal.
func pager(disabled bool) (io.Writer, func()) {
	if disabled || !isTerminal() {
		return os.Stdout, func() {}
	}
	path, err := exec.LookPath("less")
	if err != nil {
		return os.Stdout, func() {}
	}
	cmd := exec.Command(path, "-R", "-F", "-X", "-S")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil || cmd.Start() != nil {
		return os.Stdout, func() {}
	}
	return in, func() { in.Close(); cmd.Wait() }
}

func cmdShow(ctx context.Context, args []string) error {
	var o opts
	pos := parse(flags("show", &o), args)
	if len(pos) != 1 {
		return errors.New("usage: replan show CODE")
	}
	if err := o.resolveEvent(); err != nil {
		return err
	}
	cat, err := loadCatalog(ctx, &o)
	if err != nil {
		return err
	}
	s, ok := cat.Lookup(pos[0])
	if !ok {
		return fmt.Errorf("%s not found in %s", pos[0], o.event)
	}
	u := ui.New(os.Stdout)
	u.Loc = cat.Location
	u.Session(s, cat.Repeats(s))
	return nil
}

func cmdInit(ctx context.Context, args []string) error {
	var o opts
	parse(flags("init", &o), args)
	if o.event == "" {
		o.event = "reinvent2026"
	}
	if _, err := os.Stat(o.file); err == nil {
		return fmt.Errorf("%s already exists", o.file)
	}
	if err := os.WriteFile(o.file, []byte(spec.Starter(o.event, nil)), 0o644); err != nil {
		return err
	}
	fmt.Printf("Wrote %s. Find sessions with `replan search`, list them in rank order, then run `replan plan`.\n", o.file)
	return nil
}

func cmdPlan(ctx context.Context, args []string, doApply bool) error {
	var o opts
	parse(flags("plan", &o), args)
	sp, err := spec.Load(o.file)
	if err != nil {
		return err
	}
	if o.event == "" {
		o.event = sp.Event
	}
	sp.Event = o.event
	cat, err := loadCatalog(ctx, &o)
	if err != nil {
		return err
	}
	svc, now, err := backend(cat, &o)
	if err != nil {
		return err
	}
	u := ui.New(os.Stdout)
	u.Loc = cat.Location
	u.Header(cat, newApp(&o).Mode())

	sched, err := svc.GetSchedule(ctx, o.event)
	if err != nil {
		if api.IsKind(err, api.KindAuthRequired) {
			return fmt.Errorf("reading your schedule needs sign-in: run `replan login`, or try `--sim` (%w)", err)
		}
		return err
	}
	in := plan.Input{Spec: sp, Catalog: cat, Schedule: sched, Matrix: plan.MatrixFor(sp.Rules), Now: now, AssumeReservable: o.sim, IgnoreSeatBands: o.sim && o.simOpen}
	p := plan.Build(in)
	u.Plan(p)
	u.P("")
	u.Agenda(p.Itinerary)
	if !doApply || p.Empty() {
		return nil
	}

	if !o.autoApprove {
		fmt.Print(u.Bold("Do you want to perform these actions?") + " Only 'yes' will be accepted: ")
		line, rerr := bufio.NewReader(os.Stdin).ReadString('\n')
		if rerr != nil && strings.TrimSpace(line) == "" {
			fmt.Println("\nApply cancelled: no input. Run it in a terminal, or pass --auto-approve.")
			return nil
		}
		if strings.TrimSpace(line) != "yes" {
			fmt.Println("Apply cancelled.")
			return nil
		}
	}
	u.P("")
	r := &apply.Runner{API: svc, Input: in, Progress: u.Progress}
	res, err := r.Run(ctx, p)
	if err != nil {
		return err
	}
	u.P("")
	if res.ReservationsOff {
		u.P(u.Yellow("Reservations are closed right now. Run `replan apply` again once they open."))
	}
	for _, x := range res.Unverified {
		u.P("%s %s was reported done but is not on your schedule", u.Yellow("!"), x)
	}
	summary := fmt.Sprintf("Apply complete: %d reserved, %d problems.", len(res.Reserved), res.Failed)
	if n := len(res.Released); n > 0 {
		summary = fmt.Sprintf("Apply complete: %d reserved, %d seat(s) given back to the walk-up line, %d problems.", len(res.Reserved), n, res.Failed)
	}
	if res.Failed > 0 {
		u.P(u.Yellow(summary))
	} else {
		u.P(u.Green(summary))
	}
	u.P(u.Dim("Verified against GetSchedule. Run `replan plan` any time to check for drift."))
	return nil
}

func cmdAgenda(ctx context.Context, args []string) error {
	var o opts
	parse(flags("agenda", &o), args)
	if err := o.resolveEvent(); err != nil {
		return err
	}
	cat, err := loadCatalog(ctx, &o)
	if err != nil {
		return err
	}
	svc, _, err := backend(cat, &o)
	if err != nil {
		return err
	}
	sched, err := svc.GetSchedule(ctx, o.event)
	if err != nil {
		return err
	}
	var slots []plan.Slot
	for _, id := range sched.Reserved {
		if s, ok := cat.Lookup(id); ok {
			slots = append(slots, plan.Slot{Start: s.Start, End: s.End, Venue: s.Venue, Label: s.Code, Session: s, Managed: true})
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
	sortSlots(slots)
	u := ui.New(os.Stdout)
	u.Loc = cat.Location
	u.Agenda(slots)
	if len(sched.Favorites) > 0 {
		u.P(u.Dim(fmt.Sprintf("%d favorites (interest only, no seat held)", len(sched.Favorites))))
	}
	return nil
}

func sortSlots(s []plan.Slot) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Start.Before(s[j-1].Start); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func cmdSnapshot(ctx context.Context, args []string) error {
	var o opts
	parse(flags("snapshot", &o), args)
	if err := o.resolveEvent(); err != nil {
		return err
	}
	if o.out == "" {
		o.out = "catalog-" + o.event + ".json"
	}
	cat, err := loadCatalog(ctx, &o)
	if err != nil {
		return err
	}
	if err := cat.SaveFile(o.out); err != nil {
		return err
	}
	fmt.Printf("Saved %d sessions of %s to %s\n", len(cat.Sessions), cat.Event.Name, o.out)
	return nil
}

func cmdMCP(ctx context.Context, args []string) error {
	var o opts
	parse(flags("mcp", &o), args)
	abs, err := filepath.Abs(o.file)
	if err != nil {
		return err
	}
	o.file = abs
	// stdout carries the protocol: log only to stderr.
	fmt.Fprintf(os.Stderr, "replan MCP server: plan file %s, %s\n", o.file, map[bool]string{true: "SIMULATOR", false: "live schedule"}[o.sim])
	a := newApp(&o)
	if err := timeTravel(ctx, a, &o); err != nil {
		return err
	}
	return mcpserver.New(a).Run(ctx, &mcp.StdioTransport{})
}

func cmdSetup(args []string) error {
	var o opts
	fs := flags("setup", &o)
	name := fs.String("name", "replan", "agent / server name")
	workspace := fs.Bool("workspace", false, "kiro: write .kiro/agents in this directory instead of ~/.kiro/agents")
	force := fs.Bool("force", false, "kiro: replace an agent file replan did not write")
	pos := parse(fs, args)
	if len(pos) != 1 || (pos[0] != "kiro" && pos[0] != "claude") {
		return errors.New("usage: replan setup kiro|claude [-f reinvent.yaml] [--sim] [--sim-full CODES] [--name replan] [--workspace]")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	spec, err := filepath.Abs(o.file)
	if err != nil {
		return err
	}
	so := setup.Options{Binary: exe, SpecPath: spec, Sim: o.sim, SimFull: o.simFullList, Name: *name, Now: o.now}
	so.SimOpen = o.sim && o.simOpen
	if o.sim && o.simFull != defaultFullRate {
		so.SimFullRate = strconv.FormatFloat(o.simFull, 'f', -1, 64)
	}
	for _, p := range []struct {
		src string
		dst *string
	}{{o.catalog, &so.Catalog}, {o.compare, &so.Compare}} {
		if p.src != "" {
			if *p.dst, err = filepath.Abs(p.src); err != nil {
				return err
			}
		}
	}
	if pos[0] == "claude" {
		fmt.Println("Run this to connect Claude Code:")
		fmt.Println()
		fmt.Println("  " + setup.ClaudeCommand(so))
		return nil
	}
	path, err := setup.KiroAgentPath(*name, *workspace)
	if err != nil {
		return err
	}
	if err := setup.WriteKiroAgent(path, so, *force); err != nil {
		return err
	}
	fmt.Printf("Wrote Kiro CLI agent %q to %s\n", *name, path)
	fmt.Printf("  plan file: %s", spec)
	if o.sim {
		fmt.Print("  (simulator: nothing real changes)")
	}
	fmt.Println()
	fmt.Println("  auto-approved: read-only tools and plan-file edits; apply always asks you first")
	fmt.Println()
	fmt.Printf("Start it:\n\n  kiro-cli chat --agent %s\n", *name)
	return nil
}

func cmdUI(ctx context.Context, args []string) error {
	var o opts
	fs := flags("ui", &o)
	port := fs.Int("port", 0, "port on 127.0.0.1 (default: a free one)")
	noOpen := fs.Bool("no-open", false, "don't open the browser")
	parse(fs, args)
	abs, err := filepath.Abs(o.file)
	if err != nil {
		return err
	}
	o.file = abs
	a := newApp(&o)
	ev, err := a.ResolveEvent(o.event)
	if err != nil {
		ev = "reinvent2026"
	}
	if _, err := a.Catalog(ctx, ev); err != nil { // fail early, with the sign-in hint
		return err
	}
	o.event = ev
	if err := timeTravel(ctx, a, &o); err != nil {
		return err
	}
	url, stop, err := web.Start(ctx, a, ev, fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		return err
	}
	defer stop()
	fmt.Printf("replan UI for %s (%s)\n  %s\nplan file: %s\nPress Ctrl+C to stop.\n", ev, a.Mode(), url, abs)
	if !*noOpen {
		openURL(url)
	}
	<-ctx.Done()
	return nil
}

func openURL(u string) {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", u).Start()
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	default:
		exec.Command("xdg-open", u).Start()
	}
}

// timeTravel applies --now to a long-running app (MCP server, UI).
func timeTravel(ctx context.Context, a *app.App, o *opts) error {
	if o.now == "" {
		return nil
	}
	ev, err := a.ResolveEvent(o.event)
	if err != nil {
		return err
	}
	cat, err := a.Catalog(ctx, ev)
	if err != nil {
		return err
	}
	t, err := app.ParseNow(o.now, cat)
	if err != nil {
		return err
	}
	a.Cfg.Now = t
	return nil
}
