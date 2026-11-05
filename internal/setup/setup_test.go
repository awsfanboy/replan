package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKiroAgentNeverAutoApprovesApply(t *testing.T) {
	a := KiroAgent(Options{Binary: "/b/replan", SpecPath: "/p/reinvent.yaml", Sim: true, SimFull: "COM317-R1", Name: "replan"})
	allowed := strings.Join(a["allowedTools"].([]string), ",")
	if strings.Contains(allowed, "apply") {
		t.Fatalf("apply must never be auto-approved: %s", allowed)
	}
	if !strings.Contains(allowed, "@replan/plan") || !strings.Contains(allowed, "@replan/search_sessions") {
		t.Fatalf("read-only tools should be allowed: %s", allowed)
	}
	srv := a["mcpServers"].(map[string]any)["replan"].(map[string]any)
	if got := strings.Join(srv["args"].([]string), " "); got != "mcp -f /p/reinvent.yaml --sim --sim-full COM317-R1" {
		t.Fatalf("args = %q", got)
	}
	rules := a["permissions"].(map[string]any)["rules"].([]map[string]any)
	if rules[1]["effect"] != "ask" || rules[1]["match"].([]string)[0] != "replan/apply" {
		t.Fatalf("apply must be 'ask' in the Kiro 3 permissions: %+v", rules)
	}
	for _, m := range rules[0]["match"].([]string) {
		if strings.Contains(m, "apply") {
			t.Fatalf("apply must never be allowed silently: %v", m)
		}
	}
	if a["includeMcpJson"] != false {
		t.Fatal("the agent should only see replan's tools")
	}
}

func TestWriteKiroAgentDoesNotClobberForeignFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replan.json")
	os.WriteFile(path, []byte(`{"description":"someone else's agent"}`), 0o644)
	o := Options{Binary: "/b/replan", SpecPath: "/p/r.yaml", Name: "replan"}
	if err := WriteKiroAgent(path, o, false); err == nil {
		t.Fatal("should refuse to overwrite a file replan did not write")
	}
	if err := WriteKiroAgent(path, o, true); err != nil {
		t.Fatal(err)
	}
	if err := WriteKiroAgent(path, o, false); err != nil {
		t.Fatalf("re-running setup over its own file should work: %v", err)
	}
}

func TestClaudeCommandQuotes(t *testing.T) {
	got := ClaudeCommand(Options{Binary: "/Users/me/my tools/replan", SpecPath: "/p/reinvent.yaml", Name: "replan"})
	want := "claude mcp add replan -- '/Users/me/my tools/replan' mcp -f /p/reinvent.yaml"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
