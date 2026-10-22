# Worktree discipline for task lanes

Task-bound worktrees are the unit of parallel agent work: one lane, one
worktree, one branch (SPEC §9.4). The engine's provision/remove
sagas own lanes created through `worktree provision`; this document pins
the two operational rules that protect the shared family checkout — the
primary checkout and the `main` branch every lane depends on — from
mistakes made *outside* those sagas.

Both rules come from a real incident, forensically reconstructed; see the
end of this document.

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
