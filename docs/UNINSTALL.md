# Uninstall and data retention

Remove the installed binaries (`noise-tasks`; for a full package install
also `noise`, `noisetask`, and the libexec tree — see
[PACKAGING.md](PACKAGING.md)). The only data the tool ever writes, per
project, is:

- inside Git, `<git-common-dir>/noise-tasks/` — `tasks.sqlite`, the task
  authority, plus its lock and WAL sidecars (typically
  `.git/noise-tasks/` under the primary checkout);
- outside Git, `<project>/.tasks/tasks.sqlite`;
- at the project boundary, `.tasks/preferences.json` (adapter and model; no
  credentials) and anything you added under `.tasks/docs/`;
- task-bound workspaces under `<project>/.worktrees/<tag>-<id>` and their
  retained branches `tasks/<tag>/<id>`;
- any `*.ntbackup` bundles you created with `backup create`.

Delete those and nothing of the tool remains. There is no telemetry; the
only data that ever leaves the machine is PRD text sent to your configured
provider by `generate`.
