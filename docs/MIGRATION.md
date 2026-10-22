# Migrating to the SQLite authority

This runbook covers the v2.5 SPEC §8.6 one-way cutover from the retired JSON
authority (`<git-common-dir>/noise-tasks/live.json`) to the repository-family
SQLite authority (`<git-common-dir>/noise-tasks/tasks.sqlite`). The verified
`.ntbackup` bundle is the recovery medium. Migration never selects a worktree
projection and never restores JSON authority.

There is no quarantine, finalize, or rollback-to-JSON phase. Historic
`sqlite-migration.json` and quarantine/finalize artifacts are inert in v2.5.
The permanent proof is the migration receipt embedded in `tasks.sqlite`.

## 1. SQLite cutover

### Preconditions

- Install the same v2.5 binary in the primary checkout and every linked
  worktree. Confirm with `noise-tasks version`.
- Stop every process that can mutate the family, including pre-v2.5 binaries
  and agents in linked worktrees. `--offline` is an operator assertion, not a
  process killer.
- Update strict consumers to `context --json` schema 4 before migration. A
  consumer pinned to schema 1–3 fails closed by contract.
- Choose a durable destination for the `.ntbackup` bundle. Do not place the
  only copy in a disposable worktree.

### 1. Create and verify the recovery bundle

```sh
noise-tasks backup create --legacy --output /safe/path/family.ntbackup --json
noise-tasks backup verify --json /safe/path/family.ntbackup
```

The create command securely validates the exact derived `live.json`, converts
it to a current-schema SQLite payload, and records semantic, comparison, and
payload digests in the bundle manifest. `backup verify` is context-free and
read-only; it reopens and verifies the bundle independently.

Keep this exact bundle. A different bundle is not accepted by recovery merely
because it looks similar.

### 2. Create and accept the migration plan

```sh
noise-tasks migrate plan --json /safe/path/family.ntbackup
```

The plan command re-reads the exact derived legacy source, proves its
comparison digest and shared inventory equal the verified bundle, and
atomically publishes
`<git-common-dir>/noise-tasks/migration-plan.json`. Record the returned
`planDigest`. The plan changes no authority.

Inspect the family, source, target, bundle digests, schemas, and inventory in
the JSON output. Apply only after those facts match the intended repository
family.

### 3. Apply the accepted plan

```sh
noise-tasks migrate apply --digest sha256:<accepted-plan-digest> \
  --offline --request-id migrate-2026-08-28 --json \
  /safe/path/family.ntbackup
```

Under the exclusive family maintenance lock, apply re-verifies the bundle,
plan, source, family identity, owner boundary, schemas, and inventories. It
builds and verifies a fresh current-schema database, embeds a permanent
content-free migration receipt and event, then durably performs the cutover:

1. fsync `migration-apply.json`, the bounded recovery journal;
2. remove only the exact verified `live.json` and fsync the directory;
3. publish `tasks.sqlite` by a no-replace atomic rename and fsync;
4. remove the recovery journal and fsync again.

There is no point at which both files are selected as authority. On success,
`live.json` and the recovery journal are absent; `tasks.sqlite` contains the
permanent migration receipt. The derived plan may remain as a content-free
audit input.

### Crash recovery

If a normal command reports:

```text
noise-tasks: migration recovery required; rerun noise tasks migrate apply with the original plan and backup
```

keep writers stopped and rerun the exact apply command with the same bundle,
accepted digest, and request id. Apply recognizes only the five SPEC §8.6
states: fresh, resume-before-retirement, reconstruct-after-retirement,
published-journal-cleanup, and completed replay. A mismatched plan, request,
bundle, receipt, family, or authority shape fails closed without deleting or
choosing data.

In the deliberate no-authority crash state (`live.json` and `tasks.sqlite`
both absent, matching journal present), apply reconstructs the target from the
verified bundle. Do not run `setup`, `init`, a new plan, or a new legacy backup
over this state.

### 4. Verify

After publication, verify the cutover with read and mutation probes using
the released binary:

- from the **primary checkout**: `list`, `show --json` on a known task,
  and one CAS mutation (`set-status -revision`); and
- from **at least one linked worktree**: the same reads observe the
  committed mutation immediately.

Race two claimants on one task across the two checkouts and confirm exactly
one wins; the loser gets a typed claim conflict (exit 3). Run
`noise-tasks verify --json` after the probes.

