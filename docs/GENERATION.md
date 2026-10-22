# Generating tasks from a PRD

Onboarding records one generation provider in `.tasks/preferences.json` —
an adapter and a model, never credentials — and creates the database. Start
inside a Git repository (outside Git the current directory becomes the
project boundary and the database lives at `.tasks/tasks.sqlite`):

```sh
git init greeter && cd greeter
export ANTHROPIC_API_KEY=...            # for the anthropic-api adapter
noise-tasks setup --adapter anthropic-api --model claude-sonnet-4-5 --yes
```

The compiled-in adapters are `anthropic-api` (in-process HTTPS; reads
`ANTHROPIC_API_KEY` at invocation time), `claude-cli`, `omp`, `codex`, and
`kimi`. CLI adapters run the fixed `claude`, `omp`, `codex`, or `kimi`
executable from your `PATH`. Rerun `setup --adapter <adapter> --model <model>`
to change the choice. Codex and Kimi run in empty temporary directories;
Kimi does not offer switches to disable tools or session storage.
`noise-tasks init` is the task-only variant: it creates the database with an
empty `main` tag and no generation preferences.

Write a small PRD at the conventional path and generate a first backlog
(`generate` is the only networked command; without `--yes` it previews the
drafts and asks before appending):

```sh
mkdir -p .tasks/docs
cat > .tasks/docs/PRD.md <<'PRD'
# Greeter CLI PRD

A tiny command-line greeter.

## Requirements
- A `greet` command that prints "Hello, <name>!" for a required --name flag.
- A friendly default of "world" when --name is omitted.
- Unit tests for the greeting formatting.

## Non-goals
- No configuration file, no internationalization, no daemon.
PRD

noise-tasks generate --yes .tasks/docs/PRD.md
```

```text
3 task(s) drafted from .tasks/docs/PRD.md for tag "main":

   1. Create the project skeleton  [high]  deps: -
   2. Implement the greet command  [medium]  deps: 1
   3. Add unit tests  [medium]  deps: 2
appended 3 task(s) to tag "main":
```
