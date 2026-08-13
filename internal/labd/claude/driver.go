// Package claude is the agent driver: it owns one agent's runtime
// lifecycle — provisioning (worktree, image, container), the claude
// process speaking stream-json, turn delivery from the Postgres queue,
// event persistence, restart/resume, and session retirement.
//
// Persistence is resumption, not process lifetime: the claude process
// may die at any time; continuity comes from --resume against the
// agent's mounted .claude volume, driven by the stored
// claude_session_id.
package claude

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/labd/budget"
	"github.com/bio4554/lab/internal/labd/gitrepo"
	"github.com/bio4554/lab/internal/labd/runtime"
	"github.com/bio4554/lab/internal/labd/store"
)

// TurnGate is consulted before each queued turn is delivered to the
// claude process. A denied turn stays queued (never errored); the pump
// logs the denial once and re-checks on its poll/wake cadence, so the
// turn delivers as soon as the gate opens. labd wires budget.Gate
// here; nil always allows.
type TurnGate interface {
	Check(ctx context.Context, agentID uuid.UUID) (budget.Verdict, error)
}

// Sentinel pump outcomes, internal to the Run loop.
var (
	// errProcessExited: the claude process ended (stdout EOF) without
	// the driver asking it to. Run replaces the container and resumes.
	errProcessExited = errors.New("claude: process exited")
	// errSessionRetired: the pump fetched a turn addressed to a
	// different (newer) session — the current session was retired out
	// from under it. Run re-provisions against the new session.
	errSessionRetired = errors.New("claude: session retired")
)

// Options configures a Driver. Store is required; Git, Runtime and
// Builder are required for Run/Retire but may be nil in tests that
// exercise only the pump.
type Options struct {
	Store   *store.Store
	Git     *gitrepo.Manager
	Runtime *runtime.Runtime
	Builder *runtime.Builder
	// Creds resolves an agent's credential row to an env var + secret.
	// Nil defaults to EnvCredentialSource.
	Creds  CredentialSource
	Logger *slog.Logger
	// AgentAPIURL, when non-empty, is the labd agent API base URL as
	// reachable from inside containers (http://host.docker.internal:
	// <port>). It enables the lab env contract: each container create
	// mints a fresh agent token (revoking the agent's previous ones)
	// and injects LAB_AGENT_ID, LAB_AGENT_TOKEN, LAB_API_URL and
	// LAB_PROJECT. Empty (labctl's in-process mode) injects none.
	AgentAPIURL string
	// TurnGate, when non-nil, is checked before every turn delivery;
	// see the interface doc. Nil allows every turn.
	TurnGate TurnGate
	// KBase, when non-nil, provisions each container's kbase access at
	// create time: ensure a principal for the agent, mint a fresh
	// project-scoped token (prior ones revoked), injected as KBASE_URL
	// + KBASE_TOKEN. Failures degrade gracefully — the container
	// starts without kbase env (kbase is a dependency, not a hard
	// requirement). labd wires a kbclient.TokenProvisioner here.
	KBase KBaseTokenSource
	// TurnWake, when non-nil, returns a channel signalled whenever a
	// turn is enqueued for the agent (labd wires this to LISTEN
	// lab_turns). The idle poll remains the fallback.
	TurnWake func(agentID uuid.UUID) <-chan struct{}
	// PollInterval is the idle turn-queue poll cadence. Default 1s.
	// (LISTEN wiring arrives with Phase 6.)
	PollInterval time.Duration
	// ShutdownGrace is how long a shutdown waits for the claude
	// process to finish its in-flight generation after stdin closes
	// before the container is stopped hard. Default 15s.
	ShutdownGrace time.Duration
	// RestartBackoff is the pause before replacing a dead claude
	// process. Default 3s.
	RestartBackoff time.Duration
}

