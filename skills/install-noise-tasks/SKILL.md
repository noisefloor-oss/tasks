---
name: install-noise-tasks
description: Install or upgrade noise-tasks, the standalone task-backlog CLI, by building a tagged release from source. Use when the user asks to install, update or reinstall noise tasks.
---

# Install noise-tasks

## What this is

Install or upgrade noise-tasks vX.Y.Z+ on the user's Linux host by building
the tagged release from source. The agent runs the checks and commands; the
human owns every privileged decision and every provider call.

## Read this to the human first

noise-tasks is a standalone local CLI. It stores no credentials, and its only
networked command is `generate`, which sends PRD text to the user's configured
provider and spends their money — the agent never starts it. Host
requirements, each with its refusal:

- Linux (`uname -s` prints `Linux`; anything else → stop).
- Go at the version the tag's `go.mod` declares or newer (`go version`; a
  missing or older toolchain → stop). The build is pure Go, including the
  SQLite driver: no C toolchain or system libraries.

## Never

- Run `sudo` or pass a password.
- Run `make package` on the host or install the release's installation
  archives: both install a setuid worktree broker under `/usr/local` and
  require root by design. That is an owner decision; point the user at
  [docs/PACKAGING.md](https://github.com/noisefloor-oss/tasks/blob/main/docs/PACKAGING.md)
  if they ask.
- Run `noise-tasks generate` or any other provider call unless the user
  starts it.
- Write outside `~/.local/bin` and the user's existing projects.
- Edit `.tasks/preferences.json` by hand — it is a closed schema; use
  `setup`.
- Bypass a failed check, or fall back to another tag, commit or artifact
  after a failure.

## Install

1. Resolve the latest published stable `vX.Y.Z` anonymously; exclude drafts
   and pre-releases. Release checksums are deliberately not used here:
   nothing is downloaded to install — the build starts from the verified
   tag below.
2. Clone and build in a fresh scratch directory (for example
   `mktemp -d`), never inside a checkout the user already has:
   `git clone https://github.com/noisefloor-oss/tasks.git` and
   `git checkout vX.Y.Z`. The Go toolchain also writes its own build and
   module caches outside that directory; that is the full write boundary.
3. Bind the checkout to the published tag, not to release metadata: the
   release API's `target_commitish` may name a branch (the current releases
   say `main`). Ask `git ls-remote origin 'refs/tags/vX.Y.Z^{}'` for the
   peeled commit — annotated tags resolve to it directly; earlier releases
   in this family are annotated, the current public one is lightweight —
   and when no peeled line comes back, use the plain
   `refs/tags/vX.Y.Z` line instead. Compare that SHA with
   `git rev-parse HEAD`. Mismatch → hard stop.
4. `make build VERSION=X.Y.Z` (no leading `v`). The standalone engine lands
   in `build/bin/noise-tasks`.
5. `install -m 0755 build/bin/noise-tasks ~/.local/bin/`, and confirm with
   the user that the directory is on their `PATH`.
6. Confirm: `noise-tasks version` prints `noise-tasks X.Y.Z`. Checking a
   board needs an initialized project (`setup` or `init`), which is the
   user's call to make — do not initialize a project for them. Ask the user
   to read docs/SECURITY_NOTES.md in this repository before generating anything.

## Upgrade

The same resolve, verify, build and install at the newer tag. Per-repository
task state lives in each repository's own store and migrates itself; there is
nothing to move. Ask the user before replacing a binary they may be running.

## If anything fails

Stop and report the exact failing check to the user. Never bypass a check and
never fall back to a different artifact, tag or commit to get past it.
