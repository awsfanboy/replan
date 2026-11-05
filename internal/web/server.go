// Package web is replan's local browser UI: browse and filter the whole
// catalog, build the ranked plan, run plan, and apply after confirmation.
//
// It is just another MCP client: every change goes through the same replan
// tools an AI agent uses (edit_plan_file, plan, apply), so the safety rules
// are identical. The server listens on 127.0.0.1 only, checks the Host
// header (against DNS rebinding) and requires a per-run token on API calls
// (against other web pages posting to it).
package web

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/awsfanboy/replan/internal/app"
	"github.com/awsfanboy/replan/internal/mcpserver"
)

//go:embed index.html
var static embed.FS

// tools the UI may call. list_events/search are not needed: the UI loads
// the whole catalog once and filters locally.
var allowedTools = map[string]bool{
	"get_plan_file": true, "edit_plan_file": true, "plan": true, "apply": true, "get_agenda": true, "get_session": true,
}

type Server struct {
	App   *app.App
	Event string
	Token string

	cs   *mcp.ClientSession
	host string
}

// Start listens on addr (e.g. "127.0.0.1:0") and returns the URL to open.
func Start(ctx context.Context, a *app.App, event, addr string) (string, func(), error) {
	srv := mcpserver.New(a)
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		return "", nil, err
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "replan-ui", Version: mcpserver.Version}, nil).Connect(ctx, ct, nil)
	if err != nil {
		return "", nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", nil, err
	}
	b := make([]byte, 16)
	rand.Read(b)
	s := &Server{App: a, Event: event, Token: hex.EncodeToString(b), cs: cs, host: ln.Addr().String()}
	hs := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	go hs.Serve(ln)
	url := fmt.Sprintf("http://%s/?t=%s", s.host, s.Token)
	return url, func() { hs.Close(); cs.Close() }, nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /api/catalog", s.api(s.catalog))
	mux.HandleFunc("POST /api/tool/{name}", s.api(s.tool))
	return s.guard(mux)
}

// guard rejects requests whose Host isn't our loopback address (DNS rebinding).
func (s *Server) guard(next http.Handler) http.Handler {
	_, port, _ := net.SplitHostPort(s.host)
	ok := map[string]bool{s.host: true, "localhost:" + port: true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ok[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) api(h func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Replan-Token") != s.Token {
			http.Error(w, "missing or wrong token: reopen the URL replan printed", http.StatusForbidden)
			return
		}
		out, err := h(r)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(out)
	}
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	b, _ := static.ReadFile("index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; connect-src 'self'")
	w.Write(b)
}

type catalogRow struct {
	mcpserver.SessionOut
	ID       string   `json:"id"`
	DayKey   string   `json:"dayKey"`
	Sort     string   `json:"sort"`
	Abstract string   `json:"abstract,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

func (s *Server) catalog(r *http.Request) (any, error) {
	cat, err := s.App.Catalog(r.Context(), s.Event)
	if err != nil {
		return nil, err
	}
	rows := make([]catalogRow, 0, len(cat.Sessions))
	for _, ss := range cat.Sessions {
		row := catalogRow{SessionOut: mcpserver.SessionView(ss, cat.Location), ID: ss.SessionID, Abstract: ss.Abstract, DayKey: "unscheduled", Sort: "9999"}
		if ss.HasTime() {
			st := ss.Start.In(cat.Location)
			row.DayKey, row.Sort = st.Format("2006-01-02 Mon"), st.Format("2006-01-02T15:04")
		}
		for _, list := range [][]string{ss.Topics, ss.Services, ss.AreasOfInterest, ss.Tracks} {
			row.Tags = append(row.Tags, list...)
		}
		rows = append(rows, row)
	}
	return map[string]any{
		"event":    map[string]string{"id": cat.Event.EventID, "name": cat.Event.Name, "timezone": cat.Location.String()},
		"mode":     s.App.Mode(),
		"sim":      s.App.Cfg.Sim,
		"planFile": s.App.Cfg.SpecPath,
		"facets":   cat.Facets(cat.Sessions),
		"sessions": rows,
	}, nil
}

func (s *Server) tool(r *http.Request) (any, error) {
	name := r.PathValue("name")
	if !allowedTools[name] {
		return nil, fmt.Errorf("tool %q is not available from the UI", name)
	}
	var args map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&args); err != nil {
		args = map[string]any{}
	}
	if name == "edit_plan_file" || name == "get_session" || name == "get_agenda" {
		if _, ok := args["event"]; !ok {
			args["event"] = s.Event
		}
	}
	res, err := s.cs.CallTool(r.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, err
	}
	var text strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			text.WriteString(t.Text)
		}
	}
	return map[string]any{"isError": res.IsError, "text": text.String(), "data": res.StructuredContent}, nil
}
