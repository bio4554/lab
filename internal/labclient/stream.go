package labclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bio4554/lab/internal/wire"
)

// StreamStatus reports the SSE subscription's connection state. One is
// sent on every transition (and on each failed retry, with Attempt
// incremented).
type StreamStatus struct {
	Connected bool
	// Err is the last connect/read error while disconnected.
	Err error
	// Attempt counts consecutive failed connects (0 once connected).
	Attempt int
	// RetryIn is the backoff delay before the next connect attempt.
	RetryIn time.Duration
	// LastEventID is the resume point the next connect will use, -1
	// when the stream will start live-only.
	LastEventID int64
}

// Stream is a live event subscription. Events carries every event
// exactly once, in id order; Status carries connection transitions
// (buffered — stale statuses are dropped rather than blocking).
// Both channels close after the context ends.
type Stream struct {
	Events <-chan wire.Event
	Status <-chan StreamStatus
}

// Reconnect backoff bounds.
const (
	streamBackoffMin = 500 * time.Millisecond
	streamBackoffMax = 10 * time.Second
)

// StreamEvents subscribes to GET /v1/events/stream. afterID is the
// resume point: events with id > afterID are delivered; pass a
// negative afterID to start live-only at the daemon's current head.
// The subscription reconnects with backoff until ctx ends, resuming
// via Last-Event-ID so no event is lost or repeated across daemon
// restarts.
func (c *Client) StreamEvents(ctx context.Context, afterID int64) *Stream {
	events := make(chan wire.Event, 64)
	status := make(chan StreamStatus, 16)
	s := &Stream{Events: events, Status: status}
	go c.streamLoop(ctx, afterID, events, status)
	return s
}

// sendStatus never blocks: when the buffer is full the oldest status
// is dropped — only the latest state matters to a UI.
func sendStatus(ch chan StreamStatus, st StreamStatus) {
	for {
		select {
		case ch <- st:
			return
		default:
			select {
			case <-ch:
			default:
			}
		}
	}
}

func (c *Client) streamLoop(ctx context.Context, lastID int64, events chan<- wire.Event, status chan StreamStatus) {
	defer close(events)
	defer close(status)

	backoff := streamBackoffMin
	attempt := 0
	for ctx.Err() == nil {
		err := c.streamOnce(ctx, &lastID, events, status, &attempt)
		if ctx.Err() != nil {
			return
		}
		if attempt == 0 {
			// The previous connect succeeded; restart the backoff.
			backoff = streamBackoffMin
		}
		attempt++
		sendStatus(status, StreamStatus{Err: err, Attempt: attempt, RetryIn: backoff, LastEventID: lastID})
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, streamBackoffMax)
	}
}

// streamOnce runs one connection until it fails or ctx ends. On a
// successful connect it resets *attempt and reports Connected.
func (c *Client) streamOnce(ctx context.Context, lastID *int64, events chan<- wire.Event, status chan StreamStatus, attempt *int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/events/stream", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if *lastID >= 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(*lastID, 10))
	}
	// Use a Timeout-free client: an SSE response never ends on its own.
	httpc := &http.Client{Transport: c.httpClient().Transport}
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("labd: event stream HTTP %d", resp.StatusCode)
	}

	*attempt = 0
	sendStatus(status, StreamStatus{Connected: true, LastEventID: *lastID})

	br := bufio.NewReader(resp.Body)
	var id int64 = -1
	var data []string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if len(data) > 0 {
				var ev wire.Event
				if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &ev); err != nil {
					return fmt.Errorf("labclient: decoding stream event: %w", err)
				}
				select {
				case events <- ev:
				case <-ctx.Done():
					return ctx.Err()
				}
				if id >= 0 {
					*lastID = id
				} else {
					*lastID = ev.ID
				}
			}
			id, data = -1, nil
		case strings.HasPrefix(line, ":"):
			// keepalive comment
		case strings.HasPrefix(line, "id:"):
			if n, err := strconv.ParseInt(strings.TrimSpace(line[3:]), 10, 64); err == nil {
				id = n
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
}
