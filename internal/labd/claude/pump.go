package claude

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/labd/store"
	"github.com/bio4554/lab/internal/streamjson"
)

// process is the driver's handle on one claude process's stdio —
// a container attach in production, in-memory pipes in tests.
type process struct {
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader
}

// pumpMsg is one decoded stdout event or a terminal decoder error.
type pumpMsg struct {
	ev  streamjson.Event
	err error
}

// pump drives one claude process for one lab session: stdout events
// are persisted to the event log (tagged with the in-flight turn),
// queued turns are delivered to stdin one at a time, result events
// finish turns and record usage, and the claude session id is captured
// as soon as the stream reveals it.
//
// pending, when non-nil, is a turn already flipped to running that a
// previous pump could not deliver; it is delivered first. pump returns
// a non-nil turn in the same way when it fetches a turn addressed to a
// different session (the retirement race), together with
// errSessionRetired.
//
// Other returns: errProcessExited when stdout ends without the driver
// asking, ctx.Err() on shutdown (stdin is closed and events are
// drained for the grace period first), or a persistence error.
func (d *Driver) pump(ctx context.Context, agent store.Agent, sess store.Session, proc process, pending *store.Turn) (*store.Turn, error) {
	// Store writes must survive ctx cancellation: shutdown still
	// persists drained events and finishes turns.
	dbctx := context.WithoutCancel(ctx)

	// stderr: drain concurrently (the attach demux stalls otherwise),
	// log at warn, persist nothing.
	go func() {
		sc := bufio.NewScanner(proc.stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if len(sc.Bytes()) > 0 {
				d.log.Warn("claude stderr", "agent", agent.Name, "line", sc.Text())
			}
		}
	}()

	// stdout: decode into a channel; closed when the stream ends.
	msgs := make(chan pumpMsg)
	go func() {
		defer close(msgs)
		dec := streamjson.NewDecoder(proc.stdout)
		for {
			ev, err := dec.Next()
			if err != nil {
				var mal *streamjson.MalformedLineError
				if errors.As(err, &mal) {
					d.log.Warn("malformed stream line", "agent", agent.Name, "error", mal.Err)
					continue
				}
				if !errors.Is(err, io.EOF) {
					msgs <- pumpMsg{err: err}
				}
				return
			}
			msgs <- pumpMsg{ev: ev}
		}
	}()

	enc := streamjson.NewEncoder(proc.stdin)
	var current *store.Turn
	claudeSID := ""
	if sess.ClaudeSessionID != nil {
		claudeSID = *sess.ClaudeSessionID
	}

	deliver := func(turn *store.Turn) error {
		if turn.SessionID == nil {
			if err := d.st.SetTurnSession(dbctx, turn.ID, sess.ID); err != nil {
				return err
			}
		}
		if err := enc.UserMessage(turn.Content); err != nil {
			return fmt.Errorf("claude: writing turn %s to stdin: %w", turn.ID, err)
		}
		current = turn
		return d.st.UpdateAgentState(dbctx, agent.ID, store.AgentStateWorking)
	}

	// handleEvent persists one event and applies its side effects.
	handleEvent := func(ev streamjson.Event) error {
		var turnID *uuid.UUID
		if current != nil {
			turnID = &current.ID
		}
		if _, err := d.st.AppendEvent(dbctx, sess.ID, agent.ID, turnID, ev.Kind, ev.Raw); err != nil {
			return err
		}
		if sid := ev.SessionID(); sid != "" && sid != claudeSID {
			if err := d.st.SetClaudeSessionID(dbctx, sess.ID, sid); err != nil {
				return err
			}
			claudeSID = sid
		}
		if ev.Kind == streamjson.KindRateLimitEvent {
			rl, err := ev.RateLimit()
			if err != nil {
				d.log.Warn("unparseable rate_limit_event", "agent", agent.Name, "error", err)
				return nil
			}
			if rl.Limited() {
				d.holdCredential(dbctx, agent, rl.ResetTime(),
					fmt.Sprintf("rate_limit_event status %q (%s)", rl.Status, rl.RateLimitType))
			}
			return nil
		}
		if ev.Kind != streamjson.KindResult {
			return nil
		}
		res, err := ev.Result()
		if err != nil {
			d.log.Warn("unparseable result event", "agent", agent.Name, "error", err)
			res = nil
		}
		if current != nil {
			status, errMsg := store.TurnStatusDone, ""
			if res == nil {
				status, errMsg = store.TurnStatusError, "unparseable result event"
			} else if res.IsError {
				status, errMsg = store.TurnStatusError, "result subtype "+res.Subtype
			}
			if err := d.st.FinishTurn(dbctx, current.ID, status, errMsg); err != nil {
				return err
			}
			current = nil
			if err := d.st.UpdateAgentState(dbctx, agent.ID, store.AgentStateIdle); err != nil {
				return err
			}
		}
		if res != nil && res.IsError && limitShapedResult(ev.Raw) {
			d.holdCredential(dbctx, agent, time.Time{}, "limit-shaped result error, subtype "+res.Subtype)
		}
		if res != nil && agent.CredentialID != nil {
			window := time.Now().UTC().Truncate(time.Hour)
			delta := store.UsageDelta{
				TokensIn:  res.Usage.InputTokens + res.Usage.CacheCreationInputTokens + res.Usage.CacheReadInputTokens,
				TokensOut: res.Usage.OutputTokens,
				CostUSD:   res.TotalCostUSD,
				Turns:     1,
			}
			if err := d.st.AddUsage(dbctx, *agent.CredentialID, agent.ID, window, delta); err != nil {
				return err
			}
		}
		return nil
	}

	// tryNextTurn fetches and delivers the next queued turn if idle.
	// A turn addressed to another session means this session was
	// retired: hand the turn back for the replacement process. The
	// gate is consulted before the turn is claimed, so a denied turn
	// stays queued untouched; the denial is logged once per reason and
	// re-checked on every poll/wake.
	var deniedLogged string
	tryNextTurn := func() (*store.Turn, error) {
		if current != nil {
			return nil, nil
		}
		// A restart poke (credential rebind) applies at the turn
		// boundary: never mid-turn, but promptly even when idle.
		if d.takeRestartPoke(agent.ID) {
			return nil, errRestartRequested
		}
		if d.gate != nil {
			queued, err := d.st.PeekQueuedTurn(dbctx, agent.ID)
			if err != nil || queued == nil {
				return nil, err
			}
			verdict, err := d.gate.Check(dbctx, agent.ID)
			if err != nil {
				return nil, fmt.Errorf("claude: turn gate: %w", err)
			}
			if !verdict.Allowed {
				if verdict.Reason != deniedLogged {
					d.log.Info("turn held by budget gate", "agent", agent.Name,
						"turn", queued.ID, "reason", verdict.Reason, "retry_after", verdict.RetryAfter)
					deniedLogged = verdict.Reason
				}
				return nil, nil
			}
			deniedLogged = ""
		}
		turn, err := d.st.NextQueuedTurn(dbctx, agent.ID)
		if err != nil || turn == nil {
			return nil, err
		}
		if turn.SessionID != nil && *turn.SessionID != sess.ID {
			return turn, errSessionRetired
		}
		return nil, deliver(turn)
	}

	if pending != nil {
		if err := deliver(pending); err != nil {
			return nil, err
		}
	} else if carried, err := tryNextTurn(); err != nil {
		return carried, err
	}

	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()

	// Optional enqueue wake-up (LISTEN lab_turns via labd); a nil
	// channel never fires and the ticker remains the fallback.
	var wake <-chan struct{}
	if d.turnWake != nil {
		wake = d.turnWake(agent.ID)
	}

	for {
		select {
		case <-ctx.Done():
			return nil, d.drainShutdown(dbctx, agent, proc, msgs, handleEvent, &current, ctx.Err())

		case m, ok := <-msgs:
			if !ok {
				// stdout EOF: the process died out from under us.
				if current != nil {
					if err := d.st.FinishTurn(dbctx, current.ID, store.TurnStatusError, "claude process exited mid-turn"); err != nil {
						return nil, err
					}
					current = nil
				}
				return nil, errProcessExited
			}
			if m.err != nil {
				d.log.Error("stream decode failed", "agent", agent.Name, "error", m.err)
				continue // channel close follows; handled above
			}
			if err := handleEvent(m.ev); err != nil {
				return nil, err
			}
			// A result frees the queue slot: first enforce the context
			// threshold (a crossing retires the session between turns,
			// never mid-turn), then check for the next turn immediately
			// rather than waiting out a poll tick.
			if m.ev.Kind == streamjson.KindResult {
				retired, err := d.maybeRetireForContext(dbctx, agent, m.ev)
				if err != nil {
					return nil, err
				}
				if retired {
					return nil, errSessionRetired
				}
				if carried, err := tryNextTurn(); err != nil {
					return carried, err
				}
			}

		case <-wake:
			if carried, err := tryNextTurn(); err != nil {
				return carried, err
			}

		case <-ticker.C:
			if carried, err := tryNextTurn(); err != nil {
				return carried, err
			}
		}
	}
}

