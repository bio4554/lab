// Command claudestub is the e2e suite's scripted stand-in for the
// Claude Code CLI. labd bakes it into agent images as `claude` (via
// the builder's ClaudeStubPath hook) so the whole stack — daemon,
// Docker, stream pump, turn queue, rollups — runs deterministically
// with no Anthropic credential.
//
// Protocol (a faithful, minimal subset of --output-format stream-json):
//
//   - on start: one system/init event carrying the session id — the id
//     from --resume when present, a fresh one otherwise;
//   - per stdin user-message line: one assistant event echoing the
//     prompt, then one result event with fixed usage numbers that
//     closes the turn;
//   - a prompt of the form "sleep:<seconds>" delays the result — the
//     kill-mid-turn recovery test uses it to die with a turn running;
//   - a prompt of the form "fail" produces an is_error result;
//   - stdin EOF: exit 0 (StdinOnce semantics: detach ⇒ process exit).
//
// All other flags (role prompt, model, permissions) are accepted and
// ignored.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	sessionID := ""
	args := os.Args[1:]
	for i, a := range args {
		if a == "--resume" && i+1 < len(args) {
			sessionID = args[i+1]
		}
	}
	if sessionID == "" {
		sessionID = newUUID()
	}

	out := json.NewEncoder(os.Stdout)
	emit := func(v any) {
		if err := out.Encode(v); err != nil {
			fmt.Fprintln(os.Stderr, "claudestub: write:", err)
			os.Exit(1)
		}
	}

	emit(map[string]any{
		"type": "system", "subtype": "init",
		"session_id": sessionID,
		"cwd":        "/work",
		"model":      "claude-stub",
		"tools":      []string{"Bash"},
	})

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	turns := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var in struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &in); err != nil || in.Type != "user" {
			fmt.Fprintln(os.Stderr, "claudestub: ignoring unparseable stdin line")
			continue
		}
		prompt := ""
		for _, b := range in.Message.Content {
			if b.Type == "text" {
				prompt += b.Text
			}
		}
		turns++

		if rest, ok := strings.CutPrefix(prompt, "sleep:"); ok {
			if secs, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
				time.Sleep(time.Duration(secs) * time.Second)
			}
		}

		emit(map[string]any{
			"type":       "assistant",
			"session_id": sessionID,
			"message": map[string]any{
				"role":  "assistant",
				"model": "claude-stub",
				"content": []map[string]any{
					{"type": "text", "text": "stub-echo: " + prompt},
				},
			},
		})

		isError := prompt == "fail"
		subtype := "success"
		if isError {
			subtype = "error_during_execution"
		}
		emit(map[string]any{
			"type":           "result",
			"subtype":        subtype,
			"is_error":       isError,
			"num_turns":      turns,
			"session_id":     sessionID,
			"total_cost_usd": 0.000123,
			"usage": map[string]any{
				"input_tokens":                100,
				"cache_creation_input_tokens": 200,
				"cache_read_input_tokens":     300,
				"output_tokens":               42,
			},
		})
	}
}

// newUUID returns a random v4-shaped UUID without external deps.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
