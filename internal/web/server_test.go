package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awsfanboy/replan/internal/app"
	tu "github.com/awsfanboy/replan/internal/testutil"
)

func start(t *testing.T) (base, token string) {
	t.Helper()
	a := app.New(app.Config{SpecPath: filepath.Join(t.TempDir(), "reinvent.yaml"), Sim: true})
	a.SetCatalog(tu.Catalog(
		tu.S{Code: "AIM301", Room: "Wynn | Lafite 1", Clock: "09:00"},
		tu.S{Code: "DEV402", Type: "Chalk talk", Room: "Caesars Forum | Academy 410", Clock: "11:00"},
	))
	u, stop, err := Start(context.Background(), a, "reinvent2026", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	p, _ := url.Parse(u)
	return "http://" + p.Host, p.Query().Get("t")
}

func call(t *testing.T, method, u, token string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, u, r)
	if token != "" {
		req.Header.Set("X-Replan-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestTokenAndHostAreRequired(t *testing.T) {
	base, token := start(t)
	if code, _ := call(t, "GET", base+"/api/catalog", "", nil); code != http.StatusForbidden {
		t.Fatalf("no token: %d", code)
	}
	req, _ := http.NewRequest("GET", base+"/api/catalog", nil)
	req.Header.Set("X-Replan-Token", token)
	req.Host = "evil.example:80" // DNS rebinding
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host: %d", resp.StatusCode)
	}
	if code, _ := call(t, "POST", base+"/api/tool/list_events", token, map[string]any{}); code == http.StatusOK {
		t.Fatal("tools outside the allow-list must be refused")
	}
}

func TestBrowseEditPlanApply(t *testing.T) {
	base, token := start(t)
	code, cat := call(t, "GET", base+"/api/catalog", token, nil)
	if code != 200 || len(cat["sessions"].([]any)) != 2 || cat["sim"] != true {
		t.Fatalf("catalog: %d %v", code, cat)
	}
	page, _ := http.Get(base + "/")
	b, _ := io.ReadAll(page.Body)
	if !strings.Contains(string(b), "replan") {
		t.Fatal("index page not served")
	}

	_, r := call(t, "POST", base+"/api/tool/edit_plan_file", token, map[string]any{"add": []map[string]any{{"reserve": "AIM301"}, {"reserve": "DEV402"}}})
	if r["isError"] == true {
		t.Fatalf("edit: %v", r["text"])
	}
	_, r = call(t, "POST", base+"/api/tool/plan", token, map[string]any{})
	id := r["data"].(map[string]any)["plan_id"].(string)

	_, r = call(t, "POST", base+"/api/tool/apply", token, map[string]any{"plan_id": id})
	if r["isError"] != true {
		t.Fatal("apply without confirmation must be refused")
	}
	_, r = call(t, "POST", base+"/api/tool/apply", token, map[string]any{"plan_id": id, "user_confirmed": true})
	data := r["data"].(map[string]any)
	if r["isError"] == true || len(data["reserved"].([]any)) != 2 {
		t.Fatalf("apply: %v", r["text"])
	}
}
