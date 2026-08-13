package labclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/wire"
)

func TestNewNormalizesAddr(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1:7710":         "http://127.0.0.1:7710",
		"http://localhost:7710/": "http://localhost:7710",
	} {
		if got := New(addr).BaseURL; got != want {
			t.Errorf("New(%q).BaseURL = %q, want %q", addr, got, want)
		}
	}
}

func TestRequestsAndDecoding(t *testing.T) {
	agentID := uuid.New()
	mux := http.NewServeMux()
	seen := map[string]string{} // path → body
	record := func(r *http.Request) {
		body := make([]byte, r.ContentLength+1)
		n, _ := r.Body.Read(body)
		seen[r.Method+" "+r.URL.String()] = string(body[:n])
	}
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		json.NewEncoder(w).Encode(wire.DaemonStatus{Version: "v1", DBHealthy: true, Stacks: []string{"base", "go"}})
	})
	mux.HandleFunc("POST /v1/projects", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(wire.Project{Name: "p1", Stack: "go"})
	})
	mux.HandleFunc("POST /v1/projects/{p}/agents/{a}/turns", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(wire.Turn{AgentID: agentID, Status: "queued"})
	})
	mux.HandleFunc("POST /v1/projects/{p}/agents/{a}/start", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL)
	ctx := context.Background()

	st, err := c.Status(ctx)
	if err != nil || st.Version != "v1" || len(st.Stacks) != 2 {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	proj, err := c.CreateProject(ctx, wire.CreateProjectRequest{Name: "p1", Origin: "/x", Stack: "go"})
	if err != nil || proj.Name != "p1" {
		t.Fatalf("CreateProject = %+v, %v", proj, err)
	}
	if want := `{"name":"p1","origin":"/x","stack":"go"}`; seen["POST /v1/projects"] != want+"\n" && seen["POST /v1/projects"] != want {
		t.Errorf("project body = %q", seen["POST /v1/projects"])
	}
	turn, err := c.SubmitTurn(ctx, "p one", "a/gent", "hi")
	if err != nil || turn.Status != "queued" {
		t.Fatalf("SubmitTurn = %+v, %v", turn, err)
	}
	// Names are path-escaped.
	if _, ok := seen["POST /v1/projects/p%20one/agents/a%2Fgent/turns"]; !ok {
		t.Errorf("turn path not escaped; seen: %v", seen)
	}
	if err := c.StartAgent(ctx, "p1", "coder"); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
}

func TestAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(wire.Error{Error: "agent is retired"})
	}))
	defer srv.Close()
	err := New(srv.URL).StartAgent(context.Background(), "p", "a")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || apiErr.Message != "agent is retired" {
		t.Fatalf("err = %v", err)
	}
}

func TestAllSessionEventsPagination(t *testing.T) {
	sess := uuid.New()
	// 1000-event first page, then 500: two requests expected.
	var afterSeqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		afterSeqs = append(afterSeqs, r.URL.Query().Get("after_seq"))
		start := int64(1)
		if s := r.URL.Query().Get("after_seq"); s != "" {
			var n int64
			json.Unmarshal([]byte(s), &n)
			start = n + 1
		}
		count := int64(1000)
		if start > 1 {
			count = 500
		}
		events := make([]wire.Event, count)
		for i := range events {
			events[i] = wire.Event{ID: start + int64(i), SessionID: sess, Seq: start + int64(i), Kind: "assistant", Payload: []byte(`{}`)}
		}
		json.NewEncoder(w).Encode(events)
	}))
	defer srv.Close()

	events, err := New(srv.URL).AllSessionEvents(context.Background(), sess, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1500 || events[0].Seq != 1 || events[1499].Seq != 1500 {
		t.Fatalf("got %d events", len(events))
	}
	if len(afterSeqs) != 2 || afterSeqs[1] != "1000" {
		t.Fatalf("after_seq sequence = %v", afterSeqs)
	}
}

// sseServer serves scripted SSE connections: each connection gets the
// handler for its ordinal (0-based) and the request that opened it.
func sseServer(t *testing.T, handlers ...func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	var conn int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := conn
		conn++
		if i >= len(handlers) {
			// Hold the connection open until the client goes away.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		handlers[i](w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeSSE(w http.ResponseWriter, id int64, payload string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "id: %d\ndata: %s\n\n", id, payload)
	w.(http.Flusher).Flush()
}

func event(id int64) string {
	b, _ := json.Marshal(wire.Event{ID: id, Kind: "assistant", Payload: []byte(`{}`), SessionID: uuid.Nil})
	return string(b)
}

func TestStreamResumeAcrossReconnect(t *testing.T) {
	var firstLastEventID, secondLastEventID string
	srv := sseServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			firstLastEventID = r.Header.Get("Last-Event-ID")
			writeSSE(w, 1, event(1))
			writeSSE(w, 2, event(2))
			// Connection drops (handler returns).
		},
		func(w http.ResponseWriter, r *http.Request) {
			secondLastEventID = r.Header.Get("Last-Event-ID")
			writeSSE(w, 3, event(3))
			<-r.Context().Done()
		},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stream := New(srv.URL).StreamEvents(ctx, 0)

	var got []int64
	for len(got) < 3 {
		select {
		case ev, ok := <-stream.Events:
			if !ok {
				t.Fatalf("stream closed early; got %v", got)
			}
			got = append(got, ev.ID)
		case <-ctx.Done():
			t.Fatalf("timed out; got %v", got)
		}
	}
	if got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("events = %v, want [1 2 3]", got)
	}
	if firstLastEventID != "0" {
		t.Errorf("first connect Last-Event-ID = %q, want \"0\"", firstLastEventID)
	}
	if secondLastEventID != "2" {
		t.Errorf("reconnect Last-Event-ID = %q, want \"2\"", secondLastEventID)
	}

	cancel()
	// Channels close once the context ends.
	for range stream.Events {
	}
	for range stream.Status {
	}
}

func TestStreamLiveOnlyOmitsHeader(t *testing.T) {
	headerSet := make(chan bool, 1)
	srv := sseServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, ok := r.Header[http.CanonicalHeaderKey("Last-Event-ID")]
		headerSet <- ok
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := New(srv.URL).StreamEvents(ctx, -1)
	select {
	case ok := <-headerSet:
		if ok {
			t.Fatal("live-only stream sent Last-Event-ID")
		}
	case <-ctx.Done():
		t.Fatal("no connection")
	}
	// Status should report connected.
	for st := range stream.Status {
		if st.Connected {
			return
		}
	}
	t.Fatal("never reported connected")
}

func TestStreamReportsDisconnect(t *testing.T) {
	srv := sseServer(t) // zero scripted handlers: all connections hold
	srv.Close()         // immediately unreachable
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := New(srv.URL).StreamEvents(ctx, -1)
	for st := range stream.Status {
		if !st.Connected && st.Err != nil && st.Attempt >= 1 && st.RetryIn > 0 {
			cancel()
			return
		}
	}
	t.Fatal("no disconnected status")
}
