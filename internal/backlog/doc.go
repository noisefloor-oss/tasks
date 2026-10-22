// Package backlog owns the SPEC §1-§3 codec: the task/tag data model,
// JSON round-trip with byte-faithful unknown-field preservation, project
// boundary discovery (§2), the Git common-directory resolution behind the
// §8.1 store path, and atomic minimal deterministic writes. The codec also
// backs the standalone --file fixture document; no worktree file is task
// state (§8.1).
package backlog
