package gitrepo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// BranchName returns the branch an agent works on: agent/<agentName>.
// Agent names are unique per project, so the mapping is stable.
func BranchName(agentName string) string {
	return "agent/" + agentName
}

// AddWorktree ensures branch agent/<agentName> exists (created from the
// default branch if not) and that a worktree for agentID is checked out
// on it at the contract path, returning that path (absolute).
//
// It is idempotent: an existing healthy worktree is returned as-is; a
// broken one (registered but missing, or unregistered leftovers) is
// cleaned up and recreated.
func (m *Manager) AddWorktree(ctx context.Context, projectID, agentID, agentName string) (string, error) {
	lock := m.projectLock(projectID)
	lock.Lock()
	defer lock.Unlock()

	repo := m.repoDir(projectID)
	path, err := filepath.Abs(m.worktreeDir(projectID, agentID))
	if err != nil {
		return "", fmt.Errorf("gitrepo: %w", err)
	}
	branch := BranchName(agentName)

	if healthy, registered := worktreeState(ctx, repo, path); healthy {
		return path, nil
	} else if registered || dirExists(path) {
		// Broken: registered without a directory, or a stale directory
		// git no longer knows about. Clear both and recreate.
		run(ctx, repo, "worktree", "remove", "--force", path)
		run(ctx, repo, "worktree", "prune")
		if err := os.RemoveAll(path); err != nil {
			return "", fmt.Errorf("gitrepo: %w", err)
		}
	}

	if _, err := run(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
		def, err := m.DefaultBranch(ctx, projectID)
		if err != nil {
			return "", err
		}
		if _, err := run(ctx, repo, "branch", branch, def); err != nil {
			return "", err
		}
	}
	if _, err := run(ctx, repo, "worktree", "add", path, branch); err != nil {
		return "", err
	}
	return path, nil
}

// worktreeState reports whether the worktree at path is registered on
// repo and, if so, whether it is healthy (its checkout actually works).
func worktreeState(ctx context.Context, repo, path string) (healthy, registered bool) {
	for _, wt := range listWorktrees(ctx, repo) {
		// git reports symlink-resolved paths (e.g. /private/var vs
		// /var on macOS); compare canonical forms.
		if canonical(wt) == canonical(path) {
			registered = true
			break
		}
	}
	if !registered {
		return false, false
	}
	_, err := run(ctx, path, "rev-parse", "--is-inside-work-tree")
	return err == nil, true
}

func canonical(path string) string {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	return path
}

func dirExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// RemoveWorktree removes an agent's worktree (with --force when force is
// set, discarding uncommitted changes) and optionally deletes its
// branch. Removing a worktree that does not exist is not an error.
func (m *Manager) RemoveWorktree(ctx context.Context, projectID, agentID string, force, deleteBranch bool) error {
	lock := m.projectLock(projectID)
	lock.Lock()
	defer lock.Unlock()

	repo := m.repoDir(projectID)
	path, err := filepath.Abs(m.worktreeDir(projectID, agentID))
	if err != nil {
		return fmt.Errorf("gitrepo: %w", err)
	}

	_, registered := worktreeState(ctx, repo, path)
	if !registered && !dirExists(path) {
		return nil
	}

	branch := ""
	if deleteBranch {
		// The branch is recorded in the worktree's HEAD; read it before
		// the worktree goes away.
		branch, _ = run(ctx, path, "symbolic-ref", "--short", "HEAD")
	}

	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	if _, err := run(ctx, repo, append(args, path)...); err != nil {
		return err
	}
	if deleteBranch && branch != "" {
		if _, err := run(ctx, repo, "branch", "-D", branch); err != nil {
			return err
		}
	}
	return nil
}

// Status describes an agent worktree for UI purposes.
type Status struct {
	// Branch is the checked-out branch (agent/<name>).
	Branch string
	// DirtyFiles are paths with uncommitted changes (porcelain lines,
	// status code stripped), empty for a clean tree.
	DirtyFiles []string
	// Ahead and Behind count commits relative to the project's default
	// branch.
	Ahead, Behind int
}

// Status reports the porcelain status of an agent's worktree and how far
// its branch has diverged from the project's default branch.
func (m *Manager) Status(ctx context.Context, projectID, agentID string) (Status, error) {
	lock := m.projectLock(projectID)
	lock.Lock()
	defer lock.Unlock()

	var st Status
	path := m.worktreeDir(projectID, agentID)

	branch, err := run(ctx, path, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return st, err
	}
	st.Branch = branch

	out, err := run(ctx, path, "status", "--porcelain")
	if err != nil {
		return st, err
	}
	for _, line := range strings.Split(out, "\n") {
		if len(line) > 3 {
			st.DirtyFiles = append(st.DirtyFiles, line[3:])
		}
	}

	def, err := m.DefaultBranch(ctx, projectID)
	if err != nil {
		return st, err
	}
	counts, err := run(ctx, path, "rev-list", "--left-right", "--count", def+"...HEAD")
	if err != nil {
		return st, err
	}
	fields := strings.Fields(counts)
	if len(fields) != 2 {
		return st, fmt.Errorf("gitrepo: unexpected rev-list output %q", counts)
	}
	if st.Behind, err = strconv.Atoi(fields[0]); err != nil {
		return st, fmt.Errorf("gitrepo: parse rev-list output %q: %w", counts, err)
	}
	if st.Ahead, err = strconv.Atoi(fields[1]); err != nil {
		return st, fmt.Errorf("gitrepo: parse rev-list output %q: %w", counts, err)
	}
	return st, nil
}
