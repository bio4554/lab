package streamjson

import (
	"encoding/json"
	"io"
)

// Encoder writes input lines for the CLI's stdin
// (--input-format stream-json).
type Encoder struct {
	w io.Writer
}

// NewEncoder returns an Encoder writing to w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w}
}

// UserMessage emits one user-message input line:
//
//	{"type":"user","message":{"role":"user","content":[{"type":"text","text":...}]}}
//
// The line and its trailing newline are written in a single Write call
// so the process on the other end of a pipe sees a complete line.
func (e *Encoder) UserMessage(text string) error {
	type textBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type message struct {
		Role    string      `json:"role"`
		Content []textBlock `json:"content"`
	}
	type line struct {
		Type    string  `json:"type"`
		Message message `json:"message"`
	}
	buf, err := json.Marshal(line{
		Type: "user",
		Message: message{
			Role:    "user",
			Content: []textBlock{{Type: "text", Text: text}},
		},
	})
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	_, err = e.w.Write(buf)
	return err
}

// RawLine writes an already-encoded event line verbatim, appending a
// newline. This is the re-encode half of the lossless guarantee:
// writing Event.Raw reproduces the original stream byte-for-byte.
func (e *Encoder) RawLine(raw json.RawMessage) error {
	buf := make([]byte, 0, len(raw)+1)
	buf = append(buf, raw...)
	buf = append(buf, '\n')
	_, err := e.w.Write(buf)
	return err
}
