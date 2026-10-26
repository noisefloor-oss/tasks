package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const worktreeSagaSchemaVersion = 5

// WorktreeSagaSchemaVersion is the store schema version that introduced
// the §9.4 worktree saga surface: the earliest store whose saga rows can
// prove worktree ownership.
const WorktreeSagaSchemaVersion = worktreeSagaSchemaVersion
const ReceiptKindWorktree = "worktree"

// worktreeSagaSchemaDDL is the exact additive schema-v5 catalog from SPEC
// §9.4. It intentionally stores only immutable effect identity, current saga
// state, the recovery fence, and the committed recovery generation.
const worktreeSagaSchemaDDL = `
CREATE TABLE worktree_sagas (
  saga_id                 INTEGER PRIMARY KEY AUTOINCREMENT CHECK(saga_id > 0),
  run_id                  INTEGER,
  source_saga_id          INTEGER,
  tag                     TEXT NOT NULL,
  task_id                 TEXT NOT NULL,
  operation               TEXT NOT NULL CHECK(operation IN ('provision','remove')),
  status                  TEXT NOT NULL CHECK(status IN ('pending','needs-broker','bound','removed','failed','cleanup-required')),
  binding_active          INTEGER NOT NULL CHECK(binding_active IN (0,1)),
  saga_fence              INTEGER NOT NULL UNIQUE CHECK(typeof(saga_fence) = 'integer' AND saga_fence > 0),
  request_id              TEXT NOT NULL UNIQUE,
  recovery_generation     INTEGER NOT NULL DEFAULT 0 CHECK(typeof(recovery_generation) = 'integer' AND recovery_generation >= 0),
  owner_boundary          TEXT NOT NULL,
  registration_root       TEXT,
  target_path             TEXT NOT NULL,
  branch                  TEXT NOT NULL,
  base_commit             TEXT,
  force                   INTEGER NOT NULL CHECK(force IN (0,1)),
  created_at              TEXT NOT NULL CHECK(length(created_at) > 0),
  updated_at              TEXT NOT NULL CHECK(length(updated_at) > 0),
  extra                   TEXT,
  FOREIGN KEY (run_id) REFERENCES runs(run_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  FOREIGN KEY (source_saga_id) REFERENCES worktree_sagas(saga_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  FOREIGN KEY (tag, task_id) REFERENCES tasks(tag, id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CHECK((operation = 'provision' AND status IN ('pending','needs-broker','bound','failed','cleanup-required')) OR
        (operation = 'remove' AND status IN ('pending','needs-broker','removed','failed','cleanup-required'))),
  CHECK(operation = 'remove' OR force = 0),
  CHECK(base_commit IS NULL OR
        (length(base_commit) IN (40,64) AND base_commit NOT GLOB '*[^0-9a-f]*')),
  CHECK(binding_active = 0 OR (operation = 'provision' AND status = 'bound')),
  CHECK((operation = 'provision' AND run_id IS NOT NULL AND source_saga_id IS NULL AND
         registration_root IS NOT NULL AND base_commit IS NOT NULL) OR
        (operation = 'remove' AND
         (source_saga_id IS NOT NULL OR
          (run_id IS NULL AND registration_root IS NULL AND base_commit IS NULL))))
);
CREATE UNIQUE INDEX worktree_sagas_one_active_binding
  ON worktree_sagas(tag, task_id) WHERE binding_active = 1;
CREATE UNIQUE INDEX worktree_sagas_one_live_effect
  ON worktree_sagas(tag, task_id)
  WHERE status IN ('pending','needs-broker','cleanup-required');
CREATE UNIQUE INDEX worktree_sagas_one_removed_source
  ON worktree_sagas(source_saga_id) WHERE status = 'removed';
CREATE INDEX worktree_sagas_run_history ON worktree_sagas(run_id, saga_id);
CREATE INDEX worktree_sagas_status ON worktree_sagas(status, saga_id);
`

