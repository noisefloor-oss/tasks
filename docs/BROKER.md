# Confined worktree registration and the broker

`noise-worktree-broker` is the SPEC §9.5 confined-registration broker: a
setuid/setgid root helper that performs exactly one step of the §9.4
worktree saga — the Git worktree *registration* — inside a sandbox that
must not be able to write the Git common directory directly.

It accepts exactly three verbs, `create`, `remove`, and
`prune-registration`, and refuses every invocation that does not carry an
authenticated host capability on inherited fd 3. It is stateless and
SQLite-blind: it never opens the task store, never chooses a task, never
mints a path, holds no state, listens on nothing, and runs as no daemon.
Whether an effect is an identical retry or a replay is decided by the
worktree saga and the shared `(worktree, requestId)` receipt rule, not by
the broker.

## Capability format

The capability is a two-line document — one line of canonical JSON and one
line of hex HMAC-SHA256 over the domain string
`noise-worktree-broker-capability-v1\0` and the JSON body — minted with the
32-byte capability key installed at `PREFIX/libexec/noise-worktree-broker.key`
(root-owned, mode 0400, beside the recorded libexec root and outside the
version-swapped plugin tree). The canonical body is:

```json
{"version":1,"verb":"create","family_id":"…","saga_fence":"…",
 "registration_root":"<common-dir>/worktrees","owner_boundary":"…",
 "target":"<owner>/.worktrees/<tag>-<id>","branch":"tasks/<tag>/<id>",
 "base_commit":"<full lowercase object id>",
 "force":false,"uid":1200,"gid":1200,"expires":1790000000}
```

Every field is bound: changing any byte fails authentication. The
capability contains no claim/run token and no credential. `force` is bound
to `remove` only. `base_commit` is the full 40- or 64-hex object id recorded
by the worktree saga before broker execution; symbolic and abbreviated
revisions are refused. The key is never present in the sandbox.

Mint with the packaged, unprivileged tool on the trusted host:

```sh
/usr/libexec/noise-broker-mint -verb create \
  -family <family-id> -saga <saga-fence> \
  -registration-root /srv/family/.git/worktrees \
  -owner-boundary /srv/family \
  -target /srv/family/.worktrees/main-42 \
  -branch tasks/main/42 \
  -base-commit 0123456789abcdef0123456789abcdef01234567 \
  -uid 1200 -gid 1200 -ttl 10m
```

## Broker behavior

On every invocation the broker, in order:

1. Parses one strict argv: exactly one verb of `create`, `remove`, or
   `prune-registration`. There is no option surface at all — in
   particular no caller-selected key path; anything else, including any
   option, is a usage error (exit 2).
2. Reads the capability from inherited fd 3 with one complete, bounded
   read — a partial or oversized stream is a refusal, never a truncated
   document — and then closes fd 3, so no leaf exec can inherit it.
   Alternatively fd 3 may be the host's own mount-namespace descriptor,
   in which case the broker re-enters that namespace, likewise closes the
   descriptor, and reads the capability document from stdin under the
   same complete, bounded discipline. An invocation with neither — the
   leaf's view — is refused with a bounded reason (exit 1).
3. Loads the capability key from the fixed installed path beside the
   build-time-recorded libexec root — never from argv or environment. The
   open is `O_NOFOLLOW|O_CLOEXEC`; `fstat` on the open descriptor must
   prove a regular root-owned file with exactly owner-only permissions
   (mode 0400 per the packaging docs); and the read drains that one
   descriptor, bounded to the key size, so the path is never reopened and
   there is no stat/read TOCTOU window.
4. Verifies the HMAC, the schema version, and the expiry with the real
   clock, then checks the capability's verb and invoking uid/gid against
   the actual invocation. Any mismatch is a refusal.
5. Validates every binding syntactically (paths absolute and lexically
   clean, branch exactly `tasks/<tag>/<id>`, target exactly
   `<owner>/.worktrees/<tag>-<id>`) and then on the filesystem with
   no-follow `openat` walks: the owner boundary, the registration root,
   and `.worktrees` must be real, non-symlink directories. For `create`,
   a missing target is the fresh case and an existing target is admitted
   only as a real directory — never a symlink or other non-directory —
   leaving Execute to return `changed:false` only when the exact target
   is already registered (the identical retry) and to refuse any other
   existing target.
6. If the registration root is not visible in the current mount namespace,
   re-enters the host mount namespace — only through PID 1's namespace or
   the authenticated inherited fd-3 descriptor, never a caller-supplied
   path — while still privileged. On platforms without Linux mount
   namespaces, the registration root must already be directly reachable;
   namespace re-entry refuses fail-closed.
7. Drops privilege permanently to the invoking uid/gid (supplementary
   groups cleared, all three copies of each id set) before any Git or
   target effect.
8. Runs the trusted absolute Git executable `/usr/bin/git` with fixed argv
   and a stripped, fixed environment
   (`PATH`, `HOME=/nonexistent`, `LC_ALL=C`, `GIT_CONFIG_NOSYSTEM=1`,
   `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_TERMINAL_PROMPT=0`,
   `GIT_OPTIONAL_LOCKS=0`); nothing inherited reaches the child. It first
   proves with `rev-parse --git-common-dir` that the bound registration
   root is exactly this repository's worktrees directory.

The three verbs are idempotent for identical bound inputs: `create` uses
exactly the authenticated saga-recorded base commit and is a no-op success
(`changed:false`) when the exact target and branch are already registered;
`remove` of an absent registration likewise; and `prune-registration`
removes only the exact absent target's matching registration, reporting
`changed:false` when it is already absent. It never invokes repository-wide
pruning. Success is one bounded JSON line on
stdout (`{"ok":true,"verb":"…","changed":…}`). Every refusal is one
bounded, sanitized, single-line reason on stderr (exit 1). Git stdout and
stderr are streamed into independent bounds while fully drained; structured
porcelain is parsed only when complete, and any overflow is refused rather
than truncated into an incomplete decision. The broker never distinguishes
a retry from a replay; the saga does.