### Restore from the bundle

Backup restore is generally useful and always restores SQLite. It never
resurrects `live.json`:

```sh
noise-tasks backup verify --json /safe/path/family.ntbackup
noise-tasks backup restore --overwrite --offline \
  --request-id restore-2026-08-28 --json /safe/path/family.ntbackup
```

Restore requires the same family and owner boundary when overwriting an
existing authority, publishes atomically, and embeds its own permanent receipt.
Use a new request id for a deliberately new restore. Reusing one request id
with different inputs is a typed conflict (exit 3).

### History diffs after cutover

`verify --against <git-ref>` compared the tracked projection against a
Git ref; with the projection gone it is valid only with `--file`. To
diff current state against history, export the comparison baseline you
would have committed (or keep a fixture snapshot in a PR) and run
`noise-tasks verify --file <snapshot> --against <ref>`. Authoritative
integrity is `verify` against the database itself.

## 2. Operational discipline after cutover

### `--file` is a fixture surface, never state

An explicit `--file <path>` operates on the named standalone JSON document
only: it is never selected by discovery, never becomes an authority, and
does not support claims, worktree bindings, revisions, or the feed. Keep
it out of anything that touches real task state; a script that passes
`--file` against a repository file is writing a fixture, not the backlog.

### Required `--revision` on mutations

Every mutation of an existing task — `set-status`, `append`, `remove` —
requires the expected revision. Flags precede the positional id and
status arguments:

```sh
noise-tasks show -json 42                # read: includes "revision": 7
noise-tasks set-status -revision 7 42 in-progress
```

Omitting `--revision` is a usage error (exit 2) — and usage validation
runs first: a transition missing `--revision` exits 2 even when its
`--owner`/`--token` would also mismatch, so exit-code-driven retry logic
should check 2 before 3. A stale revision is a typed conflict: exit 3,
nothing on stdout, and stderr carries the current revision, status, and
claim — one JSON value under `--json`, a human-readable diagnostic
otherwise — so the caller can re-read and retry. The tool never retries a
stale write implicitly. `add` needs no revision: ids come from the tag
row's `nextId` counter inside the same immediate transaction, so id
allocation is collision-free across processes and worktrees.

Script migration is mechanical: anywhere you mutate, read the task first
(`show --json` or `list --json`) and pass the reported revision. Treat
exit 3 as "re-read and reconcile", not as a failure to retry blindly.

### `init` and `setup` bootstrap the database directly

`noise-tasks init` noninteractively creates the §8.1 database with an
empty `main` tag; `setup` creates the database and preferences together
and remains rerunnable repair that never rewrites task state. Neither
command ever creates a worktree task-state file. A fresh clone of a
migrated repository is the intentional no-database state: commands fail
closed with `noise-tasks: project is not initialized; run noise tasks
setup`, and only `setup`, `init`, `context`, and `version` proceed.

### Claims for multi-agent work

`claim` and `release` coordinate exclusive work on a task:

```sh
noise-tasks claim -owner agent-7 -request-id agent-7-42-1 -json 42
# {"token": 18, "expiresAt": "...", "revision": 7, ...}

noise-tasks set-status -revision 7 -owner agent-7 -token 18 42 done
```

- Default TTL is 900 seconds; renew with
  `noise-tasks claim -owner agent-7 -token 18 -renew -json 42`. Renewal is a
  compare-and-swap that re-mints the token: the renew output carries the
  **new** token and supersedes the old one, so always take the token from
  the latest claim/renew response. The token is returned only to the
  acquirer and is a write capability — keep it out of logs.
- A status transition on a claimed task requires the matching `--owner` and
  `--token`. Marking `done` as the claim holder also releases the claim.
- Expired claims are treated as absent. Expiry is the whole recovery model:
  a dead agent's claim lapses on its own; there is no reaper or daemon.
