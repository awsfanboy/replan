package mcpserver_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/awsfanboy/replan/internal/api"
	"github.com/awsfanboy/replan/internal/app"
	"github.com/awsfanboy/replan/internal/mcpserver"
	tu "github.com/awsfanboy/replan/internal/testutil"
)

var campus = tu.Catalog(
	tu.S{Code: "AIM301", Room: "Wynn | Lafite 1", Clock: "09:00"},
	tu.S{Code: "AIM301-R", Room: "Wynn | Lafite 2", Clock: "14:00"},
	tu.S{Code: "DEV402", Type: "Chalk talk", Room: "Caesars Forum | Academy 410", Clock: "11:00"},
	tu.S{Code: "SVS401", Type: "Workshop", Room: "Encore | Bluethorn 1", Clock: "16:00", Minutes: 120},
)

// connect starts the server with a fresh plan file and returns a client session.
func connect(t *testing.T, elicit func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)) (*mcp.ClientSession, *app.App) {
	t.Helper()
	a := app.New(app.Config{SpecPath: filepath.Join(t.TempDir(), "reinvent.yaml"), Sim: true})
	a.SetCatalog(campus)
	srv := mcpserver.New(a)
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ClientOptions{ElicitationHandler: elicit})
	cs, err := c.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, a
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if out != nil && res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: decoding: %v", name, err)
		}
	}
	return res
}

func text(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func buildPlan(t *testing.T, cs *mcp.ClientSession) mcpserver.PlanOut {
	t.Helper()
	res := call(t, cs, "edit_plan_file", map[string]any{
		"event": "reinvent2026",
		"add": []map[string]any{
			{"reserve": "AIM301", "fallbacks": []string{"AIM301-R"}, "why": "agents"},
			{"reserve": "DEV402"},
		},
		"add_favorites": []string{"SVS401"},
	}, nil)
	if res.IsError {
		t.Fatalf("edit: %s", text(res))
	}
	var p mcpserver.PlanOut
	if res := call(t, cs, "plan", map[string]any{}, &p); res.IsError {
		t.Fatalf("plan: %s", text(res))
	}
	return p
}

func TestToolsAreListedWithSafetyAnnotations(t *testing.T) {
	cs, _ := connect(t, nil)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		got[tl.Name] = tl
	}
	for _, n := range []string{"list_events", "search_sessions", "get_session", "get_plan_file", "edit_plan_file", "plan", "apply", "get_agenda"} {
		if got[n] == nil {
			t.Errorf("missing tool %s", n)
		}
	}
	if a := got["apply"].Annotations; a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Errorf("apply must be marked destructive: %+v", a)
	}
	if !got["plan"].Annotations.ReadOnlyHint {
		t.Error("plan must be read-only")
	}
}

func TestSearchAndSessionDetails(t *testing.T) {
	cs, _ := connect(t, nil)
	var out mcpserver.SearchOut
	call(t, cs, "search_sessions", map[string]any{"event": "reinvent2026", "live_only": true}, &out)
	if out.Total != 2 { // chalk talk + workshop; breakouts are likely recorded
		t.Fatalf("live_only search: %+v", out)
	}
	var d mcpserver.GetSessionOut
	call(t, cs, "get_session", map[string]any{"code": "aim301", "event": "reinvent2026"}, &d)
	if d.Session.Venue != "Wynn" || len(d.Repeats) != 1 || d.Repeats[0].Code != "AIM301-R" {
		t.Fatalf("details: %+v", d)
	}
}

func TestEditRejectsUnknownCodesAndSavesNothing(t *testing.T) {
	cs, _ := connect(t, nil)
	res := call(t, cs, "edit_plan_file", map[string]any{"event": "reinvent2026", "add": []map[string]any{{"reserve": "NOPE1"}}}, nil)
	if !res.IsError || !strings.Contains(text(res), "NOPE1") {
		t.Fatalf("want error naming NOPE1, got %s", text(res))
	}
	var f mcpserver.PlanFileOut
	call(t, cs, "get_plan_file", map[string]any{}, &f)
	if f.Exists {
		t.Fatal("nothing should have been saved")
	}
}

