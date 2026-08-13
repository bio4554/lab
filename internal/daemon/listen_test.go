package daemon

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestListenRetriesUntilFree: with the port occupied, Listen keeps
// retrying and succeeds once the occupant releases it.
func TestListenRetriesUntilFree(t *testing.T) {
	occupant, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := occupant.Addr().String()

	release := time.AfterFunc(700*time.Millisecond, func() { occupant.Close() })
	defer release.Stop()

	start := time.Now()
	ln, err := Listen(context.Background(), addr, 5*time.Second, discardLogger())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Errorf("Listen returned after %s; want it to have waited for the release", elapsed)
	}
}

// TestListenBoundedFailure: the retry window is bounded — a port that
// never frees up fails with "address in use" context, not a hang.
func TestListenBoundedFailure(t *testing.T) {
	occupant, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupant.Close()

	start := time.Now()
	_, err = Listen(context.Background(), occupant.Addr().String(), 1200*time.Millisecond, discardLogger())
	if err == nil {
		t.Fatal("Listen succeeded on a permanently occupied port")
	}
	if !strings.Contains(err.Error(), "still in use") {
		t.Errorf("error = %v, want bounded-retry context", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Listen took %s; want bounded by the window", elapsed)
	}
}

// TestListenImmediateSuccess: a free port binds on the first try.
func TestListenImmediateSuccess(t *testing.T) {
	ln, err := Listen(context.Background(), "127.0.0.1:0", time.Second, discardLogger())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ln.Close()
}