// Driver drives one or more agents' claude processes. Methods are safe
// to call from separate processes (labctl runs Run and Retire in
// different processes); coordination happens through Postgres and
// Docker, not shared memory.
type Driver struct {
	st    *store.Store
	git   *gitrepo.Manager
	rt    *runtime.Runtime
	build *runtime.Builder
	creds CredentialSource
	log   *slog.Logger

	agentAPIURL    string
	gate           TurnGate
	kbase          KBaseTokenSource
	turnWake       func(uuid.UUID) <-chan struct{}
	pollInterval   time.Duration
	shutdownGrace  time.Duration
	restartBackoff time.Duration
}

// New returns a Driver.
func New(opts Options) *Driver {
	d := &Driver{
		st:             opts.Store,
		git:            opts.Git,
		rt:             opts.Runtime,
		build:          opts.Builder,
		creds:          opts.Creds,
		log:            opts.Logger,
		agentAPIURL:    opts.AgentAPIURL,
		gate:           opts.TurnGate,
		kbase:          opts.KBase,
		turnWake:       opts.TurnWake,
		pollInterval:   opts.PollInterval,
		shutdownGrace:  opts.ShutdownGrace,
		restartBackoff: opts.RestartBackoff,
	}
	if d.creds == nil {
		d.creds = EnvCredentialSource{}
	}
	if d.log == nil {
		d.log = slog.Default()
	}
	if d.pollInterval <= 0 {
		d.pollInterval = time.Second
	}
	if d.shutdownGrace <= 0 {
		d.shutdownGrace = 15 * time.Second
	}
	if d.restartBackoff <= 0 {
		d.restartBackoff = 3 * time.Second
	}
	return d
}

// claudeArgs builds the container main-process argv. --resume is
// included iff the lab session already has a claude_session_id (a
// process restart, not a fresh session).
func claudeArgs(rolePrompt string, model *string, claudeSessionID *string) []string {
	args := []string{
		"claude", "-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
	}
	if rolePrompt != "" {
		args = append(args, "--append-system-prompt", rolePrompt)
	}
	if model != nil && *model != "" {
		args = append(args, "--model", *model)
	}
	if claudeSessionID != nil && *claudeSessionID != "" {
		args = append(args, "--resume", *claudeSessionID)
	}
	return args
}

// Run provisions the agent and pumps its claude process until ctx is
// cancelled. If the process dies it is replaced (same worktree, same
// .claude volume, --resume) and the lab session continues. Run returns
// ctx.Err() on shutdown, or a non-ctx error if provisioning or
// persistence fails unrecoverably.
func (d *Driver) Run(ctx context.Context, agentID uuid.UUID) error {
	agent, err := d.st.GetAgent(ctx, agentID)
	if err != nil {
		return err
	}
	project, err := d.st.GetProject(ctx, agent.ProjectID)
	if err != nil {
		return err
	}

	// A turn left running by a previous driver crash can never finish
	// (its events are gone with the old process) and would block the
	// serial queue forever.
	if t, err := d.st.RunningTurn(ctx, agent.ID); err != nil {
		return err
	} else if t != nil {
		d.log.Warn("erroring orphaned running turn", "agent", agent.Name, "turn", t.ID)
		if err := d.st.FinishTurn(ctx, t.ID, store.TurnStatusError, "orphaned by driver restart"); err != nil {
			return err
		}
	}

	var pending *store.Turn // turn carried across a session retirement
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sess, err := d.ensureSession(ctx, agent.ID)
		if err != nil {
			return err
		}
		if pending != nil && pending.SessionID != nil && *pending.SessionID != sess.ID {
			// Can only happen if the session changed again between
			// pump exits; don't let the turn wedge the queue.
			d.log.Warn("pending turn addressed to a stale session", "turn", pending.ID)
			if err := d.st.FinishTurn(ctx, pending.ID, store.TurnStatusError, "session retired before delivery"); err != nil {
				return err
			}
			pending = nil
		}

		pending, err = d.runProcess(ctx, project, agent, sess, pending)
		switch {
		case ctx.Err() != nil:
			d.setStopped(agent.ID)
			return ctx.Err()
		case errors.Is(err, errSessionRetired):
			d.log.Info("session retired; starting fresh process", "agent", agent.Name)
		case errors.Is(err, errProcessExited):
			d.log.Warn("claude process exited; replacing", "agent", agent.Name, "backoff", d.restartBackoff)
			select {
			case <-time.After(d.restartBackoff):
			case <-ctx.Done():
				d.setStopped(agent.ID)
				return ctx.Err()
			}
		case err != nil:
			d.setStopped(agent.ID)
			return err
		}
	}
}

