# Compatibility

The tool is experimental; only the commitments below are stable:

- **Context schema.** `noise-tasks context --json` emits one JSON object
  with every field present, currently `schemaVersion: 5`. Consumers treat an
  unknown schema version, a missing field, or an extra field as an
  unsupported contract and fail closed.
- **Exit codes.** 0 success; 1 operational failure (including an
  uninitialized project or a corrupt or newer store); 2 usage error; 3
  typed conflict — a stale revision, a claim or fencing conflict, or a
  request-id replay mismatch. `verify` keeps its own 0/1/2 contract
  (1 = findings). The `noise` host adds 126 (unsafe plugin or exec failure)
  and 127 (plugin not installed).
- **Store schema.** The binary owns the SQLite schema, recorded in
  the database's `PRAGMA user_version` (currently 6). An older store
  upgrades transactionally at its next mutation; a store newer than the
  binary refuses to open, fail-closed. The JSON task model is the stable
  exchange format used by the non-authoritative `--file` fixture surface,
  by backups, and by migration.
- **Managed compatibility window.** The `noisetask` shim, the `parse-prd`
  alias for `generate`, and the legacy provider-selector environment
  variables remain during the window; their removal is evidence-gated and
  will be announced as a breaking release, never tied to elapsed releases.

Everything else — flags, output shapes, file layouts — may change between
releases without a deprecation cycle.
