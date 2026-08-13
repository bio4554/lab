package claude

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/labd/store"
)

// RetireSeed is the default first prompt of a successor session, used
// by auto-retirement and when a retire request carries no explicit
// seed. Identity is already re-injected via --append-system-prompt;
// the seed's job is memory recovery through kbase — the successor
// pulls its own memory via the CLI (fresher than embedding recall
// output server-side, and CLI-over-Bash is the standard agent
// interface).
func RetireSeed(reason, recallQuery string) string {
	return fmt.Sprintf("Your previous session was retired (%s). "+
		"Recover your working state: run `kbase recall %q`, `kbase ticket list`, "+
		"and `kbase show` on anything relevant, then continue your work. "+
		"Report status with `lab-agent status`.", reason, recallQuery)
}

// Retire ends the agent's current session and starts a fresh one:
// the old session is ended with reason, a new session is created
// chained to it (prev_session_id), seedPrompt is enqueued as the new
// session's first turn, and the agent's container is stopped so the
// running driver (possibly in another process) replaces the claude
// process — the new session has no claude_session_id, so the
// replacement starts without --resume.
//
// The seed turn is addressed to the new session explicitly; a pump
// that fetches it while still attached to the old process detects the
// mismatch and restarts instead of delivering it (see pump).
//
// Retire is safe when no driver is running: the next Run picks up the
// new session and the queued seed.
func (d *Driver) Retire(ctx context.Context, agentID uuid.UUID, reason, seedPrompt string) (store.Session, error) {
	agent, err := d.st.GetAgent(ctx, agentID)
	if err != nil {
		return store.Session{}, err
	}

	var prev *uuid.UUID
	if old, err := d.st.CurrentSession(ctx, agentID); err != nil {
		return store.Session{}, err
	} else if old != nil {
		if err := d.st.EndSession(ctx, old.ID, reason); err != nil {
			return store.Session{}, err
		}
		prev = &old.ID
	}

	next, err := d.st.CreateSession(ctx, agentID, prev)
	if err != nil {
		return store.Session{}, err
	}
	if seedPrompt != "" {
		if _, err := d.st.EnqueueTurn(ctx, store.NewTurn{
			AgentID:    agentID,
			SessionID:  &next.ID,
			SourceKind: store.SourceKindUser,
			Content:    seedPrompt,
		}); err != nil {
			return store.Session{}, err
		}
	}

	// Stop the old process so the driver re-provisions against the new
	// session. Best-effort: the container may already be gone, or no
	// driver may be running at all.
	if d.rt != nil {
		containers, err := d.rt.List(ctx, agent.ProjectID.String())
		if err != nil {
			d.log.Warn("listing containers for retirement", "agent", agent.Name, "error", err)
			return next, nil
		}
		for _, c := range containers {
			if c.AgentID != agentID.String() || c.State != "running" {
				continue
			}
			d.log.Info("stopping container for retirement", "agent", agent.Name, "container", c.ID[:12])
			if err := d.rt.Stop(ctx, c.ID, 10); err != nil {
				d.log.Warn("stopping container for retirement", "agent", agent.Name, "error", err)
			}
		}
	}
	return next, nil
}