type worktreeSagaState struct {
	id, fence, generation int64
	run, source           sql.NullInt64
	tag, task             string
	operation, status     string
	active, force         bool
	request, boundary     string
	registration          sql.NullString
	path, branch          string
	base                  sql.NullString
	created, updated      string
	extra                 sql.NullString
}

// worktreeSagaColumns is the exact §9.4 physical column list in scan order,
// shared by every worktree_sagas read.
const worktreeSagaColumns = `saga_id,run_id,source_saga_id,tag,task_id,operation,status,
	binding_active,saga_fence,request_id,recovery_generation,owner_boundary,
	registration_root,target_path,branch,base_commit,force,created_at,updated_at,extra`

// scanWorktreeSaga scans one worktree_sagas row in worktreeSagaColumns order
// through either a single-row or multi-row Scan.
func scanWorktreeSaga(scan func(dest ...any) error) (*worktreeSagaState, error) {
	s := &worktreeSagaState{}
	var active, force int64
	if err := scan(&s.id, &s.run, &s.source, &s.tag, &s.task, &s.operation, &s.status,
		&active, &s.fence, &s.request, &s.generation, &s.boundary, &s.registration,
		&s.path, &s.branch, &s.base, &force, &s.created, &s.updated, &s.extra); err != nil {
		return nil, err
	}
	s.active, s.force = active == 1, force == 1
	return s, nil
}

func canonicalAbsolutePath(v string) bool {
	return v != "" && filepath.IsAbs(v) && filepath.Clean(v) == v
}

func sagaInvariant(s *worktreeSagaState, format string, args ...any) error {
	return fmt.Errorf("worktree saga state invariant failed: saga %d "+format, append([]any{s.id}, args...)...)
}

func legacyWorktreeSagaRequestID(familyID, tag, task, path, branch string) string {
	h := sha256.New()
	_, _ = h.Write([]byte("noise-tasks-worktree-auto-v1\x00" + familyID + "\x00legacy\x00" + tag + "\x00" + task + "\x00" + path + "\x00" + branch))
	return "auto-" + hex.EncodeToString(h.Sum(nil))
}

func automaticCleanupRequestID(familyID string, source, run int64) string {
	cause := "task-done"
	if run != 0 {
		cause = "run:" + strconv.FormatInt(run, 10)
	}
	sum := sha256.Sum256([]byte("noise-tasks-worktree-auto-v1\x00" + familyID + "\x00" + strconv.FormatInt(source, 10) + "\x00" + cause))
	return "auto-" + hex.EncodeToString(sum[:])
}

func worktreeRecoveryRequestID(familyID string, sagaID, generation int64) (string, error) {
	if sagaID <= 0 || generation <= 0 {
		return "", fmt.Errorf("invalid worktree recovery identity")
	}
	sum := sha256.Sum256([]byte("noise-tasks-worktree-recover-v1\x00" + familyID + "\x00" + strconv.FormatInt(sagaID, 10) + "\x00" + strconv.FormatInt(generation, 10)))
	return "recover-" + hex.EncodeToString(sum[:]), nil
}

