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
cutover from the retired JSON layouts, atomic orchestration plans, and
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

`noise-tasks release --owner you --token 1 1` drops a claim; setting status
`done` clears it. `noise-tasks --help` lists every command, and
`noise-tasks <command> -help` shows its flags.