// runProcess provisions one container + claude process for the session
// and pumps it to completion. It returns the pump's carried-over turn
// (see pump) and one of the sentinel errors, ctx.Err(), or a fatal
// error.
func (d *Driver) runProcess(ctx context.Context, project store.Project, agent store.Agent, sess store.Session, pending *store.Turn) (*store.Turn, error) {
	if d.git == nil || d.rt == nil || d.build == nil {
		return pending, fmt.Errorf("claude: driver not configured for provisioning (git/runtime/builder)")
	}

	worktree, err := d.git.AddWorktree(ctx, project.ID.String(), agent.ID.String(), agent.Name)
	if err != nil {
		return pending, err
	}
	image, err := d.build.EnsureImage(ctx, project.Stack)
	if err != nil {
		return pending, err
	}
	env, err := d.resolveEnv(ctx, agent)
	if err != nil {
		return pending, err
	}
	env, err = d.addLabEnv(ctx, project, agent, env)
	if err != nil {
		return pending, err
	}
	env = d.addKBaseEnv(ctx, project, agent, env)

	// Replace any existing container for this agent: with StdinOnce
	// semantics a container we are not attached to has a dead (or
	// doomed) claude process; --resume carries the continuity.
	if err := d.removeContainers(ctx, project, agent); err != nil {
		return pending, err
	}

	id, err := d.rt.Create(ctx, runtime.Spec{
		AgentID:      agent.ID.String(),
		ProjectID:    project.ID.String(),
		Image:        image,
		WorktreePath: worktree,
		Env:          env,
		Cmd:          claudeArgs(agent.RolePrompt, agent.Model, sess.ClaudeSessionID),
	})
	if err != nil {
		return pending, err
	}
	if err := d.st.SetAgentContainer(ctx, agent.ID, &id); err != nil {
		return pending, err
	}

	// The attach must outlive ctx: on shutdown the pump keeps draining
	// events through the grace period after ctx is already done.
	attachCtx, detach := context.WithCancel(context.WithoutCancel(ctx))
	defer detach()
	stdin, stdout, stderr, err := d.rt.Attach(attachCtx, id)
	if err != nil {
		return pending, err
	}
	if err := d.rt.Start(ctx, id); err != nil {
		return pending, err
	}
	if err := d.st.UpdateAgentState(ctx, agent.ID, store.AgentStateIdle); err != nil {
		return pending, err
	}
	d.log.Info("claude process started", "agent", agent.Name, "container", id[:12],
		"resume", sess.ClaudeSessionID != nil, "session", sess.ID)

	pending, pumpErr := d.pump(ctx, agent, sess, process{stdin: stdin, stdout: stdout, stderr: stderr}, pending)

	// Tear down: detach, then stop the container with a short timeout
	// (the process is usually already dead — stdin closed or stdout
	// EOF). Best-effort; the next provision force-removes leftovers.
	detach()
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := d.rt.Stop(stopCtx, id, 5); err != nil {
		d.log.Debug("stopping container after pump", "container", id[:12], "error", err)
	}
	return pending, pumpErr
}

// ensureSession returns the agent's open session, creating the first
// one (unchained) if none exists.
func (d *Driver) ensureSession(ctx context.Context, agentID uuid.UUID) (store.Session, error) {
	if sess, err := d.st.CurrentSession(ctx, agentID); err != nil {
		return store.Session{}, err
	} else if sess != nil {
		return *sess, nil
	}
	return d.st.CreateSession(ctx, agentID, nil)
}

