package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// §9.4 automatic worktree cleanup scheduling. The four terminal parents —
// run finish recording succeeded, run cancel, force release that terminalizes
// a run, and direct top-level set-status done — establish cleanup authority
// inside their own mutation transaction by calling scheduleWorktreeAutoCleanup
// before that transaction commits. Scheduling mutates saga rows only: it
// appends no event (the parent's own event audits the decision without
// widening its receipt's event range) and writes no worktree receipt (none
// exists until an effect attempt commits). Neither a new nor an adopted
// cleanup saga stores attempt lineage, a cause revision, or a second
// authority record. The Git effect starts only after the parent transaction
// commits and is driven by the ordinary removal and recovery paths.

// scheduleWorktreeAutoCleanup establishes the §9.4 automatic cleanup
// authority for one task inside the parent mutation's transaction. runID is
// the run this parent just terminalized for run-caused cleanup, or zero for
// the runless direct task-done cause. A task with no live worktree effect is
// a no-op. With an active bound source, a live linked removal is adopted
// unchanged and one unforced remove/pending saga is inserted otherwise; a
// run-caused row references the source run while a task-done row is runless.
// When the only live effect is the run's nonterminal provision, it is
// changed to cleanup-required instead, without advancing its recovery
// generation. A derived request id colliding with a nonidentical saga or
// receipt is corruption and fails the parent transaction.
func (t *tx) scheduleWorktreeAutoCleanup(tag, task string, runID int64, at string) error {
	source, err := t.activeBoundProvision(tag, task)
	if err != nil {
		return err
	}
	if source != nil {
		return t.scheduleLinkedRemoval(source, runID > 0, at)
	}
	return t.flipNonterminalProvisionCleanup(tag, task, runID, at)
}

// activeBoundProvision loads the task's active bound provision source, or
// nil when the task has none.
func (t *tx) activeBoundProvision(tag, task string) (*worktreeSagaState, error) {
	var id int64
	err := t.QueryRow(`SELECT saga_id FROM worktree_sagas
		WHERE tag=? AND task_id=? AND operation='provision' AND status='bound' AND binding_active=1`,
		tag, task).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read active worktree provision for %s/%s: %w", tag, task, err)
	}
	return loadWorktreeSaga(t, id)
}

// scheduleLinkedRemoval coalesces the automatic cleanup for one active bound
// source. A live linked removal already holds the cleanup authority and is
// adopted exactly as stored: adoption changes no immutable identity, request
// id, run/source edge, fence, or force bit, so a manual force bit in
// particular is never turned on or off. Otherwise one unforced remove/pending
// saga is inserted with the §9.4 automatic request id; a live saga already
// storing that exact identity is the coalesced repeat of an earlier parent
// decision. A terminal saga under that id — a scheduled cleanup that
// terminalized while the task was reopened — is a nonidentical collision:
// §9.4 adopts only live removals and never inserts a second row over a taken
// generated id, so the parent fails closed.
func (t *tx) scheduleLinkedRemoval(source *worktreeSagaState, runCaused bool, at string) error {
	// A live linked removal against the source is the adopted cleanup.
	var adopted int
	err := t.QueryRow(`SELECT 1 FROM worktree_sagas
		WHERE source_saga_id=? AND status IN ('pending','needs-broker','cleanup-required')`,
		source.id).Scan(&adopted)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read live worktree removal for saga %d: %w", source.id, err)
	}
	familyID, _, err := t.requireIdentity()
	if err != nil {
		return err
	}
	// A run-caused row references the source run; a direct task-done row is
	// runless. Both derive the request id from the same cause identity.
	rowRun := sql.NullInt64{}
	if runCaused {
		rowRun = source.run
	}
	requestID := automaticCleanupRequestID(familyID, source.id, rowRun.Int64)
	existing, err := loadWorktreeSagaByRequest(t, requestID)
	if err != nil {
		return err
	}
	if existing != nil {
		// Only a live cleanup coalesces. A terminal saga under the derived id
		// can be neither adopted — §9.4 adopts live linked removals only —
		// nor inserted over — the id is deterministic and already taken by
		// the terminal row and its original receipt — so it is corruption.
		if !autoCleanupMatchesSource(existing, source, rowRun) {
			return receiptIntegrityError(ReceiptKindWorktree)
		}
		return nil
	}
	var reserved int
	err = t.QueryRow(`SELECT 1 FROM receipts WHERE kind=? AND request_id=?`,
		ReceiptKindWorktree, requestID).Scan(&reserved)
	if err == nil {
		return receiptIntegrityError(ReceiptKindWorktree)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check worktree cleanup request reservation: %w", err)
	}
	fence, err := t.nextFence()
	if err != nil {
		return err
	}
	if _, err := t.Exec(`INSERT INTO worktree_sagas(
		run_id,source_saga_id,tag,task_id,operation,status,binding_active,saga_fence,
		request_id,recovery_generation,owner_boundary,registration_root,target_path,
		branch,base_commit,force,created_at,updated_at,extra)
		VALUES (?,?,?,?,'remove','pending',0,?,?,0,?,?,?,?,?,0,?,?,NULL)`,
		rowRun, source.id, source.tag, source.task, fence, requestID,
		source.boundary, source.registration.String, source.path, source.branch,
		source.base.String, at, at); err != nil {
		return fmt.Errorf("schedule worktree cleanup: %w", err)
	}
	return nil
}

// autoCleanupMatchesSource reports whether the saga found under a derived
// automatic request id is exactly the cleanup this parent would schedule
// against source: live, the same source edge, run or runless cause, immutable
// effect identity, and unforced. A terminal saga is never that cleanup — the
// parent establishes a live removal — so a terminal row (a failed cleanup,
// or a removed one beside a still-active source) or anything else under that
// id is corruption.
func autoCleanupMatchesSource(s, source *worktreeSagaState, rowRun sql.NullInt64) bool {
	return s != nil && !isTerminalWorktreeStatus(s.status) && s.operation == "remove" &&
		s.source.Valid && s.source.Int64 == source.id &&
		s.run.Valid == rowRun.Valid && (!rowRun.Valid || s.run.Int64 == rowRun.Int64) &&
		s.tag == source.tag && s.task == source.task && s.boundary == source.boundary &&
		s.registration == source.registration && s.path == source.path &&
		s.branch == source.branch && s.base == source.base && !s.force
}

// flipNonterminalProvisionCleanup applies §9.4's provision flip: when the
// only live effect is the run's nonterminal provision, the parent changes it
// to cleanup-required instead of inventing a removal source, without
// advancing its recovery generation. A run-caused parent flips provisions of
// exactly its terminalized run; the direct task-done cause flips the task's
// own provision, which a done task can never bind. Legacy rows and effects
// already cleanup-required are untouched.
func (t *tx) flipNonterminalProvisionCleanup(tag, task string, runID int64, at string) error {
	query := `UPDATE worktree_sagas SET status='cleanup-required',updated_at=?
		WHERE tag=? AND task_id=? AND operation='provision'
		AND status IN ('pending','needs-broker')`
	args := []any{at, tag, task}
	if runID > 0 {
		query += ` AND run_id=?`
		args = append(args, runID)
	}
	updated, err := t.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("flag worktree cleanup required for %s/%s: %w", tag, task, err)
	}
	n, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if n > 1 {
		return fmt.Errorf("worktree saga state invariant failed: %d live provisions for %s/%s", n, tag, task)
	}
	return nil
}
