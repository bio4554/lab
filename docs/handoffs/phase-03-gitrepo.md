# Phase 3 handoff — gitrepo: projects & worktrees

You are the implementation agent for Phase 3 of the `lab` project. This
document is your complete brief. Read `docs/DESIGN.md` first —
especially "Projects & git" and "Agent runtime". `docs/PLAN.md` shows
where this phase fits.

Your job: `internal/labd/gitrepo` — project repository management.
Phase 5 wires it to real agents; Phase 4 (parallel) mounts the worktree
paths you produce into containers. You are the only phase touching
`internal/labd/gitrepo`.

**Decision already made** (PLAN.md): shell out to the `git` binary
(`exec.Command`), do not use go-git — its worktree support isn't worth
the risk. Always pass an explicit `-C <dir>` (or equivalent); never
depend on process CWD.

## Ground rules

- Branch `phase-03-gitrepo` off `development`.
- Stay in scope; smallest reasonable call on gaps, recorded in your
  report — no redesign.
- `docs/DESIGN.md` and `docs/PLAN.md` are read-only.
- `make check` green when you stop. Tests requiring `git` may assume it
  exists (it's a dev prerequisite; add it to the README line if you
  touch nothing else there — actually leave README to the orchestrator,
  note it in the report instead).
- **This repo is public on GitHub.** No secrets in code or test
  fixtures.
- Allowed new deps: none (stdlib + git binary).
- Finish with `docs/handoffs/phase-03-report.md`.

## Data-dir layout (contract with Phases 4/5)

Under `config.DataDir` (default `~/.lab`):

```
<data_dir>/projects/<project-id>/repo.git            # bare repo
<data_dir>/projects/<project-id>/worktrees/<agent-id>/   # one per agent
```

Worktree paths are what Phase 4 bind-mounts at `/work`. Branch naming:
`agent/<agent-name>` (agent names are unique per project).

## Deliverables

`gitrepo.Manager` created from a data-dir root. Operations (names
indicative; IDs are opaque strings to this package):

1. **CreateProject(projectID, originKind, origin)**
   - `git_url`: `git clone --bare <url>` into `repo.git`.
   - `local_path`: if the folder is not a git repo, `git init` +
     initial commit of its contents (author `lab <lab@localhost>`);
     then clone --bare from it. Either way the local path stays the
     `origin` remote of the bare repo.
   - Detect and record the default branch (`HEAD` of the clone);
     expose it (`Manager.DefaultBranch(projectID)`).
2. **RemoveProject(projectID)** — delete the project dir (worktrees
   first, `git worktree remove --force`, then the repo).
3. **AddWorktree(projectID, agentID, agentName)** — create branch
   `agent/<agentName>` from the default branch if it doesn't exist,
   `git worktree add` at the layout path, return the absolute path.
   Idempotent: if the worktree already exists and is healthy, return
   it.
4. **RemoveWorktree(projectID, agentID)** — `git worktree remove`
   (force flag parameter), delete the branch optionally.
5. **Fetch(projectID)** — fetch origin into the bare repo (for
   `git_url` projects; for `local_path`, fetch from the folder).
6. **Merge(projectID, sourceBranch)** — merge `agent/<x>` into the
   default branch **in a temporary worktree**, never in an agent's
   worktree. On conflict: abort the merge, leave everything clean, and
   return a typed `MergeConflictError` listing conflicted files. On
   success return the merge commit hash. No auto-resolution ever.
7. **Status(projectID, agentID)** — porcelain status of an agent
   worktree (dirty files, ahead/behind its base) for later UI use.

Implementation notes:

- One internal `run(ctx, dir, args...)` helper: captures stdout/stderr,
  wraps errors with the git command + stderr tail. All public methods
  take a `context.Context`.
- Concurrency: guard per-project operations with a per-project mutex
  (two agents' worktree ops on one repo must not interleave git's
  internal locking into user-visible failures).

## Tests

Integration tests using real git in `t.TempDir()` (no network — for the
`git_url` case, clone from a local bare fixture repo you create in the
test):

- Both origin kinds end-to-end (create → default branch detected →
  worktree added).
- `local_path` pointed at a non-repo folder gets initialized and its
  files survive into the worktree.
- Two agents, parallel `AddWorktree` + commits in each (goroutines,
  `-race`), both merge cleanly in sequence.
- Conflicting merge: returns `MergeConflictError` with the right file
  list, repo left clean (subsequent clean merge still works).
- Idempotent `AddWorktree`; `RemoveWorktree`; `Status` on a dirty tree.

## Acceptance criteria

- `make check` green (with `-race` locally for the parallel test).
- No leftover state: all tests in temp dirs; repeated runs pass.
- Layout exactly as the contract above (Phase 4/5 depend on it).

## Out of scope

Containers/mounts (Phase 4); driving agents (Phase 5); merge policy /
who triggers merges (Phase 11); auth for private remotes (post-v1 —
assume public or local remotes; note this limitation in your report).
