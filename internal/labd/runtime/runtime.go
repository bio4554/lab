// Package runtime owns the Docker side of labd: building the per-stack
// agent images (from the templates embedded in the stacks package) and
// running agent containers — create/start/stop/remove/inspect/list plus
// a demultiplexed stdio attach that Phase 5 pumps stream-json through.
// It talks to the Docker Engine API via the official client; nothing
// shells out to the docker CLI.
package runtime

import (
	"context"
	"fmt"
	"sort"

	"github.com/bio4554/lab/stacks"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
)

// Labels applied to every agent container; List filters on them and
// labd's restart reconciliation depends on them.
const (
	LabelAgentID   = "lab.agent-id"
	LabelProjectID = "lab.project-id"
)

// Stacks returns the valid stack names, sorted. Phase 5+ validates
// project creation against this list. It is derived from the embedded
// templates so the list can never drift from stacks/.
func Stacks() []string {
	entries, err := stacks.FS.ReadDir(".")
	if err != nil {
		// The FS is embedded at compile time; failure to read it is a
		// build defect, not a runtime condition.
		panic(fmt.Sprintf("runtime: reading embedded stacks: %v", err))
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// ValidStack reports whether name is a known stack.
func ValidStack(name string) bool {
	for _, s := range Stacks() {
		if s == name {
			return true
		}
	}
	return false
}

// Runtime wraps a Docker client with lab's container conventions.
type Runtime struct {
	cli *client.Client
}

// New connects to the Docker daemon using the standard environment
// (DOCKER_HOST etc.) with API version negotiation. The connection is
// lazy; use Ping to probe reachability.
func New() (*Runtime, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("runtime: new docker client: %w", err)
	}
	return &Runtime{cli: cli}, nil
}

// Close releases the underlying client.
func (r *Runtime) Close() error { return r.cli.Close() }

// Ping probes the Docker daemon.
func (r *Runtime) Ping(ctx context.Context) error {
	_, err := r.cli.Ping(ctx)
	return err
}

// Spec describes one agent container.
type Spec struct {
	AgentID      string            // container is named lab-agent-<AgentID>
	ProjectID    string            // recorded as a label
	Image        string            // image tag from Builder.EnsureImage
	WorktreePath string            // host path bind-mounted rw at /work
	Env          map[string]string // passed through verbatim; credential selection is the caller's job
	Cmd          []string          // main process argv; nil → sleep infinity (tests)
}

// ContainerName returns the container name for an agent ID.
func ContainerName(agentID string) string { return "lab-agent-" + agentID }

// VolumeName returns the per-agent named volume that persists
// /home/agent/.claude across container replacements.
func VolumeName(agentID string) string { return "lab-claude-" + agentID }

// Create creates (but does not start) the agent container and returns
// its Docker ID. The named .claude volume is created implicitly by
// Docker if absent.
func (r *Runtime) Create(ctx context.Context, spec Spec) (string, error) {
	if spec.AgentID == "" {
		return "", fmt.Errorf("runtime: create: empty agent ID")
	}
	if spec.Image == "" {
		return "", fmt.Errorf("runtime: create: empty image")
	}
	cmd := spec.Cmd
	if len(cmd) == 0 {
		cmd = []string{"sleep", "infinity"}
	}
	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)

	cfg := &container.Config{
		Image:      spec.Image,
		Cmd:        cmd,
		Env:        env,
		User:       "agent",
		WorkingDir: "/work",
		OpenStdin:  true,
		// StdinOnce makes the daemon close the container's stdin when
		// the attached client's stdin ends, while still delivering all
		// output the process writes afterwards. Without it the daemon
		// treats client stdin EOF as a detach and drops the read side
		// too. The consequence — a detach ends the main process's
		// stdin, so it exits — matches the design: agent continuity is
		// session resumption, not process lifetime.
		StdinOnce: true,
		Labels: map[string]string{
			LabelAgentID:   spec.AgentID,
			LabelProjectID: spec.ProjectID,
		},
	}
	hostCfg := &container.HostConfig{
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeBind,
				Source: spec.WorktreePath,
				Target: "/work",
			},
			{
				Type:   mount.TypeVolume,
				Source: VolumeName(spec.AgentID),
				Target: "/home/agent/.claude",
			},
		},
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
	}
	resp, err := r.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, ContainerName(spec.AgentID))
	if err != nil {
		return "", fmt.Errorf("runtime: create container for agent %s: %w", spec.AgentID, err)
	}
	return resp.ID, nil
}

// Start starts a created container.
func (r *Runtime) Start(ctx context.Context, containerID string) error {
	if err := r.cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("runtime: start %s: %w", containerID, err)
	}
	return nil
}

// Stop stops a container, giving the main process timeoutSeconds to
// exit before it is killed.
func (r *Runtime) Stop(ctx context.Context, containerID string, timeoutSeconds int) error {
	if err := r.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeoutSeconds}); err != nil {
		return fmt.Errorf("runtime: stop %s: %w", containerID, err)
	}
	return nil
}

// Remove removes a container. force kills a running one first.
func (r *Runtime) Remove(ctx context.Context, containerID string, force bool) error {
	err := r.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: force})
	if err != nil {
		return fmt.Errorf("runtime: remove %s: %w", containerID, err)
	}
	return nil
}

// RemoveVolume removes an agent's .claude volume. Callers use this on
// agent retirement, not on ordinary container replacement.
func (r *Runtime) RemoveVolume(ctx context.Context, agentID string, force bool) error {
	if err := r.cli.VolumeRemove(ctx, VolumeName(agentID), force); err != nil {
		return fmt.Errorf("runtime: remove volume for agent %s: %w", agentID, err)
	}
	return nil
}

// State is the subset of container state labd cares about.
type State struct {
	Running  bool
	ExitCode int
}

// Inspect reports whether the container is running and, if not, its
// exit code.
func (r *Runtime) Inspect(ctx context.Context, containerID string) (State, error) {
	info, err := r.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return State{}, fmt.Errorf("runtime: inspect %s: %w", containerID, err)
	}
	st := State{}
	if info.State != nil {
		st.Running = info.State.Running
		st.ExitCode = info.State.ExitCode
	}
	return st, nil
}

// Container is one row of List.
type Container struct {
	ID        string
	Name      string
	AgentID   string
	ProjectID string
	State     string // Docker state string: created|running|exited|...
}

// List returns all lab agent containers (running or not), optionally
// narrowed to one project. labd restart reconciliation walks this.
func (r *Runtime) List(ctx context.Context, projectID string) ([]Container, error) {
	f := filters.NewArgs(filters.Arg("label", LabelAgentID))
	if projectID != "" {
		f.Add("label", LabelProjectID+"="+projectID)
	}
	summaries, err := r.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, fmt.Errorf("runtime: list containers: %w", err)
	}
	out := make([]Container, 0, len(summaries))
	for _, s := range summaries {
		c := Container{
			ID:        s.ID,
			AgentID:   s.Labels[LabelAgentID],
			ProjectID: s.Labels[LabelProjectID],
			State:     string(s.State),
		}
		if len(s.Names) > 0 {
			c.Name = s.Names[0]
		}
		out = append(out, c)
	}
	return out, nil
}