// removeContainers force-removes every container labelled with this
// agent's ID (normally at most one).
func (d *Driver) removeContainers(ctx context.Context, project store.Project, agent store.Agent) error {
	containers, err := d.rt.List(ctx, project.ID.String())
	if err != nil {
		return err
	}
	for _, c := range containers {
		if c.AgentID != agent.ID.String() {
			continue
		}
		d.log.Debug("removing stale container", "agent", agent.Name, "container", c.ID[:12], "state", c.State)
		if err := d.rt.Remove(ctx, c.ID, true); err != nil {
			return err
		}
	}
	return nil
}

// resolveEnv builds the container env: exactly one Anthropic
// credential variable, resolved through the CredentialSource. An agent
// without a credential gets an empty env (the claude process will fail
// its first prompt; useful only in tests).
func (d *Driver) resolveEnv(ctx context.Context, agent store.Agent) (map[string]string, error) {
	if agent.CredentialID == nil {
		d.log.Warn("agent has no credential; claude will run unauthenticated", "agent", agent.Name)
		return nil, nil
	}
	cred, err := d.st.GetCredential(ctx, *agent.CredentialID)
	if err != nil {
		return nil, err
	}
	envVar, secret, err := d.creds.Resolve(ctx, cred)
	if err != nil {
		return nil, err
	}
	return map[string]string{envVar: secret}, nil
}

// addLabEnv adds the lab API contract to the container env: a freshly
// minted bearer token (previous tokens revoked — one live token per
// agent, rotated on every container create) plus the agent's identity
// and the API base URL. No-op when the driver has no AgentAPIURL
// (labctl's in-process mode). The plaintext token goes only into the
// env map; it is never logged or persisted.
func (d *Driver) addLabEnv(ctx context.Context, project store.Project, agent store.Agent, env map[string]string) (map[string]string, error) {
	if d.agentAPIURL == "" {
		return env, nil
	}
	if _, err := d.st.RevokeAgentTokens(ctx, agent.ID); err != nil {
		return nil, err
	}
	_, secret, err := d.st.MintAgentToken(ctx, agent.ID)
	if err != nil {
		return nil, err
	}
	if env == nil {
		env = make(map[string]string, 4)
	}
	env["LAB_AGENT_ID"] = agent.ID.String()
	env["LAB_AGENT_TOKEN"] = secret
	env["LAB_API_URL"] = d.agentAPIURL
	env["LAB_PROJECT"] = project.Name
	return env, nil
}

// KBaseTokenSource readies an agent's kbase access at container
// create: ensure a kbase principal for the agent, mint a fresh
// project-scoped token (revoking prior ones), and return the
// container-visible kbased URL plus the plaintext token.
type KBaseTokenSource interface {
	ProvisionAgentToken(ctx context.Context, agentID uuid.UUID, displayName string, projectID uuid.UUID) (url, token string, err error)
}

// addKBaseEnv adds the kbase contract (KBASE_URL + KBASE_TOKEN) to the
// container env. Unlike the lab env, kbase provisioning failures are
// not fatal: kbased being down costs the agent its knowledge base, not
// its ability to run — log a warning and start without.
func (d *Driver) addKBaseEnv(ctx context.Context, project store.Project, agent store.Agent, env map[string]string) map[string]string {
	if d.kbase == nil {
		return env
	}
	provCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	url, token, err := d.kbase.ProvisionAgentToken(provCtx, agent.ID, agent.Name, project.ID)
	if err != nil {
		d.log.Warn("kbase provisioning failed; starting agent without kbase access",
			"agent", agent.Name, "error", err)
		return env
	}
	if env == nil {
		env = make(map[string]string, 2)
	}
	env["KBASE_URL"] = url
	env["KBASE_TOKEN"] = token
	return env
}

// setStopped best-effort marks the agent stopped on driver exit.
func (d *Driver) setStopped(agentID uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.st.UpdateAgentState(ctx, agentID, store.AgentStateStopped); err != nil {
		d.log.Warn("marking agent stopped", "agent", agentID, "error", err)
	}
}