## Deck-side controller and leaf confinement

`noise-tasks` prepares durable state and the SPEC §9.3A controller projection
and leaf brief. It does not launch an agent, choose a provider, create a process
sandbox, or grant filesystem access. Deck owns those deployment actions.

When Deck runs the trusted controller in a mount namespace where the Git
registration directory is confined, it may expose that directory read-only so
paths resolve while writes fail:

```sh
mkdir -p "$controller_root$regroot"
mount --rbind "$regroot" "$controller_root$regroot"
mount -o remount,ro,bind "$regroot" "$controller_root$regroot"
```

A direct registration attempt then supplies the confinement signal. Deck uses
the returned controller-only saga token and broker request to mint one
short-lived capability bound to the exact saga fence, verb, roots, target,
branch, base commit, force bit, and controller uid/gid. It passes that document
on fd 3 only to the matching trusted `noise-tasks worktree recover` process;
`noise-tasks` deliberately forwards it only to the broker. The descriptor is
close-on-exec for every other child.

Deck may start a leaf only after either provision or recovery yields an active
bound provision and the §9.3A eligibility checks return a leaf brief. Recovery
is not required when the original provision binds directly. At that boundary
Deck:

1. Uses a distinct leaf principal that cannot act as the controller.
2. Exposes a writable view of the bound worktree at a fixed leaf path, sets it
   as the leaf's current directory, and passes the leaf brief whose workspace
   path is exactly `.`. An id-mapped mount, overlay, or equivalent host policy
   may provide writability without granting the leaf access to the absolute
   owner boundary. A usable `.git` escape is omitted.
3. Omits the Git common directory, SQLite directory, trusted plugin root,
   broker key, controller host paths, and provider credential sources from the
   leaf namespace. Merely removing `noise-tasks` from `PATH` is not an authority
   boundary; the distinct principal and filesystem policy must deny the state.
4. Unsets `NOISE_PROJECT_DIR`, passes no controller argv or environment value,
   scrubs the environment to a reviewed allowlist, and closes every inherited
   descriptor other than the leaf's intended standard streams. In particular,
   fd 3 is absent.
5. Keeps provider selection, authentication, and credentials on the trusted
   side of the boundary. The leaf receives the bounded brief and workspace and
   returns a proposal; Deck validates that proposal before using any controller
   capability.

Because the broker validates family, saga fence, verb, roots, target,
branch, base commit, identity, and expiry before dropping privilege, a
sandboxed leaf
that somehow reached the setuid binary still cannot make it act: without
the fd-3 capability, with a tampered or expired one, or with one bound to
any other verb, target, or identity, it refuses with a bounded reason and
no effect.

The package tests can prove the broker's authenticated fd-3 input, descriptor
closure, fixed process surface, privilege drop, and refusal without a
capability. They can also prove that the §9.3A brief serializes no controller
secret or host path. They cannot prove a particular Deck deployment's uid,
mount, provider, environment, descriptor, or process-isolation policy. Deck's
deployment conformance suite owns those assertions.

## Diagnostics

- Broker refusal (broker's own stderr): `noise-worktree-broker: refused:
  <bounded sanitized reason>`, exit 1.
- Orchestrator surface (SPEC §9.5): a permission failure without a valid
  broker capability is `noise-tasks: worktree registration is confined and
  no broker is available`; a broker refusal is
  `noise-tasks: worktree broker refused: <bounded sanitized reason>`
  (exit 1). Both leave the saga recoverable.

## Testing

- The unprivileged suites (`internal/broker`, `cmd/noise-worktree-broker`)
  run under `go test ./...` with no prerequisites beyond Git: capability
  mint/verify round-trip, tampering, expiry, strict canonical encoding,
  binding refusals, no-follow path validation, real-Git verb idempotency,
  and in-process fd-3 delivery at the test identity. Key-loading
  regressions cover symlinked, non-root-owned, overly broad, non-regular,
  and wrong-size keys; capability reads cover chunked (complete),
  truncated, and oversized documents and the closing of fd 3 before any
  Git exec; the argv surface proves unknown `--key` usage and
  attacker-key-minted documents are refused; and exact create idempotency
  runs through the CLI entry, including the refusal of an existing
  unregistered or symlinked target. A real-process test pins the libexec
  root by release-style ldflags and proves the key is read only from the
  fixed installed path.
- The privileged suite builds the binary the same release way, installs the
  binary root-owned mode 6755 and the key root-owned mode 0400 at that fixed
  path, then launches it with a non-root real uid/gid so the kernel supplies
  the real setuid transition on an isolated root runner:

  ```sh
  NOISE_BROKER_PRIVILEGED_TESTS=1 sudo -E go test \
    ./cmd/noise-worktree-broker/ -run Privileged -v
  ```

  It proves fd-3 authentication, tampered/expired capability and binding
  mismatch refusal, attacker-key and argv `--key` refusal, symlinked,
  overly broad, and non-root-owned key refusal at the fixed path,
  the installed binary's root ownership and 6755 mode, identical-retry
  idempotency across real processes (and the refusal of
  an existing unregistered target), the permanent privilege drop (every
  created path is owned by the bound non-root identity), operation under
  a hostile inherited environment (fixed argv and stripped environment),
  and leaf denial without fd 3. It skips safely — as do the unprivileged
  real-process tests — whenever the opt-in variable, root, or the Go
  toolchain is absent.
