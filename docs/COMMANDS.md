# Working a backlog

You write a product requirements document; `noise tasks generate` turns it
into a dependency-ordered backlog through a configured LLM provider (see
[GENERATION.md](GENERATION.md)), and the CLI tracks that backlog —
statuses, priorities, dependencies, subtasks, and milestones — as ordinary
data you can list, query, and change from a terminal or a script. The
canonical commands are `noise tasks <command>` and the standalone
`noise-tasks <command>`; both have the same behavior.

Task state lives in one SQLite database derived from the repository's Git
common directory, so the primary checkout and every linked worktree resolve
the same backlog regardless of branch; no file inside a worktree is ever
task state. Concurrent workers are fenced, not trusted: tasks are claimed
under expiring leases with fencing tokens, every mutation of an existing
task is compare-and-swap on its revision, and claims, runs, plan
applications, and worktree operations commit immutable receipts into the
same database. A content-free change feed, versioned backups with a one-way
cutover from the retired JSON layouts (`migrate` for the derived
`live.json`, `migrate adopt` for a retired `.tasks`/`.noisetask`/
`.taskmaster` layout into a fresh store; see
[MIGRATION.md](MIGRATION.md)), atomic orchestration plans, and
task-bound Git worktrees complete the engine. Machine consumers use the
`--json` surfaces and the versioned `context --json` contract rather than
reproducing discovery.

What it deliberately does not do: no daemon, no TUI, no network listener,
no telemetry, no multi-repository sync or server, and no AI rewriting of
existing tasks. It does not choose models, supervise agent processes, judge
output quality, or merge branches — deciding what to do with a prepared
task belongs to a trusted orchestrator (Noisefloor Deck is one such
consumer); `noise-tasks` commits, fences, records, and proves. The only
networked command is `generate`.

## Everyday commands

```sh
noise-tasks list
noise-tasks next        # highest-priority pending task whose dependencies are done
noise-tasks show 1
```

Claims fence concurrent workers. Acquiring one returns a fencing token;
mutating a claimed task requires its owner and token, and every live
mutation passes the expected `--revision` (compare-and-swap: a stale
revision exits 3 — re-read with `show --json` and reconcile):

```sh
noise-tasks claim --owner you --request-id demo.claim.1 1
# token=1 expiresAt=2026-10-03T08:54:08Z revision=1

noise-tasks set-status --owner you --token 1 --revision 1 1 in-progress
noise-tasks set-status --owner you --token 1 --revision 2 1 done
noise-tasks next        # the dependent task is now ready
```

Tasks may carry a set-once external link naming the source issue they
track — `family/repository#issue`. The link is unique across the store,
so replaying the same `add --link` returns the existing task instead of
duplicating it, and `set-link` assigns one under the same
compare-and-swap rule as every field mutation:

```sh
noise-tasks add --title "Fix crash on import" --link github/acme/widgets#214
noise-tasks set-link --revision 3 7 github/acme/widgets#215
```

Issue-driven work has one documented happy path: `ensure` the linked
task from the repository issue (the caller supplies the title and body;
the CLI does no network and never syncs later issue edits), then claim,
`run start`, and `worktree provision` as usual. It is idempotent by the
link's uniqueness — an importer and an agent racing on one issue get one
task, and the replay returns the existing task without writing:

```sh
noise-tasks ensure --repo acme/widgets --issue 214 \
  --title "Fix crash on import" --description "Importing an empty file crashes."
# created the linked task for github/acme/widgets#214
# (a replay prints: the same task already tracks the link, no second write)
```

`noise-tasks release --owner you --token 1 1` drops a claim; setting status
`done` clears it. `noise-tasks --help` (also `-h` and `-help`) lists every
command, and `noise-tasks <command> -h`, `-help`, or `--help` shows that
command's — or command group's — usage on stdout; help always exits 0.

`noise-tasks hygiene` is the read-only fleet report: stale runs, expired
claims, and claimless in-progress tasks from the task store, plus every
Git-registered worktree the engine does not own (path, branch or detached
head, existence, dirtiness — review checkouts anywhere on disk, not just
under `.worktrees/`) and the primary checkout's state when it is detached
or off the default branch (`origin/HEAD`, falling back to `main`). It
only reports; it never deletes, checks out, or writes anything (status
probes run `--no-optional-locks`, so no index is ever refreshed). The
worktree classes need Git 2.36+. `--json` prints the versioned schema
from SPEC §8.9.

`noise-tasks doctor [--root <dir>]` walks a workspace and inventories
every repository family it finds: store presence and schema, retired
layout residue, and — per initialized family — the hard finding of every
Git-registered worktree no worktree saga row names (path, branch or
detached head, existence, dirtiness), the same ownership judgment
`hygiene` makes: evidence that work bypassed the engine's
`run start` + `worktree provision` flow. Findings and per-family
failures alike are report-only: a family whose worktree evidence cannot
be read carries an `unboundWorktreesError` instead, every other family
still reports, and `doctor` exits 0 either way. It never mutates; the
worktree class needs Git 2.36+. `--json` prints the versioned schema
from SPEC §2.4.
