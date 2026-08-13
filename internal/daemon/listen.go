package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"syscall"
	"time"
)

// BindRetryWindow is how long Listen keeps retrying a busy address
// before giving up. Sized for the daemon-restart race: a replacement
// daemon started while its predecessor is still draining (StopAll's
// grace can hold the ports for many seconds) should win the ports, not
// exit with "address already in use".
const BindRetryWindow = 10 * time.Second

// bindRetryInterval is the pause between bind attempts.
const bindRetryInterval = 500 * time.Millisecond

// Listen binds a TCP listener on addr, retrying for up to window (0 ⇒
// BindRetryWindow) while the address is busy. Retries are logged once
// per attempt at INFO; any error other than "address in use" fails
// immediately.
func Listen(ctx context.Context, addr string, window time.Duration, log *slog.Logger) (net.Listener, error) {
	if window <= 0 {
		window = BindRetryWindow
	}
	if log == nil {
		log = slog.Default()
	}
	deadline := time.Now().Add(window)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, fmt.Errorf("listen %s: %w", addr, err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("listen %s: address still in use after %s: %w", addr, window, err)
		}
		log.Info("address in use; retrying bind", "addr", addr, "remaining", remaining.Round(time.Second))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(bindRetryInterval):
		}
	}
}
