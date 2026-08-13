// Package streamjson implements the Claude Code CLI stream-json wire
// format (`claude -p --input-format stream-json --output-format
// stream-json --verbose`): a decoder for the line-delimited event
// stream on stdout and an encoder for input lines on stdin.
//
// The package is lossless by design: every decoded Event retains its
// raw line verbatim, so unknown event kinds and unknown fields inside
// known kinds survive decode → re-encode byte-for-byte.
package streamjson

import (
	"encoding/json"
	"fmt"
	"time"
)

// Known event kinds (the top-level "type" field).
const (
	KindSystem         = "system"
	KindAssistant      = "assistant"
	KindUser           = "user"
	KindResult         = "result"
	KindRateLimitEvent = "rate_limit_event"
)

// Known subtypes.
const (
	SubtypeInit    = "init"
	SubtypeSuccess = "success"
)

// Event is one line of the output stream. Kind and Subtype are always
// populated from the envelope (empty string when absent); Raw is the
// original line verbatim, with no trailing newline. All typed
// accessors parse Raw on demand, so events of unknown kinds are fully
// representable.
type Event struct {
	Kind    string
	Subtype string
	Raw     json.RawMessage
}

// envelope is the minimal shape shared by all stream lines.
type envelope struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
}

// SessionID returns the top-level session_id, which the CLI stamps on
// every output event. Empty if absent.
func (e Event) SessionID() string {
	var env envelope
	if json.Unmarshal(e.Raw, &env) != nil {
		return ""
	}
	return env.SessionID
}

// Init carries the fields lab needs from the system/init event.
type Init struct {
	SessionID string   `json:"session_id"`
	Model     string   `json:"model"`
	Tools     []string `json:"tools"`
}

// Init parses a system/init event. It errors on any other kind.
func (e Event) Init() (*Init, error) {
	if e.Kind != KindSystem || e.Subtype != SubtypeInit {
		return nil, fmt.Errorf("streamjson: Init on %s/%s event", e.Kind, e.Subtype)
	}
	var in Init
	if err := json.Unmarshal(e.Raw, &in); err != nil {
		return nil, fmt.Errorf("streamjson: parsing init event: %w", err)
	}
	return &in, nil
}

// Message is the Anthropic-API-shaped message wrapped by assistant and
// user events.
type Message struct {
	Role    string         `json:"role"`
	Model   string         `json:"model"`
	Content []ContentBlock `json:"content"`
}

// ContentBlock is one element of a message's content array. Type is
// always set; the remaining fields are populated according to Type
// (text/thinking → Text, tool_use → ID/Name/Input, tool_result →
// ToolUseID/Content). Raw retains the block verbatim.
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`

	Raw json.RawMessage `json:"-"`
}

func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	type plain ContentBlock
	if err := json.Unmarshal(data, (*plain)(b)); err != nil {
		return err
	}
	b.Raw = append(json.RawMessage(nil), data...)
	return nil
}

// Message parses the wrapped message of an assistant or user event. It
// errors on any other kind.
func (e Event) Message() (*Message, error) {
	if e.Kind != KindAssistant && e.Kind != KindUser {
		return nil, fmt.Errorf("streamjson: Message on %q event", e.Kind)
	}
	var wrap struct {
		Message *Message `json:"message"`
	}
	if err := json.Unmarshal(e.Raw, &wrap); err != nil {
		return nil, fmt.Errorf("streamjson: parsing %s message: %w", e.Kind, err)
	}
	if wrap.Message == nil {
		return nil, fmt.Errorf("streamjson: %s event has no message", e.Kind)
	}
	return wrap.Message, nil
}

// Text returns the concatenation of the text blocks in an assistant or
// user event's message, and "" for anything else (including parse
// failures — use Message for error detail).
func (e Event) Text() string {
	msg, err := e.Message()
	if err != nil {
		return ""
	}
	var out string
	for _, b := range msg.Content {
		if b.Type == "text" {
			out += b.Text
		}
	}
	return out
}

// ToolUse summarizes one tool_use content block.
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolUses returns the tool_use blocks of an assistant event's
// message, nil for anything else.
func (e Event) ToolUses() []ToolUse {
	msg, err := e.Message()
	if err != nil {
		return nil
	}
	var uses []ToolUse
	for _, b := range msg.Content {
		if b.Type == "tool_use" {
			uses = append(uses, ToolUse{ID: b.ID, Name: b.Name, Input: b.Input})
		}
	}
	return uses
}

// RateLimit is the rate_limit_info payload of a rate_limit_event: the
// CLI's view of the credential's current rate-limit window.
type RateLimit struct {
	Status          string `json:"status"`
	ResetsAt        int64  `json:"resetsAt"` // unix seconds
	RateLimitType   string `json:"rateLimitType"`
	OverageStatus   string `json:"overageStatus"`
	OverageResetsAt int64  `json:"overageResetsAt"` // unix seconds
	IsUsingOverage  bool   `json:"isUsingOverage"`
}

// Limited reports whether the event says the credential has hit its
// window limit. "allowed" and "allowed_warning" (approaching the
// limit) pass; any other non-empty status is treated as limited.
func (r RateLimit) Limited() bool {
	switch r.Status {
	case "", "allowed", "allowed_warning":
		return false
	}
	return true
}

// ResetTime returns resetsAt as a time, zero when absent.
func (r RateLimit) ResetTime() time.Time {
	if r.ResetsAt == 0 {
		return time.Time{}
	}
	return time.Unix(r.ResetsAt, 0).UTC()
}

// RateLimit parses a rate_limit_event. It errors on any other kind, or
// when the event carries no rate_limit_info object.
func (e Event) RateLimit() (*RateLimit, error) {
	if e.Kind != KindRateLimitEvent {
		return nil, fmt.Errorf("streamjson: RateLimit on %q event", e.Kind)
	}
	var wrap struct {
		Info *RateLimit `json:"rate_limit_info"`
	}
	if err := json.Unmarshal(e.Raw, &wrap); err != nil {
		return nil, fmt.Errorf("streamjson: parsing rate_limit_event: %w", err)
	}
	if wrap.Info == nil {
		return nil, fmt.Errorf("streamjson: rate_limit_event has no rate_limit_info")
	}
	return wrap.Info, nil
}

// Usage is the token accounting attached to a result event.
type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// Result is the terminal event of a prompt.
type Result struct {
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	DurationMS   int64   `json:"duration_ms"`
	NumTurns     int     `json:"num_turns"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	SessionID    string  `json:"session_id"`
	Usage        Usage   `json:"usage"`
}

// Result parses a result event. It errors on any other kind.
func (e Event) Result() (*Result, error) {
	if e.Kind != KindResult {
		return nil, fmt.Errorf("streamjson: Result on %q event", e.Kind)
	}
	var r Result
	if err := json.Unmarshal(e.Raw, &r); err != nil {
		return nil, fmt.Errorf("streamjson: parsing result event: %w", err)
	}
	return &r, nil
}