// maybeRetireForContext retires the agent's session when the result
// event's context occupancy (input + cache-creation + cache-read
// tokens — what the context window held on this turn) has reached the
// agent's retire_context_tokens threshold. Called only after a result
// closes a turn, so retirement never lands mid-turn; the successor
// session starts at occupancy 0, so exactly one retirement fires per
// crossing.
func (d *Driver) maybeRetireForContext(ctx context.Context, agent store.Agent, ev streamjson.Event) (bool, error) {
	if agent.RetireContextTokens == nil {
		return false, nil
	}
	res, err := ev.Result()
	if err != nil {
		return false, nil
	}
	occupancy := res.Usage.InputTokens + res.Usage.CacheCreationInputTokens + res.Usage.CacheReadInputTokens
	if occupancy < *agent.RetireContextTokens {
		return false, nil
	}
	reason := fmt.Sprintf("context threshold (%d tokens)", *agent.RetireContextTokens)
	d.log.Info("context threshold crossed; retiring session",
		"agent", agent.Name, "occupancy", occupancy, "threshold", *agent.RetireContextTokens)
	if _, err := d.Retire(ctx, agent.ID, reason, RetireSeed(reason, agent.Name)); err != nil {
		return false, fmt.Errorf("claude: auto-retire: %w", err)
	}
	return true, nil
}

