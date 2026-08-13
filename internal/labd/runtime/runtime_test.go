package runtime

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
)

// Integration tests against a real Docker daemon; they skip when it is
// unreachable. Image builds use SkipClaudeInstall so tests never
// depend on the claude.ai installer, and every container/volume is
// cleaned up via t.Cleanup even on failure.

func TestStacks(t *testing.T) {
	got := Stacks()
	want := []string{"base", "go", "node", "python", "rust"}
	if !slices.Equal(got, want) {
		t.Fatalf("Stacks() = %v, want %v", got, want)
	}
	if !ValidStack("go") || ValidStack("cobol") {
		t.Fatal("ValidStack misclassifies")
	}
}

func TestContentHash(t *testing.T) {
	base := contentHash([]byte("FROM x"), map[string]string{"A": "1"}, "bin1")
	if h := contentHash([]byte("FROM x"), map[string]string{"A": "1"}, "bin1"); h != base {
		t.Error("hash not deterministic")
	}
	for name, h := range map[string]string{
		"dockerfile": contentHash([]byte("FROM y"), map[string]string{"A": "1"}, "bin1"),
		"arg value":  contentHash([]byte("FROM x"), map[string]string{"A": "2"}, "bin1"),
		"arg key":    contentHash([]byte("FROM x"), map[string]string{"B": "1"}, "bin1"),
		"extra":      contentHash([]byte("FROM x"), map[string]string{"A": "1"}, "bin2"),
	} {
		if h == base {
			t.Errorf("mutating %s did not change the hash", name)
		}
	}
}

// newTestRuntime returns a Runtime connected to the local daemon, or
// skips the test when Docker is unreachable.
func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	rt, err := New()
	if err != nil {
		t.Skipf("docker client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rt.Ping(ctx); err != nil {
		rt.Close()
		t.Skipf("docker daemon unreachable: %v", err)
	}
	t.Cleanup(func() { rt.Close() })
	return rt
}

// countingHandler counts "building image" log records so tests can
// assert laziness without depending on timing.
type countingHandler struct {
	slog.Handler
	mu     sync.Mutex
	builds int
}

func (h *countingHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "building image" {
		h.mu.Lock()
		h.builds++
		h.mu.Unlock()
	}
	return h.Handler.Handle(ctx, r)
}

func (h *countingHandler) buildCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.builds
}

func newTestBuilder(t *testing.T, rt *Runtime) (*Builder, *countingHandler) {
	t.Helper()
	h := &countingHandler{
		Handler: slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}),
	}
	b := NewBuilder(rt, BuilderOptions{
		SkipClaudeInstall: true,
		Logger:            slog.New(h),
	})
	return b, h
}

func ensureImage(t *testing.T, b *Builder, stack string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	tag, err := b.EnsureImage(ctx, stack)
	if err != nil {
		t.Fatalf("EnsureImage(%q): %v", stack, err)
	}
	return tag
}

