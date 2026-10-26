package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// §9.4 saga read surfaces: the public `worktree list` projection and the
// trusted-prune eligibility snapshot. Both are read-only: one closed deferred
// transaction observes a single committed snapshot. The list projection never
// carries a request id, registration root, generation, fence, or capability;
// the prune snapshot carries exactly what the §9.4 trusted prune surface may
// hand a controller — a stored fence and the re-emittable needs-broker mint
// included.

// WorktreeSagaListEntry is one §9.4 `worktree list` entry: exactly the
// public per-saga fields, in the spec's order. RunID and BaseCommit are null
// when the saga row stores NULL (a runless legacy cleanup row).
type WorktreeSagaListEntry struct {
	SagaID        int64   `json:"sagaId"`
	RunID         *int64  `json:"runId"`
	Tag           string  `json:"tag"`
	Task          string  `json:"task"`
	Operation     string  `json:"operation"`
	Status        string  `json:"status"`
	Active        bool    `json:"active"`
	OwnerBoundary string  `json:"ownerBoundary"`
	Path          string  `json:"path"`
	Branch        string  `json:"branch"`
	BaseCommit    *string `json:"baseCommit"`
	Force         bool    `json:"force"`
	UpdatedAt     string  `json:"updatedAt"`
}

// WorktreeSagaList is the exact §9.4 `worktree list` report: every saga row
// ordered by saga id, plus the owner boundary's unknown filesystem entries.
type WorktreeSagaList struct {
	Entries []WorktreeSagaListEntry `json:"entries"`
	Unknown []WorktreeUnknownEntry  `json:"unknown"`
}

// WorktreeUnknownEntry is one §9.4 unknown filesystem entry under the owner
// boundary's .worktrees root: exactly its full path, bytewise sorted against
// the other unknown entries, whatever the entry's name shape.
type WorktreeUnknownEntry struct {
	Path string `json:"path"`
}

// ListWorktreeSagas reads the §9.4 saga read surface (SPEC §9.4): every saga
// row of the family, ordered by saga id — rows owned by another checkout of
// the same family are reported through their ownerBoundary, never judged —
// and scans the invoking owner boundary's .worktrees root for unknown
// filesystem entries: entries no saga row's target path names, bytewise
// sorted, whatever their name shape. List never mutates anything.
func (d *DB) ListWorktreeSagas(ownerBoundary string) (*WorktreeSagaList, error) {
	report := &WorktreeSagaList{Entries: []WorktreeSagaListEntry{}, Unknown: []WorktreeUnknownEntry{}}
	err := d.deferred(func(t *tx) error {
		sagas, err := readWorktreeSagas(t, `SELECT `+worktreeSagaColumns+`
			FROM worktree_sagas ORDER BY saga_id`)
		if err != nil {
			return err
		}
		for _, s := range sagas {
			report.Entries = append(report.Entries, sagaListEntry(s))
		}
		report.Unknown, err = unknownWorktreeEntries(ownerBoundary, knownWorktreePaths(sagas))
		return err
	})
	if err != nil {
		return nil, err
	}
	return report, nil
}

