package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient(nil)
	c.BaseURL = srv.URL
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func TestThrottleWaitsAndRetries(t *testing.T) {
	calls := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
			w.Write([]byte(`{"message":"slow down"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"successful": []string{"a"}, "failed": []any{}}})
	})
	res, err := c.ReserveSessions(context.Background(), "e", []string{"a"})
	if err != nil || calls != 2 || len(res.Successful) != 1 {
		t.Fatalf("calls=%d res=%+v err=%v", calls, res, err)
	}
}

func TestBulk200WithFailuresAndChunking(t *testing.T) {
	var sizes []int
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct{ SessionIDs []string }
		json.NewDecoder(r.Body).Decode(&body)
		sizes = append(sizes, len(body.SessionIDs))
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{
			"successful": body.SessionIDs[1:],
			"failed":     []map[string]any{{"sessionId": body.SessionIDs[0], "code": "sessionFull"}},
		}})
	})
	ids := make([]string, 12)
	for i := range ids {
		ids[i] = string(rune('a' + i))
	}
	res, err := c.ReserveSessions(context.Background(), "e", ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 2 || sizes[0] != 10 || sizes[1] != 2 {
		t.Errorf("chunks = %v, want [10 2]", sizes)
	}
	if len(res.Failed) != 2 || len(res.Successful) != 10 {
		t.Errorf("res = %+v", res)
	}
}

func TestWriteServerErrorIsUnknownOutcomeAndNotRetried(t *testing.T) {
	calls := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(503)
	})
	err := c.CreatePersonalTime(context.Background(), "e", PersonalTimeInput{})
	if !IsUnknownOutcome(err) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestReadServerErrorIsRetried(t *testing.T) {
	calls := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(`{"schedule":{"reserved":[],"favorites":[],"personalTime":[]}}`))
	})
	if _, err := c.GetSchedule(context.Background(), "e"); err != nil || calls != 3 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func Test403BodyDecidesKind(t *testing.T) {
	body := `{"message":"not registered"}`
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(body))
	})
	c.ReadRetries = 0
	if _, err := c.GetSchedule(context.Background(), "e"); !IsKind(err, KindNotRegistered) {
		t.Errorf("JSON 403: %v", err)
	}
	body = ""
	if _, err := c.GetSchedule(context.Background(), "e"); !IsKind(err, KindEdgeRefused) {
		t.Errorf("empty 403: %v", err)
	}
	body = "<html>denied</html>"
	if _, err := c.GetSchedule(context.Background(), "e"); !IsKind(err, KindEdgeRefused) {
		t.Errorf("HTML 403: %v", err)
	}
}

func TestCancel404IsSuccessAnd409IsClosed(t *testing.T) {
	status := 404
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
	if err := c.CancelReservation(context.Background(), "e", "s"); err != nil {
		t.Errorf("404 cancel: %v", err)
	}
	status = 409
	if _, err := c.ReserveSessions(context.Background(), "e", []string{"s"}); !IsKind(err, KindOperationClosed) {
		t.Errorf("409: %v", err)
	}
}

func TestPaginationFollowsNextTokenThroughShortPages(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("nextToken") {
		case "":
			w.Write([]byte(`{"items":[{"sessionId":"1"}],"totalCount":3,"nextToken":"p2"}`))
		case "p2":
			w.Write([]byte(`{"items":[],"totalCount":3,"nextToken":"p3"}`)) // short page, not the end
		case "p3":
			w.Write([]byte(`{"items":[{"sessionId":"2"},{"sessionId":"3"}],"totalCount":3}`))
		}
	})
	ss, err := c.ListSessions(context.Background(), "e", "", false)
	if err != nil || len(ss) != 3 {
		t.Fatalf("got %d sessions, err %v", len(ss), err)
	}
}