func TestEnsureImageLazy(t *testing.T) {
	rt := newTestRuntime(t)
	b, h := newTestBuilder(t, rt)

	tag := ensureImage(t, b, "base")
	if !strings.HasPrefix(tag, "lab/agent-base:") {
		t.Fatalf("unexpected tag %q", tag)
	}
	first := h.buildCount()

	// Second call: image exists, so no build may happen.
	if again := ensureImage(t, b, "base"); again != tag {
		t.Fatalf("second EnsureImage returned %q, want %q", again, tag)
	}
	if h.buildCount() != first {
		t.Fatalf("second EnsureImage rebuilt: %d builds, want %d", h.buildCount(), first)
	}

	// Mutating a hash input (the build args) must produce a new tag.
	b2 := NewBuilder(rt, BuilderOptions{
		SkipClaudeInstall: false,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	// Compute the tag without building: mutate args through the same
	// path EnsureImage uses.
	if b2.skipClaudeInstall == b.skipClaudeInstall {
		t.Fatal("test setup: builders do not differ")
	}
	df, err := os.ReadFile(filepath.Join("..", "..", "..", "stacks", "base", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	h1 := contentHash(df, map[string]string{"SKIP_CLAUDE_INSTALL": "1"}, "bin")
	h2 := contentHash(df, map[string]string{}, "bin")
	if h1 == h2 {
		t.Fatal("arg mutation did not change the image hash")
	}
}

func TestEnsureImageUnknownStack(t *testing.T) {
	rt := newTestRuntime(t)
	b, _ := newTestBuilder(t, rt)
	if _, err := b.EnsureImage(context.Background(), "cobol"); err == nil {
		t.Fatal("EnsureImage accepted an unknown stack")
	}
}

// createAgent creates a container and registers cleanup of both the
// container and its .claude volume.
func createAgent(t *testing.T, rt *Runtime, spec Spec) string {
	t.Helper()
	ctx := context.Background()
	id, err := rt.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		rt.Remove(context.Background(), id, true)
		// The volume outlives the container; force-remove it too.
		rt.RemoveVolume(context.Background(), spec.AgentID, true)
	})
	return id
}

func TestContainerRoundTrip(t *testing.T) {
	rt := newTestRuntime(t)
	b, _ := newTestBuilder(t, rt)
	tag := ensureImage(t, b, "base")

	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "hello.txt"), []byte("from-worktree\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	agentID := fmt.Sprintf("test-rt-%d", os.Getpid())
	spec := Spec{
		AgentID:      agentID,
		ProjectID:    "test-project",
		Image:        tag,
		WorktreePath: worktree,
		Env:          map[string]string{"LAB_TEST_ENV": "roundtrip"},
		// Prove the mount is visible from inside, then echo stdin.
		Cmd: []string{"sh", "-c", "cat /work/hello.txt; cat"},
	}
	ctx := context.Background()
	id := createAgent(t, rt, spec)

	attachCtx, detach := context.WithCancel(ctx)
	defer detach()
	stdin, stdout, _, err := rt.Attach(attachCtx, id)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := rt.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}

	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	readLine := func(want string) {
		t.Helper()
		select {
		case got, ok := <-lines:
			if !ok {
				t.Fatalf("stdout closed, want %q", want)
			}
			if got != want {
				t.Fatalf("read %q, want %q", got, want)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}

	// The mounted worktree file, printed by the container on startup.
	readLine("from-worktree")

	// stdin → stdout round-trip through the demultiplexed attach.
	for _, msg := range []string{"ping-1", "ping-2"} {
		if _, err := io.WriteString(stdin, msg+"\n"); err != nil {
			t.Fatalf("write stdin: %v", err)
		}
		readLine(msg)
	}

	// Container metadata: env passed verbatim, labels, volume mount.
	info, err := rt.cli.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !slices.Contains(info.Config.Env, "LAB_TEST_ENV=roundtrip") {
		t.Errorf("env not passed through: %v", info.Config.Env)
	}
	if got := info.Config.Labels[LabelAgentID]; got != agentID {
		t.Errorf("agent label = %q, want %q", got, agentID)
	}
	if got := info.Config.Labels[LabelProjectID]; got != "test-project" {
		t.Errorf("project label = %q", got)
	}
	var haveVolume bool
	for _, m := range info.Mounts {
		if m.Name == VolumeName(agentID) && m.Destination == "/home/agent/.claude" {
			haveVolume = true
		}
	}
	if !haveVolume {
		t.Errorf(".claude volume not mounted: %+v", info.Mounts)
	}

	st, err := rt.Inspect(ctx, id)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !st.Running {
		t.Fatal("container not running")
	}

	// List finds it by label, both globally and per project.
	found := func(cs []Container) bool {
		for _, c := range cs {
			if c.AgentID == agentID && c.ID == id {
				return true
			}
		}
		return false
	}
	all, err := rt.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !found(all) {
		t.Error("List(all) did not find the container")
	}
	byProject, err := rt.List(ctx, "test-project")
	if err != nil {
		t.Fatalf("List(project): %v", err)
	}
	if !found(byProject) {
		t.Error("List(project) did not find the container")
	}
	byOther, err := rt.List(ctx, "other-project")
	if err != nil {
		t.Fatalf("List(other): %v", err)
	}
	if found(byOther) {
		t.Error("List(other-project) wrongly matched")
	}

	// Closing stdin must not kill the reads: output the container
	// writes afterwards still arrives, then the stream ends cleanly
	// when the container exits on stdin EOF.
	if _, err := io.WriteString(stdin, "after-close\n"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	readLine("after-close")
	select {
	case _, ok := <-lines:
		if ok {
			t.Fatal("unexpected extra stdout line")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("stdout did not reach EOF after container exit")
	}

	// Stop is a no-op on the already-exited container; verify state,
	// then remove.
	if err := rt.Stop(ctx, id, 1); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st, err = rt.Inspect(ctx, id)
	if err != nil {
		t.Fatalf("Inspect after stop: %v", err)
	}
	if st.Running {
		t.Fatal("container still running after Stop")
	}
	if err := rt.Remove(ctx, id, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := rt.Inspect(ctx, id); err == nil {
		t.Fatal("container still inspectable after Remove")
	}
}

func TestAttachSeparatesStderr(t *testing.T) {
	rt := newTestRuntime(t)
	b, _ := newTestBuilder(t, rt)
	tag := ensureImage(t, b, "base")

	agentID := fmt.Sprintf("test-demux-%d", os.Getpid())
	id := createAgent(t, rt, Spec{
		AgentID:      agentID,
		ProjectID:    "test-project",
		Image:        tag,
		WorktreePath: t.TempDir(),
		Cmd:          []string{"sh", "-c", "echo to-out; echo to-err >&2"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, stdout, stderr, err := rt.Attach(ctx, id)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := rt.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The container exits after both writes and the demuxed streams
	// end. Drain concurrently, per the Attach contract.
	errCh := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(stderr)
		errCh <- b
	}()
	outB, err := io.ReadAll(stdout)
	if err != nil && err != io.EOF {
		t.Fatalf("read stdout: %v", err)
	}
	errB := <-errCh
	if got := strings.TrimSpace(string(outB)); got != "to-out" {
		t.Errorf("stdout = %q, want to-out", got)
	}
	if got := strings.TrimSpace(string(errB)); got != "to-err" {
		t.Errorf("stderr = %q, want to-err", got)
	}
}

// TestAllStacksFull builds every stack image with the real Claude Code
// install layer (network-heavy: toolchain tarballs plus the claude.ai
// installer), then verifies claude, kbase, and lab-agent respond inside
// the base image. Gated behind an env var; run it manually when
// touching the Dockerfiles:
//
//	LAB_TEST_BUILD_ALL_STACKS=1 go test ./internal/labd/runtime/ -run TestAllStacksFull -timeout 30m
func TestAllStacksFull(t *testing.T) {
	if os.Getenv("LAB_TEST_BUILD_ALL_STACKS") == "" {
		t.Skip("set LAB_TEST_BUILD_ALL_STACKS=1 to run full image builds")
	}
	rt := newTestRuntime(t)
	b := NewBuilder(rt, BuilderOptions{}) // no SkipClaudeInstall: the real thing

	tags := map[string]string{}
	for _, stack := range Stacks() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		tag, err := b.EnsureImage(ctx, stack)
		cancel()
		if err != nil {
			t.Fatalf("EnsureImage(%q): %v", stack, err)
		}
		tags[stack] = tag
		t.Logf("built %s", tag)
	}

	// Inside the base image, the three installed CLIs must answer.
	agentID := fmt.Sprintf("test-full-%d", os.Getpid())
	id := createAgent(t, rt, Spec{
		AgentID:      agentID,
		ProjectID:    "test-project",
		Image:        tags["base"],
		WorktreePath: t.TempDir(),
		Cmd:          []string{"sh", "-c", "claude --version && kbase && lab-agent"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, stdout, stderr, err := rt.Attach(ctx, id)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := rt.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	errCh := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(stderr)
		errCh <- b
	}()
	out, _ := io.ReadAll(stdout)
	t.Logf("stdout:\n%s\nstderr:\n%s", out, <-errCh)
	st, err := rt.Inspect(ctx, id)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if st.Running || st.ExitCode != 0 {
		t.Fatalf("CLI check exited %d (running=%v)", st.ExitCode, st.Running)
	}
	for _, want := range []string{"Claude Code", "kbase", "lab-agent"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("stdout missing %q", want)
		}
	}
}

func TestGoStack(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: skipping go stack image build")
	}
	rt := newTestRuntime(t)
	b, _ := newTestBuilder(t, rt)
	tag := ensureImage(t, b, "go")
	if !strings.HasPrefix(tag, "lab/agent-go:") {
		t.Fatalf("unexpected tag %q", tag)
	}

	agentID := fmt.Sprintf("test-go-%d", os.Getpid())
	id := createAgent(t, rt, Spec{
		AgentID:      agentID,
		ProjectID:    "test-project",
		Image:        tag,
		WorktreePath: t.TempDir(),
		Cmd:          []string{"go", "version"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := rt.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitCh, errCh := rt.cli.ContainerWait(ctx, id, container.WaitConditionNotRunning)
	select {
	case res := <-waitCh:
		if res.StatusCode != 0 {
			t.Fatalf("go version exited %d", res.StatusCode)
		}
	case err := <-errCh:
		t.Fatalf("wait: %v", err)
	}
}
