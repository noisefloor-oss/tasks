package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The shared §9.4 original-operation projection and receipt surface. One
// input-digest builder, one result projection, one original-receipt writer,
// and one original-receipt validator serve both the provision and removal
// paths; every operation-dependent member is derived from the saga row so
// recovery can reuse the same code unchanged.

// worktreeInputDigest builds the §9.4 original input digest over exactly
// force, operation, owner, requestId, runId, and sagaId 0. Provision passes
// false/"provision"; removal passes its force bit and "remove". An automatic
// request id carries no owner authority, so its digest uses an empty owner
// and the recorded run id or zero (SPEC §9.4) — the same rule the receipt
// validator applies, kept in the one builder the writer and every original
// replay comparison share.
func worktreeInputDigest(owner, requestID string, runID int64, force bool, operation string) (string, error) {
	if strings.HasPrefix(requestID, "auto-") {
		owner = ""
	}
	return canonicalDigest(map[string]any{
		"force": force, "operation": operation, "owner": owner,
		"requestId": requestID, "runId": runID, "sagaId": int64(0),
	})
}

func worktreeReceiptID(requestID, inputDigest string) string {
	sum := sha256.Sum256([]byte("noise-tasks-worktree-receipt-v1\x00" + requestID + "\x00" + inputDigest))
	return "worktree-" + hex.EncodeToString(sum[:])
}

// worktreeReceiptActive is the §9.4 activity bit for one observed saga
// outcome: a bound provision is active, and a linked removal stays active
// unless it completed. The writer and the receipt validator share this one
// derivation.
func worktreeReceiptActive(s *worktreeSagaState, status string) bool {
	return s.operation == "provision" && status == "bound" ||
		s.operation == "remove" && s.source.Valid && status != "removed"
}

// worktreeResultDigest builds the exact §9.4 result digest for one observed
// saga outcome. The immutable identity members come from the saga row; the
// observed status, activity, commit time, and generation come from the
// outcome being written or the receipt being verified.
func worktreeResultDigest(s *worktreeSagaState, status string, active bool, committedAt string, generation int64) (string, error) {
	return canonicalDigest(map[string]any{
		"active": active, "baseCommit": nullableString(s.base), "branch": s.branch,
		"committedAt": committedAt, "force": s.force, "operation": s.operation,
		"ownerBoundary": s.boundary, "path": s.path, "recoveryGeneration": generation,
		"runId": nullableInt64(s.run), "sagaId": s.id, "status": status,
		"tag": s.tag, "task": s.task,
	})
}

// worktreeBrokerRequest renders the §9.4 non-secret broker mint bindings for
// one needs-broker handoff from the saga row's immutable identity: verb
// create for provision, remove for a linked removal, and prune-registration
// for a runless sourceless legacy row, whose registration root the schema
// cannot store — it stays empty here and is derived at effect time: the
// recovery commit threads the trusted host's observed root onto the fresh
// handoff, and the prune snapshot derives it for every re-emission
// (SPEC §9.4).
func worktreeBrokerRequest(familyID string, s *worktreeSagaState, base *string, recoverID string) *WorktreeBrokerRequest {
	verb := "remove"
	if s.operation == "provision" {
		verb = "create"
	} else if !s.run.Valid && !s.source.Valid {
		verb = "prune-registration"
	}
	return &WorktreeBrokerRequest{FamilyID: familyID, Verb: verb,
		RegistrationRoot: s.registration.String, OwnerBoundary: s.boundary,
		TargetPath: s.path, Branch: s.branch, BaseCommit: base, Force: s.force,
		RecoverRequestID: recoverID}
}

// worktreeResultFromSaga renders the exact §9.4 provision/remove mutation
// projection from the current saga row. The broker verb and force bit derive
// from the row, whose schema pins provision force to zero.
func worktreeResultFromSaga(t *tx, s *worktreeSagaState, sagaToken int64) (*WorktreeMutationResult, error) {
	familyID, _, err := t.requireIdentity()
	if err != nil {
		return nil, err
	}
	var runID *int64
	if s.run.Valid {
		id := s.run.Int64
		runID = &id
	}
	var base *string
	if s.base.Valid {
		value := s.base.String
		base = &value
	}
	result := &WorktreeMutationResult{SagaID: s.id, Operation: s.operation, Status: s.status,
		RunID: runID, OwnerBoundary: s.boundary, Path: s.path, Branch: s.branch,
		BaseCommit: base, SagaToken: sagaToken, RecoveryGeneration: s.generation}
	if !isTerminalWorktreeStatus(s.status) && s.generation < math.MaxInt64 {
		recoverID, err := worktreeRecoveryRequestID(familyID, s.id, s.generation+1)
		if err != nil {
			return nil, err
		}
		result.RecoverRequestID = &recoverID
		if s.status == "needs-broker" {
			result.BrokerRequest = worktreeBrokerRequest(familyID, s, base, recoverID)
		}
	}
	return result, nil
}

