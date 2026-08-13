package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/bio4554/lab/internal/streamjson"
	"github.com/bio4554/lab/internal/wire"
)

// Transcript rendering vocabulary (mockup 1c): role-gutter text blocks
// (`you ▌` / `<agent> ▌`), ⚙ tool one-liners, ✔/✘ result lines, dim
// ·-prefixed raw lines for unknown kinds. buildTranscript is a pure
// function over stored events so it is unit-testable without a
// terminal.

type entryKind int

const (
	entryMarker entryKind = iota // session/system marker line
	entryUser                    // "you ▌ text"
	entryAgent                   // "<agent> ▌ text"
	entryTool                    // "⚙ name summary [· outcome]"
	entryResult                  // "✔ done · 3.2s · 3.1k→612 tok · $0.02"
	entryRaw                     // "· kind seq=N {payload…}"
)

// TranscriptEntry is one rendered line-group of the transcript.
type TranscriptEntry struct {
	Kind entryKind
	Text string // main text (message body, marker text, raw line)
	Time string // HH:MM:SS stamp shown on user turns

	// entryTool only:
	Tool        string
	ToolSummary string
	Outcome     string // "", "✓", or "✗"

	// entryResult only:
	IsError bool
}

// toolCall tracks a pending tool_use so its tool_result can mark the
// outcome on the same entry.
type toolCall struct{ entryIdx int }

// buildTranscript folds stored events into transcript entries.
// agentName labels assistant text blocks. Events must belong to one
// session; they are sorted by seq before folding.
func buildTranscript(events []wire.Event, agentName string) []TranscriptEntry {
	sorted := make([]wire.Event, len(events))
	copy(sorted, events)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })

	var entries []TranscriptEntry
	pending := map[string]toolCall{} // tool_use id → entry
	for _, e := range sorted {
		ev := streamjson.Event{Raw: []byte(e.Payload)}
		var env struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		_ = json.Unmarshal(e.Payload, &env)
		ev.Kind, ev.Subtype = env.Type, env.Subtype

		switch ev.Kind {
		case streamjson.KindSystem:
			if ev.Subtype == streamjson.SubtypeInit {
				text := "── session started"
				if in, err := ev.Init(); err == nil && in.Model != "" {
					text += " · " + in.Model
				}
				text += " · " + e.TS.Local().Format("15:04:05") + " ──"
				entries = append(entries, TranscriptEntry{Kind: entryMarker, Text: text})
			}
			// Other system subtypes (thinking_tokens, …) are noise; skip.

		case streamjson.KindAssistant:
			msg, err := ev.Message()
			if err != nil {
				entries = append(entries, rawEntry(e))
				continue
			}
			for _, b := range msg.Content {
				switch b.Type {
				case "text":
					if strings.TrimSpace(b.Text) != "" {
						entries = append(entries, TranscriptEntry{Kind: entryAgent, Text: b.Text})
					}
				case "tool_use":
					entries = append(entries, TranscriptEntry{
						Kind: entryTool, Tool: b.Name, ToolSummary: toolInputSummary(b.Input),
					})
					pending[b.ID] = toolCall{entryIdx: len(entries) - 1}
				}
			}

		case streamjson.KindUser:
			msg, err := ev.Message()
			if err != nil {
				entries = append(entries, rawEntry(e))
				continue
			}
			for _, b := range msg.Content {
				switch b.Type {
				case "text":
					if strings.TrimSpace(b.Text) != "" {
						entries = append(entries, TranscriptEntry{
							Kind: entryUser, Text: b.Text, Time: e.TS.Local().Format("15:04:05"),
						})
					}
				case "tool_result":
					if call, ok := pending[b.ToolUseID]; ok {
						outcome := "✓"
						var res struct {
							IsError bool `json:"is_error"`
						}
						if json.Unmarshal(b.Raw, &res) == nil && res.IsError {
							outcome = "✗"
						}
						entries[call.entryIdx].Outcome = outcome
						delete(pending, b.ToolUseID)
					}
				}
			}

		case streamjson.KindResult:
			r, err := ev.Result()
			if err != nil {
				entries = append(entries, rawEntry(e))
				continue
			}
			entries = append(entries, TranscriptEntry{
				Kind:    entryResult,
				IsError: r.IsError,
				Text:    resultSummary(r),
			})

		default:
			entries = append(entries, rawEntry(e))
		}
	}
	return entries
}