// WorktreeSagaPaths reads just every saga row's target path, ordered by
// saga id, in one closed read transaction: the ownership evidence the
// §8.9 unowned-worktree report and the §2.4 doctor finding judge
// registered worktrees against, without the §9.4 unknown-entry
// filesystem scan the full list surface performs. It never mutates
// anything.
func (d *DB) WorktreeSagaPaths() ([]string, error) {
	paths := []string{}
	err := d.deferred(func(t *tx) error {
		sagas, err := readWorktreeSagas(t, `SELECT `+worktreeSagaColumns+`
			FROM worktree_sagas ORDER BY saga_id`)
		if err != nil {
			return err
		}
		for _, s := range sagas {
			paths = append(paths, s.path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return paths, nil
}

// sagaListEntry projects one saga row into the exact §9.4 list entry.
func sagaListEntry(s *worktreeSagaState) WorktreeSagaListEntry {
	e := WorktreeSagaListEntry{
		SagaID: s.id, Tag: s.tag, Task: s.task, Operation: s.operation,
		Status: s.status, Active: s.active, OwnerBoundary: s.boundary,
		Path: s.path, Branch: s.branch, Force: s.force, UpdatedAt: s.updated,
	}
	if s.run.Valid {
		run := s.run.Int64
		e.RunID = &run
	}
	if s.base.Valid {
		base := s.base.String
		e.BaseCommit = &base
	}
	return e
}

// WorktreePruneRow is one §9.4 prune-eligible saga snapshot row: the exact
// saga evidence recovery is authorized by — the saga id, its private fence,
// and the deterministic request id advertised for the next recovery
// generation. RequestID is empty at the exhausted generation, where no next
// id may be derived or advertised. BrokerRequest is the exact §9.4 handoff
// mint for a row already committed needs-broker, rendered from the row's
// immutable identity at that same advertised generation, so a prune run
// whose own broker reconciliation is refused still re-emits the
// token-bearing handoff (SPEC §9.4).
type WorktreePruneRow struct {
	SagaID        int64
	SagaToken     int64
	RequestID     string
	BrokerRequest *WorktreeBrokerRequest
}

// WorktreePruneSnapshot is the §9.4 trusted-prune eligibility snapshot: the
// family id, every nonterminal saga row (including runless legacy cleanup
// evidence), ordered by saga id, and the owner boundary's unknown filesystem
// entries — unknown judged against every saga row's target path, terminal
// rows included, so a live bound workspace is never reported unknown. The
// snapshot's read transaction is closed before any recovery effect runs.
type WorktreePruneSnapshot struct {
	FamilyID string
	Eligible []WorktreePruneRow
	Unknown  []WorktreeUnknownEntry
}

// SnapshotWorktreePrune snapshots the §9.4 prune-eligible rows (SPEC §9.4):
// every nonterminal saga — pending, needs-broker, or cleanup-required, which
// includes the runless legacy rows converted from v4 bindings — together
// with the deterministic recovery request id each row currently advertises;
// a row already committed needs-broker also carries the exact handoff mint
// rendered at that advertised generation — a runless legacy row's
// registration root derived at emission time, never persisted — so a later
// prune whose own broker reconciliation is refused still re-emits it
// (SPEC §9.4). Recovery
// filesystem report is judged against every saga row's target path,
// terminal rows included: an active bound provision with no cleanup saga is
// known workspace, never unknown. Filesystem shape alone is never ownership
// evidence: unknown entries under the owner boundary's .worktrees root are
// reported and never touched.
func (d *DB) SnapshotWorktreePrune(ownerBoundary string) (*WorktreePruneSnapshot, error) {
	snapshot := &WorktreePruneSnapshot{Eligible: []WorktreePruneRow{}, Unknown: []WorktreeUnknownEntry{}}
	err := d.deferred(func(t *tx) error {
		familyID, _, err := t.requireIdentity()
		if err != nil {
			return err
		}
		snapshot.FamilyID = familyID
		sagas, err := readWorktreeSagas(t, `SELECT `+worktreeSagaColumns+`
			FROM worktree_sagas ORDER BY saga_id`)
		if err != nil {
			return err
		}
		known := knownWorktreePaths(sagas)
		for _, s := range sagas {
			if isTerminalWorktreeStatus(s.status) {
				continue
			}
			row := WorktreePruneRow{SagaID: s.id, SagaToken: s.fence}
			if s.generation < MaxWorktreeRecoveryGeneration {
				advertised, err := worktreeRecoveryRequestID(familyID, s.id, s.generation+1)
				if err != nil {
					return err
				}
				row.RequestID = advertised
				if s.status == "needs-broker" {
					var base *string
					if s.base.Valid {
						value := s.base.String
						base = &value
					}
					// The durable handoff at the currently advertised
					// generation. A runless legacy row keeps its empty
					// stored root here — deriveLegacyPruneRegistrationRoots
					// completes that mint with the effect-time derivation
					// after the read transaction closes (SPEC §9.4).
					row.BrokerRequest = worktreeBrokerRequest(familyID, s, base, advertised)
				}
			}
			snapshot.Eligible = append(snapshot.Eligible, row)
		}
		snapshot.Unknown, err = unknownWorktreeEntries(ownerBoundary, known)
		return err
	})
	if err != nil {
		return nil, err
	}
	deriveLegacyPruneRegistrationRoots(snapshot.Eligible)
	return snapshot, nil
}

// deriveLegacyPruneRegistrationRoots completes every re-emitted
// prune-registration mint once the snapshot's read transaction has closed: a
// runless legacy row stores no registration root, so its re-emitted mint
// derives the root at emission time — the same effect-time derivation the
// recovery driver performs — and persists nothing (SPEC §9.4). A family
// whose registration root cannot be derived offers no exact registration
// proof to carry, so that row's handoff is not re-emitted.
func deriveLegacyPruneRegistrationRoots(rows []WorktreePruneRow) {
	for i, row := range rows {
		request := row.BrokerRequest
		if request == nil || request.Verb != "prune-registration" {
			continue
		}
		roots := registrationRoots(request.OwnerBoundary)
		if len(roots) == 0 {
			rows[i].BrokerRequest = nil
			continue
		}
		request.RegistrationRoot = roots[0]
	}
}

// WorktreeSagaStatus is the current committed status of one saga row.
type WorktreeSagaStatus struct {
	SagaID int64
	Status string
}

// ReadWorktreeSagaStatuses reads the current committed status of the named
// sagas in one closed read transaction, ordered by saga id. Saga rows are
// immutable history, so a missing id is corruption.
func (d *DB) ReadWorktreeSagaStatuses(ids []int64) ([]WorktreeSagaStatus, error) {
	statuses := []WorktreeSagaStatus{}
	if len(ids) == 0 {
		return statuses, nil
	}
	err := d.deferred(func(t *tx) error {
		query := `SELECT saga_id, status FROM worktree_sagas WHERE saga_id IN (`
		args := make([]any, 0, len(ids))
		for i, id := range ids {
			if i > 0 {
				query += ","
			}
			query += "?"
			args = append(args, id)
		}
		query += `) ORDER BY saga_id`
		rows, err := t.Query(query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s WorktreeSagaStatus
			if err := rows.Scan(&s.SagaID, &s.Status); err != nil {
				return err
			}
			statuses = append(statuses, s)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(statuses) != len(ids) {
			return fmt.Errorf("read worktree saga statuses: %d of %d sagas found", len(statuses), len(ids))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return statuses, nil
}

// readWorktreeSagas scans saga rows in worktreeSagaColumns order from one
// query's result set.
func readWorktreeSagas(t *tx, query string) ([]*worktreeSagaState, error) {
	rows, err := t.Query(query)
	if err != nil {
		return nil, fmt.Errorf("read worktree sagas: %w", err)
	}
	defer rows.Close()
	var sagas []*worktreeSagaState
	for rows.Next() {
		s, err := scanWorktreeSaga(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("read worktree saga: %w", err)
		}
		sagas = append(sagas, s)
	}
	return sagas, rows.Err()
}

// unknownWorktreeEntries lists the owner boundary's .worktrees filesystem
// entries no known saga target path names — files, directories, and symlink
// entries alike, whatever their name shape — as full paths, bytewise sorted.
// Membership compares each entry's own path, never resolved through a final
// symlink, so an unrecorded alias pointing at a saga target stays unknown. A
// missing root is an empty report; shape alone is never ownership evidence,
// so nothing is ever removed here.
func unknownWorktreeEntries(ownerBoundary string, known map[string]bool) ([]WorktreeUnknownEntry, error) {
	unknown := []WorktreeUnknownEntry{}
	root := WorktreeRoot(canonicalPath(ownerBoundary))
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return unknown, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		path := filepath.Join(root, e.Name())
		// Membership never resolves an entry through a final symlink: a
		// saga row must name the entry's own path, not its target (SPEC
		// §9.4).
		if !known[path] {
			unknown = append(unknown, WorktreeUnknownEntry{Path: path})
		}
	}
	sort.Slice(unknown, func(i, j int) bool { return unknown[i].Path < unknown[j].Path })
	return unknown, nil
}

// knownWorktreePaths records every saga row's target path under both
// spellings the unknown-entry membership compares: the row's absolute
// cleaned stored spelling — an entry is known only when a saga row names
// that exact path — and a canonical spelling that resolves only the
// directory components, so a row recorded through a symlinked boundary
// component still matches the resolved root the scan walks while a recorded
// entry that itself became a symlink never marks its target known — the
// final element is never resolved (SPEC §9.4).
func knownWorktreePaths(sagas []*worktreeSagaState) map[string]bool {
	known := make(map[string]bool, 2*len(sagas))
	for _, s := range sagas {
		known[filepath.Join(canonicalPath(filepath.Dir(s.path)), filepath.Base(s.path))] = true
		if abs, err := filepath.Abs(s.path); err == nil {
			known[abs] = true
		}
	}
	return known
}