// drainShutdown is the cooperative shutdown path: close stdin (the
// process finishes its in-flight generation and exits), keep
// persisting events for the grace period, then give up. An in-flight
// turn that never produced its result is finished as error.
func (d *Driver) drainShutdown(dbctx context.Context, agent store.Agent, proc process, msgs chan pumpMsg, handleEvent func(streamjson.Event) error, current **store.Turn, cause error) error {
	if err := proc.stdin.Close(); err != nil {
		d.log.Debug("closing stdin on shutdown", "agent", agent.Name, "error", err)
	}
	deadline := time.NewTimer(d.shutdownGrace)
	defer deadline.Stop()
	for {
		select {
		case m, ok := <-msgs:
			if !ok {
				d.finishInterrupted(dbctx, agent, current, "driver shutdown before result")
				return cause
			}
			if m.err != nil {
				d.log.Error("stream decode failed", "agent", agent.Name, "error", m.err)
				continue
			}
			if err := handleEvent(m.ev); err != nil {
				d.log.Error("persisting event during shutdown", "agent", agent.Name, "error", err)
				d.finishInterrupted(dbctx, agent, current, "driver shutdown (event persistence failed)")
				return cause
			}
		case <-deadline.C:
			d.finishInterrupted(dbctx, agent, current, "hard shutdown: grace period expired mid-turn")
			// Unblock the decoder goroutine; the caller tears the
			// attach down right after.
			go func() {
				for range msgs {
				}
			}()
			return cause
		}
	}
}

// defaultLimitHold is the rate-limit hold applied when a limit is
// detected without a reset time (a limit-shaped result error). Short
// on purpose: re-checking a few times beats stalling on a guess, and
// loops still cannot spin against 429s at this cadence.
const defaultLimitHold = 5 * time.Minute

// limitErrPattern recognizes "unmistakable" rate/usage-limit result
// errors. Only consulted on is_error results, whose text is an error
// message rather than assistant output.
var limitErrPattern = regexp.MustCompile(`(?i)rate.?limit|usage limit|too many requests|\b429\b`)

func limitShapedResult(raw []byte) bool {
	return limitErrPattern.Match(raw)
}

// holdCredential records a rate-limit hold on the agent's credential
// until resetAt (defaultLimitHold from now when zero) and pauses every
// idle/working agent riding it. Containers stay up and turns stay
// queued; the budget gate keeps them queued until the hold passes (the
// daemon sweep also resumes the paused agents), or until manual
// resume. Failures are logged, not fatal: the pump must keep
// persisting events.
func (d *Driver) holdCredential(ctx context.Context, agent store.Agent, resetAt time.Time, why string) {
	if agent.CredentialID == nil {
		return
	}
	if resetAt.IsZero() {
		resetAt = time.Now().UTC().Add(defaultLimitHold)
	}
	if err := d.st.SetCredentialLimited(ctx, *agent.CredentialID, &resetAt); err != nil {
		d.log.Error("recording rate-limit hold", "credential", *agent.CredentialID, "error", err)
		return
	}
	paused, err := d.st.PauseAgentsForCredential(ctx, *agent.CredentialID)
	if err != nil {
		d.log.Error("pausing agents for rate-limited credential", "credential", *agent.CredentialID, "error", err)
	}
	d.log.Warn("credential rate limited; agents paused",
		"credential", *agent.CredentialID, "resets_at", resetAt, "agents_paused", paused, "cause", why)
}

// finishInterrupted errors the in-flight turn, if any, with reason.
func (d *Driver) finishInterrupted(dbctx context.Context, agent store.Agent, current **store.Turn, reason string) {
	if *current == nil {
		return
	}
	if err := d.st.FinishTurn(dbctx, (*current).ID, store.TurnStatusError, reason); err != nil {
		d.log.Error("finishing interrupted turn", "agent", agent.Name, "turn", (*current).ID, "error", err)
	}
	*current = nil
}