func TestApplyNeedsApprovalWithoutDialog(t *testing.T) {
	cs, _ := connect(t, nil) // client cannot show dialogs
	p := buildPlan(t, cs)
	if p.NoChange || len(p.Ranked) != 2 || p.Ranked[0].Status != "will-reserve" {
		t.Fatalf("plan: %+v", p)
	}

	res := call(t, cs, "apply", map[string]any{"plan_id": p.PlanID}, nil)
	if !res.IsError || !strings.Contains(text(res), "Approval needed") {
		t.Fatalf("apply without approval must refuse: %s", text(res))
	}

	var out mcpserver.ApplyOut
	res = call(t, cs, "apply", map[string]any{"plan_id": p.PlanID, "user_confirmed": true}, &out)
	if res.IsError || !out.Applied || out.ConfirmedVia != "conversation" || len(out.Reserved) != 2 {
		t.Fatalf("apply: %s %+v", text(res), out)
	}

	var again mcpserver.PlanOut
	call(t, cs, "plan", map[string]any{}, &again)
	if !again.NoChange {
		t.Fatalf("plan after apply should be clean: %+v", again.Changes)
	}
	var ag mcpserver.AgendaOut
	call(t, cs, "get_agenda", map[string]any{}, &ag)
	if len(ag.Days) != 1 || len(ag.Favorites) != 1 {
		t.Fatalf("agenda: %+v", ag)
	}
	walks := 0
	for _, it := range ag.Days[0].Items {
		if it.Kind == "walk" {
			walks++
		}
	}
	if walks != 1 { // Wynn → Caesars Forum
		t.Fatalf("want 1 walking block, got %+v", ag.Days[0].Items)
	}
}

func TestStalePlanIsRefused(t *testing.T) {
	cs, _ := connect(t, nil)
	p := buildPlan(t, cs)
	call(t, cs, "edit_plan_file", map[string]any{"remove": []string{"DEV402"}}, nil)
	res := call(t, cs, "apply", map[string]any{"plan_id": p.PlanID, "user_confirmed": true}, nil)
	if !res.IsError || !strings.Contains(text(res), "plan changed") {
		t.Fatalf("stale plan must be refused: %s", text(res))
	}
}

func TestDialogApprovalAndDecline(t *testing.T) {
	var asked string
	approve := true
	cs, a := connect(t, func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		asked = req.Params.Message
		if !approve {
			return &mcp.ElicitResult{Action: "decline"}, nil
		}
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}, nil
	})
	p := buildPlan(t, cs)

	approve = false
	res := call(t, cs, "apply", map[string]any{"plan_id": p.PlanID, "user_confirmed": true}, nil)
	if !strings.Contains(text(res), "declined") {
		t.Fatalf("decline: %s", text(res))
	}
	if !strings.Contains(asked, "reserve AIM301") {
		t.Fatalf("dialog should list the reservations: %q", asked)
	}
	sched, _ := a.Sim("reinvent2026").GetSchedule(context.Background(), "reinvent2026")
	if len(sched.Reserved) != 0 {
		t.Fatal("declined apply must change nothing")
	}

	// The model cannot bypass the dialog by claiming the user confirmed.
	approve = true
	a.Sim("reinvent2026").Full["AIM301"] = true
	var out mcpserver.ApplyOut
	call(t, cs, "apply", map[string]any{"plan_id": p.PlanID}, &out)
	if out.ConfirmedVia != "dialog" || !contains(out.Reserved, "AIM301-R") {
		t.Fatalf("apply via dialog with fallback: %+v", out)
	}
}

func TestPromptIsAvailable(t *testing.T) {
	cs, _ := connect(t, nil)
	res, err := cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "plan_my_week", Arguments: map[string]string{"goals": "serverless and agents"}})
	if err != nil || !strings.Contains(res.Messages[0].Content.(*mcp.TextContent).Text, "serverless and agents") {
		t.Fatalf("prompt: %v %+v", err, res)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

var _ api.ScheduleService = nil

func TestSameSlotAlternatives(t *testing.T) {
	cat := tu.Catalog(
		tu.S{Code: "A1", Room: "Wynn", Clock: "09:00"},
		tu.S{Code: "B1", Room: "Encore", Clock: "09:15"},
		tu.S{Code: "C1", Room: "Wynn", Clock: "13:00"},
	)
	for _, s := range cat.Sessions { // give everything one shared topic
		s.Topics = []string{"Serverless"}
	}
	a, _ := cat.Lookup("A1")
	alt := cat.Alternatives(a, 5)
	if len(alt) != 1 || alt[0].Code != "B1" {
		t.Fatalf("alternatives = %v", alt)
	}
}
