package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/labd/store"
	"github.com/bio4554/lab/internal/wire"
)

// sseClient consumes one /v1/events/stream connection, forwarding the
// ids of events belonging to one session. Filtering matters: the
// stream is global and other packages' tests may be appending events
// to the same dev database concurrently.
type sseClient struct {
	cancel context.CancelFunc
	ids    chan int64
	t      *testing.T
}

func openStream(t *testing.T, url string, lastEventID int64, sessionID uuid.UUID) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	// Always close the connection at test end, even on t.Fatal paths:
	// a leaked SSE connection blocks httptest.Server.Close forever.
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if lastEventID >= 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(lastEventID, 10))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("stream content-type = %q", ct)
	}
	c := &sseClient{cancel: cancel, ids: make(chan int64, 100), t: t}
	go func() {
		defer resp.Body.Close()
		defer close(c.ids)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			data, ok := strings.CutPrefix(sc.Text(), "data: ")
			if !ok {
				continue
			}
			var ev wire.Event
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				return
			}
			if ev.SessionID != sessionID {
				continue
			}
			select {
			case c.ids <- ev.ID:
			case <-ctx.Done():
				return
			}
		}
	}()
	return c
}

// next waits for the next matching event id from the stream.
func (c *sseClient) next() (int64, bool) {
	select {
	case id, ok := <-c.ids:
		return id, ok
	case <-time.After(5 * time.Second):
		c.t.Fatal("timed out waiting for SSE event")
		return 0, false
	}
}

// assertQuiet asserts no matching event arrives within the window.
func (c *sseClient) assertQuiet(d time.Duration) {
	select {
	case id, ok := <-c.ids:
		if ok {
			c.t.Fatalf("unexpected SSE event %d", id)
		}
	case <-time.After(d):
	}
}

func TestEventStreamLiveAndResume(t *testing.T) {
	srv, st, pool := newClientServer(t)
	f := createFixture(t, st, pool)
	ctx := context.Background()

	sess, err := st.CreateSession(ctx, f.Agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	append1 := func() store.Event {
		ev, err := st.AppendEvent(ctx, sess.ID, f.Agent.ID, nil, "assistant", []byte(`{"type":"assistant"}`))
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}

	// Live: subscribe at the current head, then append; events arrive
	// in order.
	head, err := st.MaxEventID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c1 := openStream(t, srv.URL+"/v1/events/stream?after_id="+itoa(head), -1, sess.ID)
	var want []int64
	for range 3 {
		want = append(want, append1().ID)
	}
	for _, w := range want {
		got, ok := c1.next()
		if !ok || got != w {
			t.Fatalf("live event = %d (ok=%v), want %d", got, ok, w)
		}
	}

	// Disconnect, append while away, reconnect with Last-Event-ID:
	// the gap is backfilled exactly once, then live continues.
	last := want[len(want)-1]
	c1.cancel()
	gap1, gap2 := append1().ID, append1().ID

	c2 := openStream(t, srv.URL+"/v1/events/stream", last, sess.ID)
	for _, w := range []int64{gap1, gap2} {
		got, ok := c2.next()
		if !ok || got != w {
			t.Fatalf("resumed event = %d (ok=%v), want %d", got, ok, w)
		}
	}
	live := append1().ID
	if got, ok := c2.next(); !ok || got != live {
		t.Fatalf("post-resume live event = %d (ok=%v), want %d", got, ok, live)
	}
	c2.assertQuiet(300 * time.Millisecond) // no duplicates trailing
}
