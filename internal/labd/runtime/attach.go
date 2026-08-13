package runtime

import (
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// Attach attaches to a container's stdio and returns a write end for
// stdin plus demultiplexed stdout/stderr readers (agent containers run
// without a TTY, so the raw stream is stdcopy-multiplexed).
//
// Lifetimes:
//   - Closing stdin half-closes the connection: the container sees EOF
//     on its stdin while stdout/stderr keep flowing.
//   - stdout/stderr reach EOF when the container's stream ends.
//   - Cancelling ctx tears the whole attachment down; pending reads
//     return the cancellation error.
//   - Both stdout and stderr must be drained concurrently (as with
//     exec.Cmd pipes): the demultiplexer stalls if one stream's frames
//     arrive while the other is unread.
func (r *Runtime) Attach(ctx context.Context, containerID string) (stdin io.WriteCloser, stdout, stderr io.Reader, err error) {
	resp, err := r.cli.ContainerAttach(ctx, containerID, container.AttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("runtime: attach %s: %w", containerID, err)
	}

	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	done := make(chan struct{})

	// Demultiplex until the stream ends, then propagate the terminal
	// condition (io.EOF on clean end) to both pipes.
	go func() {
		defer close(done)
		_, copyErr := stdcopy.StdCopy(outW, errW, resp.Reader)
		if copyErr == nil {
			copyErr = io.EOF
		} else if ctx.Err() != nil {
			// The read failed because we tore the connection down.
			copyErr = ctx.Err()
		}
		outW.CloseWithError(copyErr)
		errW.CloseWithError(copyErr)
	}()

	// Detach on context cancel; exit quietly when the stream ends on
	// its own. On cancel the pipe writers are closed directly as well:
	// StdCopy may be blocked writing into a pipe no one is reading,
	// and closing the connection alone would never unblock it.
	go func() {
		select {
		case <-ctx.Done():
			resp.Close()
			outW.CloseWithError(ctx.Err())
			errW.CloseWithError(ctx.Err())
		case <-done:
			resp.Close()
		}
	}()

	return &attachStdin{resp: resp}, outR, errR, nil
}

// attachStdin writes to the hijacked connection; Close half-closes the
// write side only, so demultiplexed reads continue past stdin EOF.
type attachStdin struct {
	resp types.HijackedResponse
}

func (w *attachStdin) Write(p []byte) (int, error) { return w.resp.Conn.Write(p) }

func (w *attachStdin) Close() error { return w.resp.CloseWrite() }
