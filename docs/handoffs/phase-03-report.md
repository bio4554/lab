# Phase 3 report — gitrepo: projects & worktrees

Branch `phase-03-gitrepo`, off `development`. `make check` green;
gitrepo tests also run clean with `-race -count=2`.

## What was built

`internal/labd/gitrepo`, three files plus tests:

- `gitrepo.go` — `Manager` (created via `NewManager(dataDir)`), the
  shared `run(ctx, dir, args...)` helper (explicit `-C` on every git
  invocation, pinned `lab <lab@localhost>` author/committer env,
  `GIT_TERMINAL_PROMPT=0`, errors wrap the git subcommand plus a
  500-byte stderr tail), per-project mutexes, `CreateProject`,
  `DefaultBranch`, `RemoveProject`, `Fetch`.
- `worktree.go` — `AddWorktree` (idempotent; heals broken/stale
  worktrees), `RemoveWorktree(force, deleteBranch)`, `Status`
  (branch, dirty files, ahead/behind vs. default branch), and the
  exported `BranchName(agentName) = "agent/<agentName>"` helper.
- `merge.go` — `Merge` in a temporary worktree under the project dir,
  returning the merge commit hash; on conflict it aborts, removes the
  temp worktree, and returns `*MergeConflictError` with the conflicted
  file list. No auto-resolution.

Layout matches the contract exactly:
`<data_dir>/projects/<id>/repo.git` and
`<data_dir>/projects/<id>/worktrees/<agent-id>/`. `AddWorktree` returns
the absolute worktree path for Phase 4 to bind-mount.

Tests (`gitrepo_test.go`) are integration tests against real git in
`t.TempDir()`, no network — the `git_url` case clones from a local
fixture repo. Coverage per the handoff: both origin kinds end-to-end;
non-repo `local_path` init with files surviving into the worktree (and
the initial import authored by lab); two agents doing parallel
`AddWorktree` + commits under `-race` then merging cleanly in sequence;
conflicting merge (typed error, right file list, repo left clean, later
clean merge works); idempotent `AddWorktree` (uncommitted files
survive); `RemoveWorktree` (dirty tree requires force; branch deletion;
double-remove is a no-op); `Status` on a dirty/ahead/behind tree;
`RemoveProject` then re-create.

## Decisions & deviations (smallest-call gaps)

- **Fetch refspec**: `git clone --bare` configures no fetch refspec, so
  `CreateProject` sets `remote.origin.fetch =
  +refs/heads/*:refs/remotes/origin/*`. `Fetch` therefore updates
  `refs/remotes/origin/*` (with `--prune`) and never touches local
  `refs/heads/*` — fetching directly into heads could clobber or
  collide with `agent/*` branches. Integrating fetched changes into the
  default branch is left to the merge-policy phase (11).
- **`RemoveWorktree` signature**: the handoff said "force flag
  parameter, delete the branch optionally" — implemented as
  `RemoveWorktree(ctx, projectID, agentID, force, deleteBranch bool)`.
  The branch to delete is read from the worktree's HEAD before removal
  (the method doesn't receive the agent name).
- **`local_path` init uses `git init -b main`** so the default branch
  is deterministic regardless of host git config. Existing repos keep
  whatever default branch they have (detected from the clone's HEAD).
- **Merge commit message** is `merge <source> into <default>`; git may
  fast-forward when possible, in which case the returned hash is the
  fast-forwarded head rather than a two-parent merge commit.
- **macOS symlinked paths**: git reports symlink-resolved worktree
  paths (`/private/var` vs `/var`), so worktree registration checks
  compare `filepath.EvalSymlinks`-canonicalized paths.
- **Non-conflict merge failures** (unknown branch, unrelated
  histories) surface as the plain git error, not `MergeConflictError`.

## Notes for the orchestrator

- **README**: per the handoff I left README.md untouched — please add
  `git` to the dev-prerequisites line.
- **Limitation (per handoff, post-v1)**: no auth for private remotes;
  `git_url` origins are assumed public or local. Credential prompts are
  hard-disabled (`GIT_TERMINAL_PROMPT=0`), so a private URL fails fast
  instead of hanging.
- Empty-repo origins (no commits) fail naturally at clone/worktree
  time with a git error; nothing special was built for them.

## Open questions

None blocking. Phase 5 may want a `Manager` method to resolve a
worktree path without creating it (`AddWorktree` is idempotent and
cheap, so it can serve that purpose for now).