func worktreeResultFromReceipt(receipt map[string]any, sagaToken int64) (*WorktreeMutationResult, error) {
	sagaID, sagaOK := receiptInt64(receipt["sagaId"])
	generation, generationOK := receiptInt64(receipt["recoveryGeneration"])
	operation, operationOK := receipt["operation"].(string)
	status, statusOK := receipt["status"].(string)
	boundary, boundaryOK := receipt["ownerBoundary"].(string)
	path, pathOK := receipt["path"].(string)
	branch, branchOK := receipt["branch"].(string)
	if !sagaOK || !generationOK || !operationOK || !statusOK ||
		!boundaryOK || !pathOK || !branchOK {
		return nil, receiptIntegrityError(ReceiptKindWorktree)
	}
	// Nullable runId and baseCommit members: a runless or base-less row (a
	// §9.4 legacy cleanup saga) renders null, not a zero value.
	var runID *int64
	if id, ok := receiptInt64(receipt["runId"]); ok {
		runID = &id
	} else if receipt["runId"] != nil {
		return nil, receiptIntegrityError(ReceiptKindWorktree)
	}
	var base *string
	if value, ok := receipt["baseCommit"].(string); ok {
		base = &value
	} else if receipt["baseCommit"] != nil {
		return nil, receiptIntegrityError(ReceiptKindWorktree)
	}
	return &WorktreeMutationResult{SagaID: sagaID, Operation: operation, Status: status,
		RunID: runID, OwnerBoundary: boundary, Path: path, Branch: branch,
		BaseCommit: base, SagaToken: sagaToken, RecoveryGeneration: generation}, nil
}

func worktreeEventExtra(sagaID int64, receiptID, digest string) map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"sagaId":        json.RawMessage(strconv.FormatInt(sagaID, 10)),
		"receiptId":     json.RawMessage(strconv.Quote(receiptID)),
		"receiptDigest": json.RawMessage(strconv.Quote(digest)),
	}
}

// validateOriginalWorktreeReceipt verifies a terminal original receipt
// against the saga row that reserved it. The operation consistency comes from
// the row itself through validateWorktreeReceipt.
func validateOriginalWorktreeReceipt(t *tx, receipt map[string]any, s *worktreeSagaState, inputDigest string) error {
	if len(receipt) != len(worktreeReceiptFields) || !isTerminalWorktreeStatus(s.status) ||
		receipt["status"] != s.status || receipt["inputDigest"] != inputDigest {
		return receiptIntegrityError(ReceiptKindWorktree)
	}
	for key := range receipt {
		if !worktreeReceiptFields[key] {
			return receiptIntegrityError(ReceiptKindWorktree)
		}
	}
	return validateWorktreeReceipt(t, receipt, s.request, s, s.generation, true)
}

// writeOriginalWorktreeReceipt writes the original operation's terminal
// receipt and its single covering event. Operation, force, source edge, and
// activity derive from the saga row, so provision and removal share one
// writer with one byte-exact receipt shape.
func writeOriginalWorktreeReceipt(t *tx, s *worktreeSagaState, owner, committedAt string) error {
	familyID, _, err := t.requireIdentity()
	if err != nil {
		return err
	}
	inputDigest, err := worktreeInputDigest(owner, s.request, s.run.Int64, s.force, s.operation)
	if err != nil {
		return err
	}
	receiptID := worktreeReceiptID(s.request, inputDigest)
	active := worktreeReceiptActive(s, s.status)
	resultDigest, err := worktreeResultDigest(s, s.status, active, committedAt, s.generation)
	if err != nil {
		return err
	}
	inv, err := countClassInventory(t)
	if err != nil {
		return err
	}
	if inv.Events < MaxRetainedEvents {
		inv.Events++
	}
	inv.Receipts++
	var seq int64
	if err := t.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='events'`).Scan(&seq); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read worktree event sequence: %w", err)
	}
	seq++
	receipt := map[string]any{
		"schemaVersion": int64(ReceiptSchemaVersion), "kind": ReceiptKindWorktree,
		"receiptId": receiptID, "familyId": familyID, "requestId": s.request,
		"inputDigest": inputDigest, "resultDigest": resultDigest,
		"operation": s.operation, "sagaId": s.id, "sourceSagaId": nullableInt64(s.source),
		"tag": s.tag, "task": s.task, "runId": nullableInt64(s.run), "owner": owner,
		"status": s.status, "active": active, "ownerBoundary": s.boundary,
		"path": s.path, "branch": s.branch, "baseCommit": nullableString(s.base),
		"force": s.force, "recoveryGeneration": s.generation,
		"inventory": inventoryRecord(inv), "eventSeq": map[string]any{"first": seq, "last": seq},
		"committedAt": committedAt,
	}
	digest, err := receiptDigest(receipt)
	if err != nil {
		return err
	}
	if err := t.appendEvent(Event{Task: s.task, Tag: s.tag, Kind: ReceiptKindWorktree,
		Action: s.status, Owner: owner, At: committedAt,
		Extra: worktreeEventExtra(s.id, receiptID, digest)}); err != nil {
		return err
	}
	var actual int64
	if err := t.QueryRow(`SELECT last_insert_rowid()`).Scan(&actual); err != nil || actual != seq {
		if err != nil {
			return err
		}
		return fmt.Errorf("worktree event sequence changed from %d to %d", seq, actual)
	}
	_, err = t.insertReceipt(ReceiptKindWorktree, receiptID, s.request, receipt, committedAt)
	return err
}
