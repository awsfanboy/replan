// Package setup writes client configuration so AI assistants can use replan:
// a Kiro CLI custom agent, and the one-line command for Claude Code.
package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Options describe how the assistant should launch `replan mcp`.
type Options struct {
	Binary   string // absolute path of the replan binary
	SpecPath string // absolute path of reinvent.yaml
	Sim      bool
	SimFull  string // comma-separated codes the simulator treats as full
	Name     string // agent / server name
	// Demo options passed through to `replan mcp`.
	Catalog, Compare, Now string
	SimFullRate           string // simulator: fraction of sessions randomly full
	SimOpen               bool   // simulator: ignore real seat bands
}

func (o Options) Args() []string {
	args := []string{"mcp", "-f", o.SpecPath}
	if o.Sim {
		args = append(args, "--sim")
		if o.SimFull != "" {
			args = append(args, "--sim-full", o.SimFull)
		}
		if o.SimFullRate != "" {
			args = append(args, "--sim-full-rate", o.SimFullRate)
		}
		if o.SimOpen {
			args = append(args, "--sim-open-seats")
		}
	}
	for _, kv := range [][2]string{{"--catalog", o.Catalog}, {"--compare", o.Compare}, {"--now", o.Now}} {
		if kv[1] != "" {
			args = append(args, kv[0], kv[1])
		}
	}
	return args
}

// ReadOnlyTools are safe to run without asking. edit_plan_file only touches
// the local plan file (with a .bak), so it is trusted too. apply never is.
var ReadOnlyTools = []string{"list_events", "search_sessions", "get_session", "get_plan_file", "edit_plan_file", "plan", "get_agenda",
	"whats_next", "tonight_check", "what_changed"}

const kiroPrompt = `You help the user plan their AWS re:Invent week with the replan tools.

How to work:
- Find sessions with search_sessions and get_session. When the user wants in-person value, prefer
  sessions that are not recorded (chalk talks, workshops, builders' sessions, code talks).
- Build the plan with edit_plan_file: rank the most important first and give EVERY entry at
  least one fallback: a repeat, or one of get_session's same_slot_alternatives. Seats run out;
  a plan without fallbacks loses sessions. Add blocks for meals and meetings the user mentions.
- Call plan and walk the user through it: what gets reserved, what was skipped and why, and the
  walking blocks that will be written into their official event app.
- Never call apply unless the user explicitly says yes to that plan. Pass the plan_id from plan,
  and user_confirmed=true only after that explicit yes. Kiro will also ask the user to approve
  the apply call itself.
- Report exactly what apply returns; it is verified against the live schedule.
- During the event: "I'm late / stuck somewhere / what's next" → whats_next; in the evening →
  tonight_check; "anything changed?" → what_changed. When the user can't use a reservation,
  offer to give the seat back (edit_plan_file skip, then plan, then apply after a yes) so someone
  in the walk-up line gets it, and suggest a reachable alternative.
- Keep answers short and scannable: a small table or list per day.`

// KiroAgent returns a Kiro CLI custom agent config.
func KiroAgent(o Options) map[string]any {
	allowed := make([]string, len(ReadOnlyTools))
	match := make([]string, len(ReadOnlyTools))
	for i, t := range ReadOnlyTools {
		allowed[i] = "@" + o.Name + "/" + t
		match[i] = o.Name + "/" + t
	}
	mode := "your real schedule"
	if o.Sim {
		mode = "a local simulator (nothing real changes)"
	}
	return map[string]any{
		"name":        o.Name,
		"description": "Plan your re:Invent week: the agent plans, you approve, replan books the seats safely.",
		"prompt":      kiroPrompt,
		"mcpServers": map[string]any{
			o.Name: map[string]any{"command": o.Binary, "args": o.Args(), "timeout": 120000},
		},
		"tools": []string{"@" + o.Name},
		// Kiro CLI 3 (KAS) reads permissions and skips profiles that only
		// have allowedTools; Kiro CLI 2.x reads allowedTools. Write both.
		"allowedTools": allowed,
		"permissions": map[string]any{"rules": []map[string]any{
			{"capability": "mcp", "match": match, "effect": "allow"},
			{"capability": "mcp", "match": []string{o.Name + "/apply"}, "effect": "ask"},
		}},
		"includeMcpJson": false,
		"welcomeMessage": fmt.Sprintf("replan is connected to %s. Tell me what you want from your week, e.g. \"hands-on serverless and agents on Tuesday, lunch free, max 4 a day\".", mode),
	}
}

// KiroAgentPath is where Kiro CLI discovers agents: ~/.kiro/agents (global)
// or ./.kiro/agents (workspace, only when kiro-cli runs in that directory).
func KiroAgentPath(name string, workspace bool) (string, error) {
	if workspace {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return filepath.Join(wd, ".kiro", "agents", name+".json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kiro", "agents", name+".json"), nil
}

// WriteKiroAgent writes the agent file. It refuses to overwrite a file that
// replan did not write, unless force is set.
func WriteKiroAgent(path string, o Options, force bool) error {
	if b, err := os.ReadFile(path); err == nil && !force {
		var existing map[string]any
		if json.Unmarshal(b, &existing) != nil || !strings.Contains(fmt.Sprint(existing["description"]), "replan") {
			return fmt.Errorf("%s exists and was not written by replan; use --force to replace it", path)
		}
	}
	b, err := json.MarshalIndent(KiroAgent(o), "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// ClaudeCommand is the one-liner that registers replan with Claude Code.
func ClaudeCommand(o Options) string {
	parts := []string{"claude", "mcp", "add", o.Name, "--", shellQuote(o.Binary)}
	for _, a := range o.Args() {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " '\"$`\\!*?&;|<>()[]{}#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