- `release -owner o -force 42` drops another owner's claim and is always
  recorded on the feed; the binary requires `--owner` even with `--force`
  (the forcer's identity).
- The `done` floor is absolute: `done` is never reverted by expiry or
  claimant disappearance.

### Change feed

`noise-tasks live feed [--since <seq>] [--json]` replays the bounded,
content-free event log (adds, updates, statuses, claims, milestone and
worktree activity) for UIs and orchestration. It reports
`oldestRetainedSeq` so a poller can tell "nothing new" from "missed
events, full resync required". The feed is pull-based: no daemon,
watcher, or push subscription exists.

### Task-bound worktrees (SPEC §9)

One deterministic Git worktree per active task, under
`<repo>/.worktrees/<tag>-<id>` on branch `tasks/<tag>/<id>`. The §9.4
worktree saga is the authority: provisioning and removal are recorded as
recoverable saga rows in the database, so every checkout of the repository
family sees the same workspaces.

- Start work by acquiring a claim, starting a run, and provisioning from
  the live run: `worktree provision <runId> -owner <o> -claim-token <t>
  -attempt <n> -attempt-token <t> -request-id <r>`. The v5 cutover removed
  `worktree create`, `claim --worktree`, and `--keep-worktree` on claim,
  release, and set-status; each is now exit 2 with guidance to acquire a
  claim, start a run, and use `worktree provision`.
- Run finish with `succeeded`, run cancel, force release, and a direct
  `set-status <id> done` schedule the workspace's automatic cleanup in
  that same transaction. Uncommitted work is never deleted by automation:
  a dirty workspace stays put for an explicit operator
  `worktree remove <runId> ... -force`. Branch retention replaces the keep
  flags — removal never deletes `tasks/<tag>/<id>`.
- `noise-tasks worktree list [--json]` reports the saga table — entries
  ordered by saga id with status, activity, owner boundary, path, branch,
  base, and force bit — plus the `unknown` filesystem entries under the
  owner boundary's `.worktrees/` root that no saga row names. It never
  shows request ids, registration roots, generations, fences, or
  capabilities.
- `noise-tasks worktree prune [--json]` is the Trusted-Orchestrator
  recovery driver: it snapshots the eligible nonterminal sagas — including
  the runless legacy rows converted from v4 bindings — closes that read
  transaction, and invokes each row's recovery. Shape alone is never
  ownership proof: unknown directories and files are reported and never
  deleted. Its JSON handoff returns per-saga committed results, the
  controller broker requests (the only prune surface that may carry a
  saga token), and the unknown entries; human prune prints neither a
  token nor a mint binding.
- `.worktrees/` must be git-ignored (provisioning refuses otherwise, so a
  worktree can never be committed by accident), and branches follow your
  normal PR merge lifecycle.
- Scripting notes: worktree flags never combine with `--file` (exit 2),
  and the removed `--worktree`/`--keep-worktree` flags exit 2 with the
  cutover guidance. Saga transitions appear on the feed as content-free
  events (§8.4/§9.4).

## Quick reference

| Command | Purpose |
| --- | --- |
| `backup create --legacy --output <bundle>` | Create the verified legacy recovery bundle |
| `backup verify <bundle>` | Independently verify any backup bundle |
| `migrate plan <bundle>` | Verify source/bundle equality and publish the derived plan |
| `migrate apply --digest d --offline --request-id r <bundle>` | Apply or recover the one-way SQLite cutover |
| `backup restore [--overwrite] --offline --request-id r <bundle>` | Restore atomically to SQLite with a permanent receipt |
| `live feed [--since n]` | Replay the content-free change feed |
| `claim -owner o -request-id r [-ttl s] <id>` | Acquire a fenced claim with §8.4 replay (default 900 s) |
| `claim-next -owner o -request-id r [-ttl s] [-milestone m] [-assignment owner]` | Select and claim the next dependency-ready task atomically, with §8.4 replay |
| `claim -owner o -token n -renew <id>` | Renew a claim; response carries the new token |
| `release -owner o -token n <id>` | Drop a claim (`-force` for others') |
| `set-status/append/remove -revision n …` | CAS mutation against the database |
| `worktree list` | Report saga entries and unknown filesystem entries |
| `worktree provision / remove <runId> …` | Provision or remove a run's worktree saga (`-force` for a dirty removal) |
| `worktree prune` | Drive recovery for eligible worktree sagas; report unknown entries |

Flags precede positional arguments, and single- and double-dash forms are
equivalent (Go flag parsing; examples use the single-dash form the usage
text prints). Exit codes that
matter during migration: `2` usage error (e.g. missing
`--revision`), `3` typed conflict (stale revision or claim mismatch —
stderr carries current state, stdout stays empty).
