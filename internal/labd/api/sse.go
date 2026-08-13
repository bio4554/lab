package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// sseKeepalive is the cadence of comment frames that keep proxies and
// dead-connection detection honest.
const sseKeepalive = 15 * time.Second

// eventStream is GET /v1/events/stream: the global live event feed as
// Server-Sent Events. Each frame's `id:` is the event id and `data:`
// is the wire.Event JSON.
//
// Resume: a reconnecting client sends `Last-Event-ID` (standard
// EventSource behavior) or `?after_id=N`; the header wins. The stream
// then backfills everything after that id from the store before going
// live, so a disconnect loses nothing and repeats nothing. With
// neither, the stream starts at the current head (live-only).
//
// Mechanics: the handler never trusts notifications for data — a hub
// wake only means "check the store", and each check reads rows after
// the last sent id. Gapless and duplicate-free by construction.
func (s *ClientServer) eventStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(s.Log, w, http.StatusInternalServerError, errors.New("response writer does not support streaming"))
		return
	}
	ctx := r.Context()

	last, err := queryInt(r, "after_id", -1)
	if err != nil {
		writeError(s.Log, w, http.StatusBadRequest, err)
		return
	}
	if h := r.Header.Get("Last-Event-ID"); h != "" {
		n, err := strconv.ParseInt(h, 10, 64)
		if err != nil {
			writeError(s.Log, w, http.StatusBadRequest, fmt.Errorf("invalid Last-Event-ID: %q", h))
			return
		}
		last = n
	}
	if last < 0 { // no resume point: live-only from the current head
		head, err := s.Store.MaxEventID(ctx)
		if err != nil {
			writeError(s.Log, w, http.StatusInternalServerError, err)
			return
		}
		last = head
	}

	// Subscribe before the first read so nothing appended in between
	// is missed.
	wake, cancel := s.Hub.Subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	for {
		// Drain everything currently after `last`, batch by batch.
		for {
			events, err := s.Store.EventsSinceID(ctx, last, 500)
			if err != nil {
				if ctx.Err() == nil {
					s.Log.Warn("event stream read failed", "error", err)
				}
				return
			}
			if len(events) == 0 {
				break
			}
			for _, e := range events {
				data, err := json.Marshal(toWireEvent(e))
				if err != nil {
					s.Log.Error("marshaling event", "event", e.ID, "error", err)
					return
				}
				if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.ID, data); err != nil {
					return // client went away
				}
				last = e.ID
			}
			flusher.Flush()
		}

		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
