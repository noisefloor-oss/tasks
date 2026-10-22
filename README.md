# Noisefloor Tasks

Tasks is a task tracker built for coding agents. It gives them a shared
backlog, a way to claim work, and a separate working copy for each task.
You can see what needs doing, what depends on it, and who has taken it on.

You describe the product in a brief, or product requirements document, and
Tasks can ask your configured agent or model to turn it into a backlog.
Agents work through the tasks
in dependency order, updating their progress as they go. You can also
create and edit tasks directly.

## A working copy for each task

Tasks manages Git worktrees, so agents can make changes on separate branches
while sharing the same backlog. It records ownership and manages the working
copies from creation through cleanup. An agent can work on one part of the
product while another works on a different task.

The command-line interface is designed for agents and scripts. Tasks tracks
the work; your agent or workflow decides how to implement it, test it and
get it reviewed. The [worktree guide](docs/WORKTREES.md) covers safe handling
of those working copies.

## A shared backlog

Every checkout of a repository sees the same tasks. An agent can list the
work or ask for the next task whose dependencies are complete:

```sh
$ noise-tasks list
ID  STATUS   PRIORITY  DEPS  TITLE
1   pending  high      -     Create the project skeleton
2   pending  medium    1     Implement the greet command
3   pending  medium    2     Add unit tests
3 tasks: 3 pending

$ noise-tasks next
id:           1
title:        Create the project skeleton
status:       pending
priority:     high
dependencies: -
updated:      2026-01-01T09:00:00Z
```

**Status: experimental.** Daily-driven by its author; interfaces may change
between releases. See [DEVELOPMENT.md](DEVELOPMENT.md) for how the project
is developed.

## Part of the Noisefloor workflow

[Deck](https://github.com/noisefloor-oss/deck) brings projects, tasks and
agent sessions into a browser workspace for building products with AI.
Tasks manages the work, [SDLC](https://github.com/noisefloor-oss/sdlc)
provides the project's delivery rules, and
[Visual Diff](https://github.com/noisefloor-oss/visual-diff) gives agents
screenshot feedback as they implement a design.

Tasks also works on its own, with the coding agents you already use.

## Install

Give your coding agent this prompt:

> Install noise-tasks from its latest release, following
> <https://github.com/noisefloor-oss/tasks/blob/main/skills/install-noise-tasks/SKILL.md>.

From source, with the Go version declared in `go.mod` or newer:

```sh
git clone https://github.com/noisefloor-oss/tasks.git
cd tasks
git checkout vX.Y.Z           # the release tag
make build VERSION=X.Y.Z      # the same version, without the leading v
install -m 0755 build/bin/noise-tasks ~/.local/bin/
noise-tasks version           # prints: noise-tasks X.Y.Z
```

Release artifacts and their SHA-256 checksums are on GitHub Releases. For a
full system install, see [docs/PACKAGING.md](docs/PACKAGING.md).

## Learn more

- [Generating tasks from a PRD](docs/GENERATION.md): set up a provider and
  turn a PRD into a backlog.
- [Working a backlog](docs/COMMANDS.md): claims, statuses, and everyday
  commands.
- [Worktrees](docs/WORKTREES.md): task-bound worktree discipline.
- [Packaging and the worktree broker](docs/PACKAGING.md), with
  [the broker itself](docs/BROKER.md).
- [Migrating from the JSON store](docs/MIGRATION.md).
- [Compatibility](docs/COMPATIBILITY.md): what stays stable between
  releases.
- [Security notes](docs/SECURITY_NOTES.md) and [SECURITY.md](SECURITY.md)
  for reporting a vulnerability.
- [Uninstall and data retention](docs/UNINSTALL.md).

## License

MIT © 2026 Doug Doan. Built at [Noisefloor](https://noisefloor.sh). See
[LICENSE](LICENSE).
