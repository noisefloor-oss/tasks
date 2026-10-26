# Worktree discipline for task lanes

Task-bound worktrees are the unit of parallel agent work: one lane, one
worktree, one branch (SPEC §9.4). The engine's provision/remove
sagas own lanes created through `worktree provision`; this document pins
the operational rules that protect the shared family checkout — the
primary checkout and the `main` branch every lane depends on — from
mistakes made *outside* those sagas.

Rules 1 and 2 come from a real incident, forensically reconstructed; see
the end of this document. Rule 3 is the ownership rule that keeps
worktrees from leaking in the first place.

## Rule 1: never force-move a branch owned by another worktree

The **owning checkout** of a branch is the worktree that has it checked
out. Only the owning checkout advances, resets, or deletes that branch.

From a linked worktree, never run any of these against a branch owned
elsewhere:

- `git checkout -B <branch> <start>`
- `git branch -f <branch> <start>`
- `git update-ref refs/heads/<branch> …`

Git accepts some of these from a non-owning worktree, and they move the
ref **without updating the owning checkout's index or working tree**.
The owning checkout is then silently diverged: its HEAD points at the new
commit while its index and working tree still reflect the old tree.

To sync a branch such as `main` in the primary checkout, run the
fast-forward **in the owning checkout**:

```sh
git fetch origin
git -C /path/to/primary merge --ff-only origin/main
```

or let the merge tooling that owns that checkout perform the update.
A linked worktree that wants current `main` should create its lane from
`origin/main` after `git fetch` — never reshape the shared branch.

## Rule 2: orchestrator loops assert primary-checkout cleanliness

Any automated loop that merges into, builds from, or publishes the
primary checkout — night-shift drains, release qualification, canary
staging — asserts cleanliness around the steps that move refs. The
assertion names the primary checkout by path (the loop may itself run
from a lane), stops when `git status` itself fails, and stops after
alerting — a successful alert must never let the loop continue through
the divergence it detected:

```sh
out="$(git -C /path/to/primary status --porcelain)" || {
    echo "orchestrator: git status failed in the primary checkout" >&2
    exit 1
}
if [ -n "$out" ]; then
    printf 'orchestrator: primary checkout is dirty:\n%s\n' "$out" >&2
    exit 1
fi
```

`D ` (staged deletion) or ` M` entries at this point mean an
index/worktree divergence that must be understood before any merge lands
on top of it. An assertion after the merge step catches a divergence the
merge itself introduced or carried.

Recovery for the incident shape below: inspect `git diff --cached --stat`,
prove which paths should exist at HEAD, then
`git restore --staged --worktree -- <path>` for each. Use
`git reset --hard <ref>` only after proving no uncommitted work would be
discarded.

## Rule 3: every worktree has one named owner

Hand-made worktrees are the source of nearly every leaked worktree in
this fleet. The rule:

- **Task work goes through the engine.** Work on a backlog task happens
  in a lane created by `run start` plus `worktree provision` (SPEC §9.4):
  a worktree under `<ownerBoundary>/.worktrees/<tag>-<task-id>` on
  branch `tasks/<tag>/<id>`, bound to its run's saga. Never create a
  worktree by hand for task work, and never adopt an existing directory
  as one.
- **Any other worktree is an exception.** An ad-hoc worktree must have a
  named owner — a person or a service, written down somewhere the
  worktree can be found later (the command history, ticket, or commit
  message that created it) — and a cleanup step executed in the same
  session that created it. An exception that outlives its session is a
  leak; treat it as one.
- **Reviewers never switch or edit the shared primary checkout.**
  Reviews read the PR diff and, when a local tree is needed, prefer a
  disposable copy, or use the task's own provisioned lane **read-only** —
  that lane is the author's working state. A reviewer never switches
  branches, pulls, edits, or runs state-changing commands in the primary
  checkout — that checkout belongs to the merge orchestrator (Rules 1 and
  2). This is the interim mitigation until a managed review workspace
  exists.

  A *disposable copy* is a separate clone: it registers nothing in the
  family's Git administrative state and is deleted freely. A
  `git worktree add` copy is **not** disposable — it registers shared
  state under `<common-dir>/worktrees/<name>` and falls under the ad-hoc
  rule above: named owner, cleanup in the same session.

### A merged PR alone does not make a worktree safe to delete

Merge status is one signal, not the whole check. This checklist governs
ad-hoc worktrees and any hand deletion of a worktree the §9.4 saga does
not own. Engine-owned lanes are fenced by the saga itself: manual
`worktree remove` revalidates the live claim and the current attempt
(SPEC §9.4), and automatic cleanup on run termination or task done
needs no claim — so never hand-delete a lane out from under its saga.
Before removing a worktree by hand, **all** of the following must hold:

1. **Clean tree.** `git -C <worktree> status --porcelain` is empty. A
   dirty tree may hold the only copy of uncommitted work.
2. **No commits that exist only locally.** `git fetch`, then verify the
   worktree's commits are not only local. Account for squash merges: a
   squash merge lands a *new* commit on `main` with different hashes, so
   the lane's commits are never reachable from `origin/main` even after
   the PR merges — check the PR's merged status and its diff, not branch
   reachability. Unpushed, unmerged commits are lost work.
3. **No live claim or run.** No live claim covers the work and no run is
   still executing in that worktree — check with
   `noise-tasks run list --task <task-id> --json` (or
   `run show <run-id> --json`) — and no process is still running with
   that worktree as its working directory. Note that a bare `release`
   refuses while a nonterminal run exists, so run liveness is the
   stronger signal.
4. **No lock.** Nothing — lockfile, editor session, build — holds the
   worktree or its Git registration under
   `<common-dir>/worktrees/<name>`.

Engine-owned lanes are removed by the §9.4 saga — automatically on run
termination or task done, or by `worktree remove`; `worktree prune`
recovers sagas left in `needs-broker` or `cleanup-required` — and the
branch is retained after removal. Ad-hoc worktrees are removed by their
named owner after the four checks above pass.

## The incident (2026-08-29, reconstructed 2026-09-06)

At 18:57:07Z, a session in a linked worktree ran
`git checkout -q -B main origin/main`. That force-reset `main` — owned by
the primary checkout — without touching the primary's index or working
tree. `main` had just fast-forwarded past a squash merge that added a
492-line test file; the primary's index still reflected the pre-merge
tree, so the divergence materialized as a **staged deletion of a file
merged an hour earlier**. The phantom deletion silently survived two
subsequent fast-forward merges and sat unnoticed for eight days, during
which any `go test` run in the primary quietly skipped that suite.

Detection was manual; Rule 2 exists so the next one is not.

## Relation to engine machinery

`worktree provision` places engine-owned lanes under
`<ownerBoundary>/.worktrees/<tag>-<task-id>` on branch
`tasks/<tag>/<id>` and records the saga binding (SPEC §9.4). Those lanes
never reshape `main`. The rules above cover everything else: ad-hoc
worktrees, reviewer copies, and orchestrator tooling that touches the
shared family checkout.
