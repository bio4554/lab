package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Integration tests against the real git binary (a dev prerequisite),
// entirely in temp dirs and with no network: the git_url case clones
// from a local fixture repo.

// newFixtureRepo creates a normal git repo with one commit and returns
// its path. It serves as the "remote" for git_url-kind projects.
func newFixtureRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	mustRun(t, ctx, dir, "init", "-b", "main")
	for name, content := range files {
		writeFile(t, filepath.Join(dir, name), content)
	}
	mustRun(t, ctx, dir, "add", "-A")
	mustRun(t, ctx, dir, "commit", "--allow-empty", "-m", "fixture")
	return dir
}

func mustRun(t *testing.T, ctx context.Context, dir string, args ...string) string {
	t.Helper()
	out, err := run(ctx, dir, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// commitInWorktree writes a file and commits it in the given worktree.
func commitInWorktree(t *testing.T, ctx context.Context, wt, name, content, msg string) {
	t.Helper()
	writeFile(t, filepath.Join(wt, name), content)
	mustRun(t, ctx, wt, "add", "-A")
	mustRun(t, ctx, wt, "commit", "-m", msg)
}

func TestCreateProjectGitURL(t *testing.T) {
	ctx := context.Background()
	fixture := newFixtureRepo(t, map[string]string{"hello.txt": "hi\n"})
	m := NewManager(t.TempDir())

	if err := m.CreateProject(ctx, "p1", OriginKindGitURL, fixture); err != nil {
		t.Fatal(err)
	}
	def, err := m.DefaultBranch(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if def != "main" {
		t.Fatalf("default branch = %q, want main", def)
	}

	wt, err := m.AddWorktree(ctx, "p1", "a1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(m.projectDir("p1"), "worktrees", "a1")
	if abs, _ := filepath.Abs(want); wt != abs {
		t.Fatalf("worktree path = %q, want %q", wt, abs)
	}
	if _, err := os.Stat(filepath.Join(wt, "hello.txt")); err != nil {
		t.Fatalf("fixture file missing from worktree: %v", err)
	}
	if branch := mustRun(t, ctx, wt, "symbolic-ref", "--short", "HEAD"); branch != "agent/alice" {
		t.Fatalf("worktree branch = %q, want agent/alice", branch)
	}
}

func TestCreateProjectLocalPathExistingRepo(t *testing.T) {
	ctx := context.Background()
	fixture := newFixtureRepo(t, map[string]string{"a.txt": "a\n"})
	m := NewManager(t.TempDir())

	if err := m.CreateProject(ctx, "p1", OriginKindLocalPath, fixture); err != nil {
		t.Fatal(err)
	}
	if def := mustDefaultBranch(t, ctx, m, "p1"); def != "main" {
		t.Fatalf("default branch = %q, want main", def)
	}
	// The local path stays the origin remote.
	origin := mustRun(t, ctx, m.repoDir("p1"), "remote", "get-url", "origin")
	if origin != fixture {
		t.Fatalf("origin = %q, want %q", origin, fixture)
	}
	if err := m.Fetch(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
}

func TestCreateProjectLocalPathNonRepo(t *testing.T) {
	ctx := context.Background()
	folder := t.TempDir()
	writeFile(t, filepath.Join(folder, "notes.md"), "keep me\n")
	writeFile(t, filepath.Join(folder, "sub", "deep.txt"), "deep\n")
	m := NewManager(t.TempDir())

	if err := m.CreateProject(ctx, "p1", OriginKindLocalPath, folder); err != nil {
		t.Fatal(err)
	}
	wt, err := m.AddWorktree(ctx, "p1", "a1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"notes.md", filepath.Join("sub", "deep.txt")} {
		if _, err := os.Stat(filepath.Join(wt, name)); err != nil {
			t.Fatalf("file %s did not survive into worktree: %v", name, err)
		}
	}
	// The initial import is authored by lab.
	author := mustRun(t, ctx, wt, "log", "-1", "--format=%an <%ae>")
	if author != "lab <lab@localhost>" {
		t.Fatalf("initial commit author = %q", author)
	}
}

func TestParallelWorktreesAndSequentialMerges(t *testing.T) {
	ctx := context.Background()
	fixture := newFixtureRepo(t, map[string]string{"base.txt": "base\n"})
	m := NewManager(t.TempDir())
	if err := m.CreateProject(ctx, "p1", OriginKindGitURL, fixture); err != nil {
		t.Fatal(err)
	}

	agents := []struct{ id, name, file string }{
		{"a1", "alice", "alice.txt"},
		{"a2", "bob", "bob.txt"},
	}
	var wg sync.WaitGroup
	errs := make([]error, len(agents))
	for i, ag := range agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// No t.Fatal in goroutines: collect errors instead.
			wt, err := m.AddWorktree(ctx, "p1", ag.id, ag.name)
			if err != nil {
				errs[i] = err
				return
			}
			if err := os.WriteFile(filepath.Join(wt, ag.file), []byte(ag.name+" was here\n"), 0o644); err != nil {
				errs[i] = err
				return
			}
			if _, err := run(ctx, wt, "add", "-A"); err != nil {
				errs[i] = err
				return
			}
			if _, err := run(ctx, wt, "commit", "-m", "add "+ag.file); err != nil {
				errs[i] = err
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	for _, ag := range agents {
		hash, err := m.Merge(ctx, "p1", BranchName(ag.name))
		if err != nil {
			t.Fatalf("merge %s: %v", ag.name, err)
		}
		if hash == "" {
			t.Fatalf("merge %s returned empty hash", ag.name)
		}
	}
	// Both files are on the default branch afterwards.
	files := mustRun(t, ctx, m.repoDir("p1"), "ls-tree", "--name-only", "main")
	for _, want := range []string{"alice.txt", "bob.txt"} {
		if !containsLine(files, want) {
			t.Fatalf("main is missing %s after merges; ls-tree:\n%s", want, files)
		}
	}
}

func containsLine(out, want string) bool {
	for line := range splitLines(out) {
		if line == want {
			return true
		}
	}
	return false
}

func splitLines(s string) map[string]struct{} {
	set := make(map[string]struct{})
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			set[s[start:i]] = struct{}{}
			start = i + 1
		}
	}
	return set
}

func TestMergeConflict(t *testing.T) {
	ctx := context.Background()
	fixture := newFixtureRepo(t, map[string]string{"shared.txt": "original\n"})
	m := NewManager(t.TempDir())
	if err := m.CreateProject(ctx, "p1", OriginKindGitURL, fixture); err != nil {
		t.Fatal(err)
	}

	wt1, err := m.AddWorktree(ctx, "p1", "a1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	wt2, err := m.AddWorktree(ctx, "p1", "a2", "bob")
	if err != nil {
		t.Fatal(err)
	}
	commitInWorktree(t, ctx, wt1, "shared.txt", "alice version\n", "alice edit")
	commitInWorktree(t, ctx, wt2, "shared.txt", "bob version\n", "bob edit")

	if _, err := m.Merge(ctx, "p1", BranchName("alice")); err != nil {
		t.Fatalf("first merge should be clean: %v", err)
	}

	_, err = m.Merge(ctx, "p1", BranchName("bob"))
	var conflict *MergeConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want MergeConflictError, got %v", err)
	}
	if len(conflict.Files) != 1 || conflict.Files[0] != "shared.txt" {
		t.Fatalf("conflicted files = %v, want [shared.txt]", conflict.Files)
	}

	// Everything is left clean: no temp worktrees linger and a
	// subsequent clean merge still works.
	for _, wt := range listWorktrees(ctx, m.repoDir("p1")) {
		if base := filepath.Base(wt); base != "a1" && base != "a2" {
			t.Fatalf("leftover worktree %s after aborted merge", wt)
		}
	}
	wt3, err := m.AddWorktree(ctx, "p1", "a3", "carol")
	if err != nil {
		t.Fatal(err)
	}
	commitInWorktree(t, ctx, wt3, "carol.txt", "no conflict\n", "carol edit")
	if _, err := m.Merge(ctx, "p1", BranchName("carol")); err != nil {
		t.Fatalf("clean merge after conflict failed: %v", err)
	}
}

func TestAddWorktreeIdempotent(t *testing.T) {
	ctx := context.Background()
	fixture := newFixtureRepo(t, map[string]string{"a.txt": "a\n"})
	m := NewManager(t.TempDir())
	if err := m.CreateProject(ctx, "p1", OriginKindGitURL, fixture); err != nil {
		t.Fatal(err)
	}

	wt1, err := m.AddWorktree(ctx, "p1", "a1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	// A dirty file must survive the second call untouched.
	writeFile(t, filepath.Join(wt1, "wip.txt"), "uncommitted\n")

	wt2, err := m.AddWorktree(ctx, "p1", "a1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if wt1 != wt2 {
		t.Fatalf("paths differ: %q vs %q", wt1, wt2)
	}
	if _, err := os.Stat(filepath.Join(wt2, "wip.txt")); err != nil {
		t.Fatalf("idempotent AddWorktree lost uncommitted file: %v", err)
	}
}

func TestRemoveWorktree(t *testing.T) {
	ctx := context.Background()
	fixture := newFixtureRepo(t, map[string]string{"a.txt": "a\n"})
	m := NewManager(t.TempDir())
	if err := m.CreateProject(ctx, "p1", OriginKindGitURL, fixture); err != nil {
		t.Fatal(err)
	}
	wt, err := m.AddWorktree(ctx, "p1", "a1", "alice")
	if err != nil {
		t.Fatal(err)
	}

	// A dirty tree needs force.
	writeFile(t, filepath.Join(wt, "a.txt"), "dirty\n")
	if err := m.RemoveWorktree(ctx, "p1", "a1", false, false); err == nil {
		t.Fatal("removing a dirty worktree without force should fail")
	}
	if err := m.RemoveWorktree(ctx, "p1", "a1", true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still exists: %v", err)
	}
	// Branch was deleted too.
	if _, err := run(ctx, m.repoDir("p1"), "rev-parse", "--verify", "--quiet", "refs/heads/agent/alice"); err == nil {
		t.Fatal("branch agent/alice should have been deleted")
	}
	// Removing again is a no-op.
	if err := m.RemoveWorktree(ctx, "p1", "a1", true, true); err != nil {
		t.Fatal(err)
	}
}

func TestStatusDirtyAheadBehind(t *testing.T) {
	ctx := context.Background()
	fixture := newFixtureRepo(t, map[string]string{"a.txt": "a\n"})
	m := NewManager(t.TempDir())
	if err := m.CreateProject(ctx, "p1", OriginKindGitURL, fixture); err != nil {
		t.Fatal(err)
	}
	wt, err := m.AddWorktree(ctx, "p1", "a1", "alice")
	if err != nil {
		t.Fatal(err)
	}

	commitInWorktree(t, ctx, wt, "b.txt", "b\n", "ahead commit")
	writeFile(t, filepath.Join(wt, "dirty.txt"), "wip\n")

	st, err := m.Status(ctx, "p1", "a1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Branch != "agent/alice" {
		t.Fatalf("branch = %q", st.Branch)
	}
	if st.Ahead != 1 || st.Behind != 0 {
		t.Fatalf("ahead/behind = %d/%d, want 1/0", st.Ahead, st.Behind)
	}
	if len(st.DirtyFiles) != 1 || st.DirtyFiles[0] != "dirty.txt" {
		t.Fatalf("dirty files = %v, want [dirty.txt]", st.DirtyFiles)
	}

	// Advance the default branch via another agent's merge; alice is
	// now behind.
	wt2, err := m.AddWorktree(ctx, "p1", "a2", "bob")
	if err != nil {
		t.Fatal(err)
	}
	commitInWorktree(t, ctx, wt2, "c.txt", "c\n", "bob commit")
	if _, err := m.Merge(ctx, "p1", BranchName("bob")); err != nil {
		t.Fatal(err)
	}
	st, err = m.Status(ctx, "p1", "a1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Behind != 1 {
		t.Fatalf("behind = %d, want 1", st.Behind)
	}
}

func TestRemoveProject(t *testing.T) {
	ctx := context.Background()
	fixture := newFixtureRepo(t, map[string]string{"a.txt": "a\n"})
	m := NewManager(t.TempDir())
	if err := m.CreateProject(ctx, "p1", OriginKindGitURL, fixture); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddWorktree(ctx, "p1", "a1", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveProject(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.projectDir("p1")); !os.IsNotExist(err) {
		t.Fatalf("project dir still exists: %v", err)
	}
	// The project can be recreated from scratch.
	if err := m.CreateProject(ctx, "p1", OriginKindGitURL, fixture); err != nil {
		t.Fatal(err)
	}
}

func mustDefaultBranch(t *testing.T, ctx context.Context, m *Manager, projectID string) string {
	t.Helper()
	def, err := m.DefaultBranch(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	return def
}
