// Package gitrepo manages per-project git state under labd's data dir:
// one bare clone per project plus one worktree (on its own branch) per
// agent. It shells out to the git binary (a documented decision — go-git
// worktree support is not worth the risk) and never depends on the
// process working directory: every command runs with an explicit -C.
//
// Layout under the data-dir root:
//
//	<root>/projects/<project-id>/repo.git              bare repo
//	<root>/projects/<project-id>/worktrees/<agent-id>/ per-agent worktree
package gitrepo

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Origin kinds accepted by CreateProject. The values match the
// lab.projects.origin_kind column so callers can pass store values
// through unchanged.
const (
	OriginKindGitURL    = "git_url"
	OriginKindLocalPath = "local_path"
)

// Manager owns the git state for all projects under a single data-dir
// root. It is safe for concurrent use; operations on the same project
// are serialized with a per-project mutex so parallel agent operations
// never surface git's internal locking as user-visible failures.
type Manager struct {
	root string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewManager returns a Manager rooted at dataDir (projects live under
// <dataDir>/projects). The directory is created lazily.
func NewManager(dataDir string) *Manager {
	return &Manager{root: dataDir, locks: make(map[string]*sync.Mutex)}
}

// projectLock returns the mutex serializing operations on projectID.
func (m *Manager) projectLock(projectID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[projectID]
	if !ok {
		l = &sync.Mutex{}
		m.locks[projectID] = l
	}
	return l
}

// projectDir returns <root>/projects/<projectID>.
func (m *Manager) projectDir(projectID string) string {
	return filepath.Join(m.root, "projects", projectID)
}

// repoDir returns the bare repo path for a project.
func (m *Manager) repoDir(projectID string) string {
	return filepath.Join(m.projectDir(projectID), "repo.git")
}

// worktreeDir returns the worktree path for an agent.
func (m *Manager) worktreeDir(projectID, agentID string) string {
	return filepath.Join(m.projectDir(projectID), "worktrees", agentID)
}

// run executes git with an explicit -C dir, returning trimmed stdout.
// Errors carry the git subcommand and a tail of stderr. The environment
// pins a deterministic lab identity so commits and merges work without
// any host git config, and disables terminal credential prompts.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=lab",
		"GIT_AUTHOR_EMAIL=lab@localhost",
		"GIT_COMMITTER_NAME=lab",
		"GIT_COMMITTER_EMAIL=lab@localhost",
		"GIT_TERMINAL_PROMPT=0",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, stderrTail(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// stderrTail keeps error messages bounded while preserving the part of
// git's stderr that actually explains the failure (the last lines).
func stderrTail(s string) string {
	s = strings.TrimSpace(s)
	const max = 500
	if len(s) > max {
		s = "..." + s[len(s)-max:]
	}
	return s
}

// CreateProject materializes the bare repo for a project.
//
// For OriginKindGitURL the origin is cloned bare. For OriginKindLocalPath
// the folder is first turned into a git repo if it is not one (git init
// plus an initial commit of its contents, authored by lab), then cloned
// bare; either way the local path remains the bare repo's origin remote.
func (m *Manager) CreateProject(ctx context.Context, projectID, originKind, origin string) error {
	lock := m.projectLock(projectID)
	lock.Lock()
	defer lock.Unlock()

	repo := m.repoDir(projectID)
	if _, err := os.Stat(repo); err == nil {
		return fmt.Errorf("gitrepo: project %s already exists", projectID)
	}

	switch originKind {
	case OriginKindGitURL:
		// Nothing to prepare; clone below.
	case OriginKindLocalPath:
		if err := ensureLocalRepo(ctx, origin); err != nil {
			return err
		}
	default:
		return fmt.Errorf("gitrepo: unknown origin kind %q", originKind)
	}

	dir := m.projectDir(projectID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("gitrepo: %w", err)
	}
	if _, err := run(ctx, dir, "clone", "--bare", origin, repo); err != nil {
		// Leave no half-created project behind.
		os.RemoveAll(dir)
		return err
	}
	// Bare clones get no fetch refspec by default; configure one so
	// Fetch tracks the origin's branches under refs/remotes/origin/*
	// without clobbering local agent/* branches.
	if _, err := run(ctx, repo, "config", "remote.origin.fetch",
		"+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return err
	}
	return nil
}

// ensureLocalRepo makes sure path is a git repository, initializing it
// with an initial commit of its contents when it is not.
func ensureLocalRepo(ctx context.Context, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("gitrepo: local origin: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("gitrepo: local origin %s is not a directory", path)
	}
	if _, err := run(ctx, path, "rev-parse", "--git-dir"); err == nil {
		return nil // already a repo
	}
	if _, err := run(ctx, path, "init", "-b", "main"); err != nil {
		return err
	}
	if _, err := run(ctx, path, "add", "-A"); err != nil {
		return err
	}
	// --allow-empty keeps an empty folder usable as an origin.
	if _, err := run(ctx, path, "commit", "--allow-empty", "-m", "initial import by lab"); err != nil {
		return err
	}
	return nil
}

// DefaultBranch reports the project's default branch, detected from the
// bare repo's HEAD (set by clone from the origin's HEAD).
func (m *Manager) DefaultBranch(ctx context.Context, projectID string) (string, error) {
	return run(ctx, m.repoDir(projectID), "symbolic-ref", "--short", "HEAD")
}

// RemoveProject deletes all project state: each worktree via git
// worktree remove --force, then the whole project directory.
func (m *Manager) RemoveProject(ctx context.Context, projectID string) error {
	lock := m.projectLock(projectID)
	lock.Lock()
	defer lock.Unlock()

	dir := m.projectDir(projectID)
	repo := m.repoDir(projectID)
	if _, err := os.Stat(repo); err == nil {
		for _, wt := range listWorktrees(ctx, repo) {
			// Best effort: a broken worktree must not block removal.
			run(ctx, repo, "worktree", "remove", "--force", wt)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("gitrepo: remove project %s: %w", projectID, err)
	}
	return nil
}

// listWorktrees returns the non-bare worktree paths registered on repo.
func listWorktrees(ctx context.Context, repo string) []string {
	out, err := run(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil
	}
	var paths []string
	entries := strings.Split(out, "\n\n")
	for _, e := range entries {
		var path string
		bare := false
		for _, line := range strings.Split(e, "\n") {
			if p, ok := strings.CutPrefix(line, "worktree "); ok {
				path = p
			}
			if line == "bare" {
				bare = true
			}
		}
		if path != "" && !bare {
			paths = append(paths, path)
		}
	}
	return paths
}

// Fetch updates the bare repo from its origin remote (the git URL or the
// local folder — git treats both the same). Fetched branches land under
// refs/remotes/origin/*; integrating them is a later phase's concern.
func (m *Manager) Fetch(ctx context.Context, projectID string) error {
	lock := m.projectLock(projectID)
	lock.Lock()
	defer lock.Unlock()

	_, err := run(ctx, m.repoDir(projectID), "fetch", "--prune", "origin")
	return err
}
