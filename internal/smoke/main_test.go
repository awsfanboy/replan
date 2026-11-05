package smoke

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Run with: REPLAN_SMOKE=1 go test ./internal/smoke -v
func TestStdioSmoke(t *testing.T) {
	if os.Getenv("REPLAN_SMOKE") == "" {
		t.Skip("set REPLAN_SMOKE=1")
	}
	ctx := context.Background()
	cmd := exec.Command("../../bin/replan", "mcp", "-f", "/tmp/replan-smoke/reinvent.yaml", "--sim", "--sim-open-seats", "--sim-full", "COM317-R1")
	c := mcp.NewClient(&mcp.Implementation{Name: "smoke", Version: "1"}, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, r *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			fmt.Println("---- DIALOG SHOWN TO HUMAN ----\n" + r.Params.Message + "\n-------------------------------")
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}, nil
		}})
	cs, err := c.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	show := func(name string, args map[string]any) {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("==== %s (error=%v)\n", name, res.IsError)
		for _, ct := range res.Content {
			if tc, ok := ct.(*mcp.TextContent); ok {
				s := tc.Text
				if len(s) > 1800 {
					s = s[:1800] + "…"
				}
				fmt.Println(s)
			}
		}
	}
	show("search_sessions", map[string]any{"event": "reinvent2026", "query": "agents", "kinds": []string{"code-talk", "chalk-talk"}, "limit": 3})
	show("edit_plan_file", map[string]any{"event": "reinvent2026", "add": []map[string]any{
		{"reserve": "COM317-R1", "fallbacks": []string{"COM317-R"}}, {"reserve": "ARC202-R2", "fallbacks": []string{"ARC202-R"}}},
		"add_blocks": []map[string]any{{"title": "Lunch", "day": "2026-12-02", "from": "12:00", "to": "13:00"}}})
	res, _ := cs.CallTool(ctx, &mcp.CallToolParams{Name: "plan", Arguments: map[string]any{}})
	b, _ := json.Marshal(res.StructuredContent)
	var p struct {
		PlanID string `json:"plan_id"`
	}
	json.Unmarshal(b, &p)
	for _, ct := range res.Content {
		fmt.Println(ct.(*mcp.TextContent).Text)
	}
	show("apply", map[string]any{"plan_id": p.PlanID})
	show("plan", map[string]any{})
}
