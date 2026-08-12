package streamjson

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxLineBytes is the largest stream line the decoder accepts. Tool
// results can run to megabytes, so the limit is deliberately generous.
const MaxLineBytes = 32 << 20 // 32MB

// ErrLineTooLong is returned (wrapped) when a line exceeds
// MaxLineBytes. The stream cannot be advanced past it; the error is
// terminal.
var ErrLineTooLong = errors.New("streamjson: line exceeds 32MB limit")

// MalformedLineError is returned by Decoder.Next for a line that is
// not a JSON object. The offending line is carried verbatim so the
// caller can log or persist it; the decoder itself has advanced past
// the line, so the caller may keep reading.
type MalformedLineError struct {
	Line []byte
	Err  error
}

func (e *MalformedLineError) Error() string {
	return fmt.Sprintf("streamjson: malformed line: %v", e.Err)
}

func (e *MalformedLineError) Unwrap() error { return e.Err }

// Decoder reads line-delimited stream-json events.
type Decoder struct {
	sc *bufio.Scanner
}

// NewDecoder returns a Decoder reading from r.
func NewDecoder(r io.Reader) *Decoder {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), MaxLineBytes)
	return &Decoder{sc: sc}
}

// Next returns the next event. It returns io.EOF at end of stream, a
// *MalformedLineError for a non-JSON line (recoverable — Next may be
// called again), and ErrLineTooLong (wrapped) if a line exceeds
// MaxLineBytes (terminal). Blank lines are skipped.
func (d *Decoder) Next() (Event, error) {
	for d.sc.Scan() {
		line := d.sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		raw := append(json.RawMessage(nil), line...)
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return Event{}, &MalformedLineError{Line: raw, Err: err}
		}
		return Event{Kind: env.Type, Subtype: env.Subtype, Raw: raw}, nil
	}
	if err := d.sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return Event{}, fmt.Errorf("%w: %w", ErrLineTooLong, err)
		}
		return Event{}, err
	}
	return Event{}, io.EOF
}