// rawEntry renders an event of unknown (or unparsable) kind as a dim
// one-liner: `· kind seq=N payload…`.
func rawEntry(e wire.Event) TranscriptEntry {
	payload := string(e.Payload)
	return TranscriptEntry{
		Kind: entryRaw,
		Text: fmt.Sprintf("· %s seq=%d %s", e.Kind, e.Seq, truncate(payload, 80)),
	}
}

// resultSummary is the ✔/✘ line: status · duration · in→out tok · cost.
func resultSummary(r *streamjson.Result) string {
	status := "done"
	if r.IsError {
		status = "error"
	}
	in := r.Usage.InputTokens + r.Usage.CacheCreationInputTokens + r.Usage.CacheReadInputTokens
	parts := []string{status, fmtDuration(r.DurationMS), fmt.Sprintf("%s → %s tok", fmtTokens(in), fmtTokens(r.Usage.OutputTokens))}
	if r.TotalCostUSD > 0 {
		parts = append(parts, fmtCost(r.TotalCostUSD))
	}
	return strings.Join(parts, " · ")
}

// toolInputSummary condenses a tool_use input object into one short
// argument string: well-known fields first, then any string field.
func toolInputSummary(input json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil || len(m) == 0 {
		return ""
	}
	for _, key := range []string{"command", "file_path", "path", "pattern", "url", "query", "description", "prompt", "content"} {
		if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
			return truncate(oneLine(v), 60)
		}
	}
	// Fall back to the first (alphabetical, for determinism) string.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return truncate(oneLine(v), 60)
		}
	}
	b, _ := json.Marshal(m)
	return truncate(string(b), 60)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// fmtTokens renders a token count like the mockups: 612, 3.1k, 1.4M.
func fmtTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return trimZero(fmt.Sprintf("%.1fM", float64(n)/1_000_000))
	case n >= 1_000:
		return trimZero(fmt.Sprintf("%.1fk", float64(n)/1_000))
	default:
		return fmt.Sprintf("%d", n)
	}
}

func trimZero(s string) string {
	return strings.Replace(s, ".0", "", 1)
}

func fmtCost(usd float64) string {
	if usd >= 10 {
		return fmt.Sprintf("$%.2f", usd)
	}
	return fmt.Sprintf("$%.3f", usd)
}

func fmtDuration(ms int64) string {
	switch {
	case ms >= 60_000:
		return fmt.Sprintf("%dm%02ds", ms/60_000, ms%60_000/1000)
	case ms >= 1000:
		return trimZero(fmt.Sprintf("%.1fs", float64(ms)/1000))
	default:
		return fmt.Sprintf("%dms", ms)
	}
}

// renderEntry styles one entry into (possibly wrapped) terminal lines.
func renderEntry(entry TranscriptEntry, agentName string, width int) string {
	if width < 20 {
		width = 20
	}
	switch entry.Kind {
	case entryMarker:
		return sDim.Render(truncate(entry.Text, width))
	case entryUser:
		gutter := sAccent.Render("you ▌ ")
		stamp := ""
		if entry.Time != "" {
			stamp = "  " + sFaint.Render(entry.Time)
		}
		body := sText.Width(width - 6).Render(entry.Text)
		return gutter + strings.TrimRight(indentCont(body, 6), "\n") + stamp
	case entryAgent:
		gutter := sAccent2.Render(agentName + " ▌ ")
		w := width - len(agentName) - 3
		body := sMuted.Width(w).Render(entry.Text)
		return gutter + strings.TrimRight(indentCont(body, len(agentName)+3), "\n")
	case entryTool:
		line := "⚙ " + entry.Tool
		if entry.ToolSummary != "" {
			line += " " + entry.ToolSummary
		}
		out := sDim.Render(truncate(line, width-4))
		switch entry.Outcome {
		case "✓":
			out += sGood.Render(" · ✓")
		case "✗":
			out += sBad.Render(" · ✗")
		}
		return out
	case entryResult:
		if entry.IsError {
			return sBad.Render("✘ " + truncate(entry.Text, width-2))
		}
		return sGood.Render("✔ " + truncate(entry.Text, width-2))
	default: // entryRaw
		return sFaint.Render(truncate(entry.Text, width))
	}
}

// indentCont indents every wrapped line after the first so continuation
// lines clear the role gutter.
func indentCont(s string, n int) string {
	lines := strings.Split(s, "\n")
	pad := strings.Repeat(" ", n)
	for i := 1; i < len(lines); i++ {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}
