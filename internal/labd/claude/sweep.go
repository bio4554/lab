package claude

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/labd/runtime"
	"github.com/bio4554/lab/internal/labd/store"
)

// ContainerRuntime is the runtime subset the boot sweep needs; tests
// substitute a fake so the decision table runs without Docker.
type ContainerRuntime interface {
	List(ctx context.Context, projectID string) ([]runtime.Container, error)
	Remove(ctx context.Context, containerID string, force bool) error
	RemoveVolume(ctx context.Context, agentID string, force bool) error
}

// Sweeper reconciles container reality against DB state. labd runs
// Sweep once on boot, after the store is reachable and before any
// driver starts or traffic is served: everything it repairs is a
// leftover of a daemon that died without cleaning up.
type Sweeper struct {
	St  *store.Store
	Rt  ContainerRuntime
	Log *slog.Logger
}

// Sweep applies the reconciliation decision table:
//
//   - container whose agent no longer exists in the DB → remove it and
//     its .claude volume;
//   - agent whose recorded container_id is stale (container gone or
//     replaced) → clear or correct the recorded id;
//   - turn stuck running (its pump died with the old daemon) → error
//     "labd restarted mid-turn" (queued turns stay queued);
//   - agent in state working with no live driver — on boot that is
//     every working agent → back to idle; stopped/paused/retired are
//     respected as-is.
//
// The sweep is idempotent: a second run finds nothing to do and logs
// nothing. Individual repair failures are logged and joined into the
// returned error; the sweep always visits every row it can.
func (s *Sweeper) Sweep(ctx context.Context) error {
	log := s.Log
	if log == nil {
		log = slog.Default()
	}

	var errs []error

	containers, err := s.Rt.List(ctx, "")
	if err != nil {
		return fmt.Errorf("boot sweep: listing containers: %w", err)
	}
	// live maps agent ID → that agent's actual container ID. Normally
	// at most one container per agent; duplicates keep the last listed
	// (the stale-id repair below tolerates either).
	live := make(map[uuid.UUID]string, len(containers))
	for _, c := range containers {
		agentID, err := uuid.Parse(c.AgentID)
		if err != nil {
			log.Warn("sweep: container with unparseable agent label; skipping",
				"container", short(c.ID), "label", c.AgentID)
			continue
		}
		_, err = s.St.GetAgent(ctx, agentID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			log.Info("sweep: removing container for deleted agent",
				"container", short(c.ID), "agent", c.AgentID)
			if err := s.Rt.Remove(ctx, c.ID, true); err != nil {
				errs = append(errs, err)
				log.Error("sweep: removing container", "container", short(c.ID), "error", err)
				continue
			}
			if err := s.Rt.RemoveVolume(ctx, c.AgentID, true); err != nil {
				// The volume may legitimately not exist; log, don't fail.
				log.Warn("sweep: removing volume for deleted agent", "agent", c.AgentID, "error", err)
			}
		case err != nil:
			errs = append(errs, err)
			log.Error("sweep: looking up container's agent", "agent", c.AgentID, "error", err)
		default:
			live[agentID] = c.ID
		}
	}

	agents, err := s.St.AllAgents(ctx)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("boot sweep: listing agents: %w", err))...)
	}
	for _, a := range agents {
		if a.ContainerID != nil {
			switch actual, ok := live[a.ID]; {
			case !ok:
				log.Info("sweep: clearing stale container id",
					"agent", a.Name, "container", short(*a.ContainerID))
				if err := s.St.SetAgentContainer(ctx, a.ID, nil); err != nil {
					errs = append(errs, err)
					log.Error("sweep: clearing container id", "agent", a.Name, "error", err)
				}
			case actual != *a.ContainerID:
				log.Info("sweep: correcting recorded container id",
					"agent", a.Name, "recorded", short(*a.ContainerID), "container", short(actual))
				if err := s.St.SetAgentContainer(ctx, a.ID, &actual); err != nil {
					errs = append(errs, err)
					log.Error("sweep: correcting container id", "agent", a.Name, "error", err)
				}
			}
		}
		if a.State == store.AgentStateWorking {
			log.Info("sweep: resetting working agent to idle", "agent", a.Name)
			if err := s.St.UpdateAgentState(ctx, a.ID, store.AgentStateIdle); err != nil {
				errs = append(errs, err)
				log.Error("sweep: resetting agent state", "agent", a.Name, "error", err)
			}
		}
	}

	running, err := s.St.AllRunningTurns(ctx)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("boot sweep: listing running turns: %w", err))...)
	}
	for _, t := range running {
		log.Info("sweep: erroring turn orphaned mid-turn", "turn", t.ID, "agent", t.AgentID)
		if err := s.St.FinishTurn(ctx, t.ID, store.TurnStatusError, "labd restarted mid-turn"); err != nil {
			errs = append(errs, err)
			log.Error("sweep: erroring orphaned turn", "turn", t.ID, "error", err)
		}
	}

	return errors.Join(errs...)
}

// short truncates a container ID for logging.
func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