// upgradeLegacyWorktreeBindings converts v4's physical task bindings into
// runless cleanup-required removal sagas. It deliberately reads the complete
// ordered source set before inserting, allocates no fence for an empty set,
// and leaves every legacy wt_* field byte-identical.
func upgradeLegacyWorktreeBindings(t *tx) error {
	familyID, _, err := t.requireIdentity()
	if err != nil {
		return err
	}
	rows, err := t.Query(`SELECT tag,id,wt_boundary,wt_path,wt_branch,wt_created_at
		FROM tasks WHERE wt_path IS NOT NULL ORDER BY tag, CAST(id AS INTEGER), id`)
	if err != nil {
		return fmt.Errorf("read legacy worktree bindings: %w", err)
	}
	type binding struct{ tag, task, boundary, path, branch, created string }
	var bindings []binding
	for rows.Next() {
		var b binding
		if err := rows.Scan(&b.tag, &b.task, &b.boundary, &b.path, &b.branch, &b.created); err != nil {
			rows.Close()
			return fmt.Errorf("read legacy worktree binding: %w", err)
		}
		bindings = append(bindings, b)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, b := range bindings {
		fence, err := t.nextFence()
		if err != nil {
			return fmt.Errorf("allocate legacy worktree saga fence: %w", err)
		}
		requestID := legacyWorktreeSagaRequestID(familyID, b.tag, b.task, b.path, b.branch)
		if _, err := t.Exec(`INSERT INTO worktree_sagas(
			run_id,source_saga_id,tag,task_id,operation,status,binding_active,saga_fence,
			request_id,recovery_generation,owner_boundary,registration_root,target_path,
			branch,base_commit,force,created_at,updated_at,extra)
			VALUES(NULL,NULL,?,?,'remove','cleanup-required',0,?, ?,0,?,NULL,?,?,NULL,0,?,?,NULL)`,
			b.tag, b.task, fence, requestID, b.boundary, b.path, b.branch, b.created, b.created); err != nil {
			return fmt.Errorf("convert legacy worktree binding %s/%s: %w", b.tag, b.task, err)
		}
	}
	return nil
}

func worktreeSagaClassRecords(t *tx) ([][]byte, int, error) {
	rows, err := t.Query(`SELECT ` + worktreeSagaColumns + ` FROM worktree_sagas ORDER BY saga_id`)
	if err != nil {
		return nil, 0, fmt.Errorf("read worktree sagas: %w", err)
	}
	defer rows.Close()
	var records [][]byte
	for rows.Next() {
		s, err := scanWorktreeSaga(rows.Scan)
		if err != nil {
			return nil, 0, err
		}
		record, err := worktreeSagaRecord(s)
		if err != nil {
			return nil, 0, err
		}
		encoded, err := canonicalJSON(record)
		if err != nil {
			return nil, 0, err
		}
		records = append(records, encoded)
	}
	return records, len(records), rows.Err()
}

// ValidateWorktreeSagaState verifies every saga relation represented by the
// database. Git-common-directory equality and filesystem ownership are
// deliberately effect-time proofs and are not inferred here.
func (d *DB) ValidateWorktreeSagaState() error {
	return d.deferred(validateWorktreeSagaState)
}

func validateWorktreeSagaState(t *tx) error {
	var version int
	if err := t.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version < worktreeSagaSchemaVersion {
		return nil
	}
	familyID, familyBoundary, err := t.requireIdentity()
	if err != nil {
		return err
	}
	rows, err := t.Query(`SELECT ` + worktreeSagaColumns + ` FROM worktree_sagas ORDER BY saga_id`)
	if err != nil {
		return err
	}
	var ordered []*worktreeSagaState
	sagas := map[int64]*worktreeSagaState{}
	for rows.Next() {
		s, err := scanWorktreeSaga(rows.Scan)
		if err != nil {
			rows.Close()
			return err
		}
		ordered, sagas[s.id] = append(ordered, s), s
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, s := range ordered {
		if err := validateWorktreeSagaRow(t, familyID, familyBoundary, s, sagas); err != nil {
			return err
		}
	}
	if err := validateWorktreeSourceActivity(sagas); err != nil {
		return err
	}
	if err := validateWorktreeReceipts(t, familyID, ordered, sagas); err != nil {
		return err
	}
	var raw string
	if err := t.QueryRow(`SELECT value FROM meta WHERE key='fencing'`).Scan(&raw); err != nil {
		return err
	}
	counter, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || counter < 0 || strconv.FormatInt(counter, 10) != raw {
		return fmt.Errorf("worktree saga state invariant failed: fencing counter is not canonical")
	}
	var max sql.NullInt64
	if err := t.QueryRow(`SELECT MAX(saga_fence) FROM worktree_sagas`).Scan(&max); err != nil {
		return err
	}
	if max.Valid && counter < max.Int64 {
		return fmt.Errorf("worktree saga state invariant failed: fencing counter is below saga fence %d", max.Int64)
	}
	return nil
}

func validateWorktreeSagaRow(t *tx, familyID, familyBoundary string, s *worktreeSagaState, sagas map[int64]*worktreeSagaState) error {
	created, ce := time.Parse(time.RFC3339, s.created)
	updated, ue := time.Parse(time.RFC3339, s.updated)
	if ce != nil || ue != nil || updated.Before(created) {
		return sagaInvariant(s, "has incoherent timestamps")
	}
	if !ValidLocalKey(s.request) || strings.HasPrefix(s.request, "recover-") {
		return sagaInvariant(s, "has invalid or recovery-reserved request id")
	}
	legacy := !s.run.Valid && !s.source.Valid
	if !canonicalAbsolutePath(s.boundary) || !canonicalAbsolutePath(s.path) ||
		s.path != filepath.Join(s.boundary, ".worktrees", WorktreeName(s.tag, s.task)) || s.branch != WorktreeBranch(s.tag, s.task) {
		return sagaInvariant(s, "has noncanonical effect paths")
	}
	if !legacy && s.boundary != familyBoundary {
		return sagaInvariant(s, "does not use the family owner boundary")
	}
	if s.registration.Valid && (!canonicalAbsolutePath(s.registration.String) || filepath.Base(s.registration.String) != "worktrees") {
		return sagaInvariant(s, "has noncanonical registration root")
	}
	if s.base.Valid && len(s.base.String) != 40 && len(s.base.String) != 64 {
		return sagaInvariant(s, "has invalid object-id width")
	}
	if legacy {
		if s.operation != "remove" || s.registration.Valid || s.base.Valid || s.force ||
			s.request != legacyWorktreeSagaRequestID(familyID, s.tag, s.task, s.path, s.branch) ||
			(s.generation == 0 && (s.status != "cleanup-required" || s.updated != s.created)) ||
			(s.generation > 0 && s.status != "cleanup-required" && s.status != "needs-broker" && s.status != "removed" && s.status != "failed") {
			return sagaInvariant(s, "has incoherent legacy identity")
		}
		var boundary, path, branch, at sql.NullString
		if err := t.QueryRow(`SELECT wt_boundary,wt_path,wt_branch,wt_created_at FROM tasks WHERE tag=? AND id=?`, s.tag, s.task).
			Scan(&boundary, &path, &branch, &at); err != nil {
			return err
		}
		if s.status == "removed" {
			if boundary.Valid || path.Valid || branch.Valid || at.Valid {
				return sagaInvariant(s, "is removed but retains legacy binding")
			}
		} else if !boundary.Valid || boundary.String != s.boundary || !path.Valid || path.String != s.path ||
			!branch.Valid || branch.String != s.branch || !at.Valid || at.String != s.created {
			return sagaInvariant(s, "does not match retained legacy binding")
		}
		_, err := worktreeSagaRecord(s)
		return err
	}
	if strings.HasPrefix(s.request, "auto-") {
		if s.operation != "remove" || !s.source.Valid || s.request != automaticCleanupRequestID(familyID, s.source.Int64, s.run.Int64) {
			return sagaInvariant(s, "has invalid automatic request id")
		}
		if s.run.Valid {
			var status string
			if err := t.QueryRow(`SELECT status FROM runs WHERE run_id=?`, s.run.Int64).Scan(&status); err != nil || (status != "succeeded" && status != "cancelled") {
				return sagaInvariant(s, "has nonterminal automatic run cause")
			}
		}
	}
	if s.operation == "provision" {
		var tag, task string
		if err := t.QueryRow(`SELECT tag,task_id FROM runs WHERE run_id=?`, s.run.Int64).Scan(&tag, &task); err != nil || tag != s.tag || task != s.task {
			return sagaInvariant(s, "does not match owning run")
		}
	} else if s.source.Valid {
		source := sagas[s.source.Int64]
		if source == nil || source.id >= s.id || source.operation != "provision" || source.status != "bound" ||
			source.tag != s.tag || source.task != s.task || source.boundary != s.boundary ||
			source.registration != s.registration || source.path != s.path || source.branch != s.branch || source.base != s.base ||
			(s.run.Valid && (!source.run.Valid || s.run.Int64 != source.run.Int64)) {
			return sagaInvariant(s, "does not match bound provision source")
		}
		sourceUpdated, _ := time.Parse(time.RFC3339, source.updated)
		if created.Before(sourceUpdated) {
			return sagaInvariant(s, "predates provision source")
		}
		if !s.run.Valid && !strings.HasPrefix(s.request, "auto-") {
			return sagaInvariant(s, "runless linked removal is not automatic")
		}
	}
	_, err := worktreeSagaRecord(s)
	return err
}

func validateWorktreeSourceActivity(sagas map[int64]*worktreeSagaState) error {
	removed := make(map[int64]int)
	removedID := make(map[int64]int64)
	for _, child := range sagas {
		if !child.source.Valid {
			continue
		}
		source := sagas[child.source.Int64]
		if source == nil {
			continue
		}
		if child.status == "removed" {
			removed[source.id]++
			removedID[source.id] = child.id
		}
		if (child.status == "pending" || child.status == "needs-broker" || child.status == "cleanup-required") && !source.active {
			return sagaInvariant(child, "is live against inactive source")
		}
	}
	for _, child := range sagas {
		if child.source.Valid && removedID[child.source.Int64] != 0 && child.id > removedID[child.source.Int64] {
			return sagaInvariant(child, "was planned after its source was removed")
		}
	}
	for _, source := range sagas {
		if source.operation != "provision" || source.status != "bound" {
			continue
		}
		count := removed[source.id]
		if count > 1 || source.active != (count == 0) {
			return sagaInvariant(source, "has activity inconsistent with removed child")
		}
	}
	return nil
}

func worktreeSagaRecord(s *worktreeSagaState) (map[string]any, error) {
	record := map[string]any{
		"sagaId": s.id, "runId": nullableInt64(s.run), "sourceSagaId": nullableInt64(s.source),
		"tag": s.tag, "task": s.task, "operation": s.operation, "status": s.status,
		"bindingActive": s.active, "sagaFence": s.fence, "requestId": s.request,
		"recoveryGeneration": s.generation, "ownerBoundary": s.boundary,
		"registrationRoot": nullableString(s.registration), "path": s.path, "branch": s.branch,
		"baseCommit": nullableString(s.base), "force": s.force, "createdAt": s.created, "updatedAt": s.updated,
	}
	if err := mergeExtra(record, s.extra); err != nil {
		return nil, sagaInvariant(s, "has invalid extra: %v", err)
	}
	return record, nil
}

var worktreeReceiptFields = map[string]bool{
	"schemaVersion": true, "kind": true, "receiptId": true, "familyId": true,
	"requestId": true, "inputDigest": true, "resultDigest": true, "operation": true,
	"sagaId": true, "sourceSagaId": true, "tag": true, "task": true, "runId": true,
	"owner": true, "status": true, "active": true, "ownerBoundary": true, "path": true,
	"branch": true, "baseCommit": true, "force": true, "recoveryGeneration": true,
	"inventory": true, "eventSeq": true, "committedAt": true, "receiptDigest": true,
}

type worktreeReceiptState struct {
	generation int64
	status     string
	recovery   bool
}

func validateWorktreeReceipts(t *tx, familyID string, ordered []*worktreeSagaState, sagas map[int64]*worktreeSagaState) error {
	originals := map[string]*worktreeSagaState{}
	for _, s := range ordered {
		originals[s.request] = s
	}
	rows, err := t.Query(`SELECT request_id FROM receipts WHERE kind=? ORDER BY receipt_id`, ReceiptKindWorktree)
	if err != nil {
		return err
	}
	var requests []string
	for rows.Next() {
		var request sql.NullString
		if err := rows.Scan(&request); err != nil || !request.Valid {
			rows.Close()
			return receiptIntegrityError(ReceiptKindWorktree)
		}
		requests = append(requests, request.String)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	recoveryBySaga := map[int64]map[int64]worktreeReceiptState{}
	originalSeen := map[int64]bool{}
	for _, request := range requests {
		receipt, err := t.findVerifiedReceipt(ReceiptKindWorktree, request)
		if err != nil {
			return err
		}
		if len(receipt) != len(worktreeReceiptFields) {
			return receiptIntegrityError(ReceiptKindWorktree)
		}
		for key := range receipt {
			if !worktreeReceiptFields[key] {
				return receiptIntegrityError(ReceiptKindWorktree)
			}
		}
		sagaID, ok := receiptInt64(receipt["sagaId"])
		if !ok || sagas[sagaID] == nil {
			return receiptIntegrityError(ReceiptKindWorktree)
		}
		s := sagas[sagaID]
		generation, ok := receiptInt64(receipt["recoveryGeneration"])
		if !ok || generation < 0 || generation > s.generation {
			return receiptIntegrityError(ReceiptKindWorktree)
		}
		original := request == s.request
		if reserved := originals[request]; reserved != nil && reserved.id != s.id {
			return receiptIntegrityError(ReceiptKindWorktree)
		}
		if !original {
			want, err := worktreeRecoveryRequestID(familyID, s.id, generation)
			if err != nil || generation == 0 || request != want {
				return receiptIntegrityError(ReceiptKindWorktree)
			}
		}
		status, ok := receipt["status"].(string)
		if !ok || !worktreeReceiptStatusAllowed(s.operation, status) {
			return receiptIntegrityError(ReceiptKindWorktree)
		}
		if err := validateWorktreeReceipt(t, receipt, request, s, generation, original); err != nil {
			return err
		}
		if original {
			if originalSeen[s.id] || generation != s.generation || (s.status != "bound" && s.status != "removed" && s.status != "failed") || status != s.status {
				return receiptIntegrityError(ReceiptKindWorktree)
			}
			originalSeen[s.id] = true
		} else {
			if recoveryBySaga[s.id] == nil {
				recoveryBySaga[s.id] = map[int64]worktreeReceiptState{}
			}
			if _, duplicate := recoveryBySaga[s.id][generation]; duplicate {
				return receiptIntegrityError(ReceiptKindWorktree)
			}
			recoveryBySaga[s.id][generation] = worktreeReceiptState{generation, status, true}
		}
	}
	for _, s := range ordered {
		terminal := s.status == "bound" || s.status == "removed" || s.status == "failed"
		if terminal != originalSeen[s.id] {
			return sagaInvariant(s, "terminal/original receipt linkage is inconsistent")
		}
		history := recoveryBySaga[s.id]
		count := int64(len(history))
		if count != s.generation {
			return sagaInvariant(s, "recovery receipt count does not equal generation")
		}
		previous := ""
		for generation := int64(1); generation <= count; generation++ {
			point, exists := history[generation]
			if !exists || generation < count && isTerminalWorktreeStatus(point.status) {
				return sagaInvariant(s, "recovery receipt generations or outcomes are impossible")
			}
			if s.operation == "provision" && previous == "cleanup-required" && point.status != "cleanup-required" && point.status != "failed" {
				return sagaInvariant(s, "provision recovery escaped cleanup-only state")
			}
			previous = point.status
		}
		if count > 0 {
			last := history[count].status
			if last != s.status && !(s.operation == "provision" && last == "needs-broker" && s.status == "cleanup-required") {
				return sagaInvariant(s, "current state is not justified by latest recovery receipt")
			}
		}
	}
	return nil
}

func validateWorktreeReceipt(t *tx, r map[string]any, request string, s *worktreeSagaState, generation int64, original bool) error {
	owner := ""
	if s.run.Valid {
		if err := t.QueryRow(`SELECT owner FROM runs WHERE run_id=?`, s.run.Int64).Scan(&owner); err != nil {
			return err
		}
	}
	status, _ := r["status"].(string)
	active, activeOK := r["active"].(bool)
	committed, committedOK := r["committedAt"].(string)
	if !activeOK || !committedOK || !validRunTimestamp(committed) ||
		r["operation"] != s.operation || r["tag"] != s.tag || r["task"] != s.task || r["owner"] != owner ||
		r["ownerBoundary"] != s.boundary || r["path"] != s.path || r["branch"] != s.branch || r["force"] != s.force ||
		!nullableReceiptInt(r["runId"], s.run) || !nullableReceiptInt(r["sourceSagaId"], s.source) || !nullableReceiptString(r["baseCommit"], s.base) {
		return receiptIntegrityError(ReceiptKindWorktree)
	}
	if active != worktreeReceiptActive(s, status) {
		return receiptIntegrityError(ReceiptKindWorktree)
	}
	inputOwner, inputOperation, inputRun, inputSaga := owner, s.operation, int64(0), int64(0)
	if s.run.Valid {
		inputRun = s.run.Int64
	}
	input := map[string]any{"force": s.force, "operation": inputOperation, "owner": inputOwner, "requestId": request, "runId": inputRun, "sagaId": inputSaga}
	if strings.HasPrefix(s.request, "auto-") {
		input["owner"] = ""
	}
	if !original {
		input = map[string]any{"force": s.force, "operation": "recover", "recoveryGeneration": generation, "requestId": request, "runId": int64(0), "sagaId": s.id}
	}
	inputDigest, err := canonicalDigest(input)
	if err != nil || r["inputDigest"] != inputDigest {
		return receiptIntegrityError(ReceiptKindWorktree)
	}
	resultDigest, err := worktreeResultDigest(s, status, active, committed, generation)
	if err != nil || r["resultDigest"] != resultDigest {
		return receiptIntegrityError(ReceiptKindWorktree)
	}
	sum := sha256.Sum256([]byte("noise-tasks-worktree-receipt-v1\x00" + request + "\x00" + inputDigest))
	if r["receiptId"] != "worktree-"+hex.EncodeToString(sum[:]) {
		return receiptIntegrityError(ReceiptKindWorktree)
	}
	inv, ok := r["inventory"].(map[string]any)
	if !ok {
		return receiptIntegrityError(ReceiptKindWorktree)
	}
	parsed, err := parseInventory(inv)
	if err != nil || parsed.WorktreeSagas < 1 || parsed.Receipts < 1 {
		return receiptIntegrityError(ReceiptKindWorktree)
	}
	seq, ok := r["eventSeq"].(map[string]any)
	if !ok || len(seq) != 2 {
		return receiptIntegrityError(ReceiptKindWorktree)
	}
	first, fok := receiptInt64(seq["first"])
	last, lok := receiptInt64(seq["last"])
	if !fok || !lok || first <= 0 || first != last {
		return receiptIntegrityError(ReceiptKindWorktree)
	}
	return nil
}

func canonicalDigest(v map[string]any) (string, error) {
	b, err := canonicalJSON(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func nullableReceiptInt(v any, want sql.NullInt64) bool {
	if !want.Valid {
		return v == nil
	}
	got, ok := receiptInt64(v)
	return ok && got == want.Int64
}

func nullableReceiptString(v any, want sql.NullString) bool {
	if !want.Valid {
		return v == nil
	}
	got, ok := v.(string)
	return ok && got == want.String
}

func worktreeReceiptStatusAllowed(operation, status string) bool {
	return status == "needs-broker" || status == "cleanup-required" || status == "failed" ||
		operation == "provision" && status == "bound" || operation == "remove" && status == "removed"
}

func isTerminalWorktreeStatus(status string) bool {
	return status == "bound" || status == "removed" || status == "failed"
}
