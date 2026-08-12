package gitrepo

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// MergeConflictError reports a merge that could not complete cleanly.
// The merge is aborted before this is returned; the repo and worktrees
// are left exactly as they were.
type MergeConflictError struct {
	// SourceBranch is the branch that failed to merge.
	SourceBranch string
	// Files are the paths that conflicted.
	Files []string
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("gitrepo: merging %s conflicts in: %s",
		e.SourceBranch, strings.Join(e.Files, ", "))
}

// Merge merges sourceBranch (typically agent/<name>) into the project's
// default branch and returns the resulting commit hash. The merge runs
// in a temporary worktree — never in an agent's worktree — and no
// conflict is ever auto-resolved: on conflict the merge is aborted,
// everything is left clean, and a *MergeConflictError lists the
// conflicted files.
func (m *Manager) Merge(ctx context.Context, projectID, sourceBranch string) (string, error) {
	lock := m.projectLock(projectID)
	lock.Lock()
	defer lock.Unlock()

	repo := m.repoDir(projectID)
	def, err := m.DefaultBranch(ctx, projectID)
	if err != nil {
		return "", err
	}

	// Reserve a temp path inside the project dir, then hand it to git
	// (worktree add wants to create the directory itself).
	tmp, err := os.MkdirTemp(m.projectDir(projectID), "merge-")
	if err != nil {
		return "", fmt.Errorf("gitrepo: %w", err)
	}
	os.Remove(tmp)
	if _, err := run(ctx, repo, "worktree", "add", tmp, def); err != nil {
		return "", err
	}
	defer func() {
		run(ctx, repo, "worktree", "remove", "--force", tmp)
		os.RemoveAll(tmp)
	}()

	if _, err := run(ctx, tmp, "merge", "--no-edit",
		"-m", fmt.Sprintf("merge %s into %s", sourceBranch, def), sourceBranch); err != nil {
		conflicts, listErr := run(ctx, tmp, "diff", "--name-only", "--diff-filter=U")
		run(ctx, tmp, "merge", "--abort")
		if listErr != nil || conflicts == "" {
			// Not a conflict (unknown branch, unrelated histories, ...):
			// surface the original git error.
			return "", err
		}
		return "", &MergeConflictError{
			SourceBranch: sourceBranch,
			Files:        strings.Split(conflicts, "\n"),
		}
	}

	return run(ctx, tmp, "rev-parse", "HEAD")
}
