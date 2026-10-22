package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func writeFinishRunReceipt(t *tx, r runRequest, inputDigest string, task *Task, claim *Claim,
	state *currentRunAttempt, result *RunFinishResult, committedAt string) error {
	familyID, _, err := t.requireIdentity()
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
	receiptID := runReceiptID(r.requestID, inputDigest)
	resultValue := runResultValue{
		Attempt: state.CurrentAttempt, AttemptID: state.AttemptID, AttemptExpires: state.AttemptExpiresAt,
		AttemptStatus: result.AttemptStatus, ClaimID: claimAcquisitionID(claim), ClaimExpires: claim.ExpiresAt,
		CommittedAt: committedAt, LeaseGeneration: state.LeaseGeneration, Operation: "finish", Owner: r.owner,
		RunID: state.RunID, RunStatus: result.RunStatus, Tag: task.Tag, Task: task.ID,
		TaskRevision: task.Revision, TaskStatus: task.Status, TerminalDigest: r.terminalResultDigest,
	}
	resultDigest, err := runResultDigest(resultValue)
	if err != nil {
		return err
	}
	var seq int64
	if err := t.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='events'`).Scan(&seq); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read run event sequence: %w", err)
	}
	seq++
	receipt := map[string]any{
		"schemaVersion": int64(ReceiptSchemaVersion), "kind": ReceiptKindRun,
		"receiptId": receiptID, "familyId": familyID, "requestId": r.requestID,
		"inputDigest": inputDigest, "resultDigest": resultDigest,
		"operation": "finish", "tag": task.Tag, "task": task.ID, "runId": state.RunID,
		"owner": r.owner, "sourceAttempt": r.attempt, "attempt": state.CurrentAttempt,
		"ttlSeconds": int64(0), "outcome": r.outcome, "revision": int64(r.revision),
		"taskStatus": r.taskStatus, "taskRevision": int64(task.Revision),
		"terminalResultDigest": r.terminalResultDigest, "force": false,
		"claimAcquisitionId": claimAcquisitionID(claim), "attemptAcquisitionId": state.AttemptID,
		"leaseGeneration": state.LeaseGeneration, "claimExpiresAt": claim.ExpiresAt,
		"attemptExpiresAt": state.AttemptExpiresAt, "runStatus": result.RunStatus,
		"attemptStatus": result.AttemptStatus, "inventory": inventoryRecord(inv),
		"resultTaskStatus": task.Status, "eventSeq": map[string]any{"first": seq, "last": seq},
		"committedAt": committedAt,
	}
	digest, err := receiptDigest(receipt)
	if err != nil {
		return err
	}
	if err := t.appendEvent(Event{Task: task.ID, Tag: task.Tag, Kind: ReceiptKindRun, Action: "finished",
		Owner: r.owner, At: committedAt, Extra: runEventExtra(state.RunID, state.CurrentAttempt, receiptID, digest)}); err != nil {
		return err
	}
	var actualSeq int64
	if err := t.QueryRow(`SELECT last_insert_rowid()`).Scan(&actualSeq); err != nil {
		return fmt.Errorf("read run event sequence: %w", err)
	}
	if actualSeq != seq {
		return fmt.Errorf("run event sequence changed from %d to %d", seq, actualSeq)
	}
	_, err = t.insertReceipt(ReceiptKindRun, receiptID, r.requestID, receipt, committedAt)
	return err
}

func validateFinishRunReceipt(t *tx, receipt map[string]any, r runRequest, inputDigest string) error {
	if err := validateRunReceiptRequest(receipt, r, inputDigest); err != nil {
		return err
	}
	tag, tagOK := receipt["tag"].(string)
	taskID, taskOK := receipt["task"].(string)
	resultTaskStatus, taskStatusOK := receipt["resultTaskStatus"].(string)
	runStatus := "retryable"
	if r.outcome == "succeeded" {
		runStatus = "succeeded"
	}
	if receipt["operation"] != "finish" || receipt["owner"] != r.owner || !tagOK || tag == "" ||
		!taskOK || taskID == "" || !taskStatusOK || !validStatuses[resultTaskStatus] ||
		receipt["outcome"] != r.outcome || receipt["taskStatus"] != r.taskStatus ||
		receipt["terminalResultDigest"] != r.terminalResultDigest || receipt["force"] != false ||
		receipt["runStatus"] != runStatus || receipt["attemptStatus"] != r.outcome {
		return receiptIntegrityError(ReceiptKindRun)
	}
	runID, runOK := receiptInt64(receipt["runId"])
	source, sourceOK := receiptInt64(receipt["sourceAttempt"])
	attempt, attemptOK := receiptInt64(receipt["attempt"])
	ttl, ttlOK := receiptInt64(receipt["ttlSeconds"])
	revision, revisionOK := receiptInt64(receipt["revision"])
	taskRevision, taskRevisionOK := receiptInt64(receipt["taskRevision"])
	generation, generationOK := receiptInt64(receipt["leaseGeneration"])
	attemptID, attemptIDOK := receipt["attemptAcquisitionId"].(string)
	claimID, claimIDOK := receipt["claimAcquisitionId"].(string)
	claimExpires, claimExpiresOK := receipt["claimExpiresAt"].(string)
	attemptExpires, attemptExpiresOK := receipt["attemptExpiresAt"].(string)
	committedAt, committedOK := receipt["committedAt"].(string)
	if !runOK || runID != r.runID || !sourceOK || source != r.attempt || !attemptOK || attempt != r.attempt ||
		!ttlOK || ttl != 0 || !revisionOK || revision != int64(r.revision) || !taskRevisionOK || taskRevision < 1 ||
		!generationOK || generation < 1 || !attemptIDOK || !validStoredAcquisitionID(attemptID) ||
		!claimIDOK || !validStoredAcquisitionID(claimID) || !claimExpiresOK || !attemptExpiresOK || !committedOK {
		return receiptIntegrityError(ReceiptKindRun)
	}
	if r.taskStatus != "" && (resultTaskStatus != r.taskStatus || taskRevision != int64(r.revision+1)) {
		return receiptIntegrityError(ReceiptKindRun)
	}
	if _, err := time.Parse(time.RFC3339, claimExpires); err != nil {
		return receiptIntegrityError(ReceiptKindRun)
	}
	claimExpiry, _ := time.Parse(time.RFC3339, claimExpires)
	attemptExpiry, err := time.Parse(time.RFC3339, attemptExpires)
	if err != nil {
		return receiptIntegrityError(ReceiptKindRun)
	}
	committedTime, err := time.Parse(time.RFC3339, committedAt)
	if err != nil || !committedTime.Before(claimExpiry) || !committedTime.Before(attemptExpiry) {
		return receiptIntegrityError(ReceiptKindRun)
	}
	resultDigest, ok := receipt["resultDigest"].(string)
	wantResult, err := runResultDigest(runResultValue{Attempt: attempt, AttemptID: attemptID,
		AttemptExpires: attemptExpires, AttemptStatus: r.outcome, ClaimID: claimID, ClaimExpires: claimExpires,
		CommittedAt: committedAt, LeaseGeneration: generation, Operation: "finish", Owner: r.owner,
		RunID: runID, RunStatus: runStatus, Tag: tag, Task: taskID, TaskRevision: int(taskRevision),
		TaskStatus: resultTaskStatus, TerminalDigest: r.terminalResultDigest})
	if !ok || err != nil || resultDigest != wantResult {
		return receiptIntegrityError(ReceiptKindRun)
	}
	invObj, ok := receipt["inventory"].(map[string]any)
	if !ok {
		return receiptIntegrityError(ReceiptKindRun)
	}
	inv, err := parseInventory(invObj)
	if err != nil || inv.Runs < 1 || int64(inv.Attempts) < attempt || inv.Receipts < 1 || inv.Events < 1 {
		return receiptIntegrityError(ReceiptKindRun)
	}
	return validateRunReceiptEvent(t, receipt, runID, attempt, ReceiptKindRun, "finished", committedAt)
}

func lookupFinishRunReplay(t *tx, r runRequest, inputDigest string) (*RunFinishResult, bool, error) {
	receipt, err := t.findVerifiedReceipt(ReceiptKindRun, r.requestID)
	if err != nil {
		return nil, false, err
	}
	if receipt == nil {
		return nil, false, nil
	}
	if err := validateFinishRunReceipt(t, receipt, r, inputDigest); err != nil {
		return nil, false, err
	}
	runID, _ := receiptInt64(receipt["runId"])
	attempt, _ := receiptInt64(receipt["attempt"])
	generation, _ := receiptInt64(receipt["leaseGeneration"])
	taskRevision, _ := receiptInt64(receipt["taskRevision"])
	tag, _ := receipt["tag"].(string)
	taskID, _ := receipt["task"].(string)
	attemptID, _ := receipt["attemptAcquisitionId"].(string)
	attemptExpires, _ := receipt["attemptExpiresAt"].(string)
	committedAt, _ := receipt["committedAt"].(string)
	resultTaskStatus, _ := receipt["resultTaskStatus"].(string)
	runStatus, _ := receipt["runStatus"].(string)
	attemptStatus, _ := receipt["attemptStatus"].(string)

	claimID, _ := receipt["claimAcquisitionId"].(string)
	var storedTag, storedTask, storedOwner, storedClaimID, storedRunStatus, storedRunUpdated string
	var currentAttempt int64
	var runTerminal, runDigest sql.NullString
	err = t.QueryRow(`SELECT tag,task_id,owner,claim_acquisition_id,status,current_attempt,updated_at,terminal_at,terminal_result_digest
		FROM runs WHERE run_id=?`, runID).Scan(&storedTag, &storedTask, &storedOwner, &storedClaimID,
		&storedRunStatus, &currentAttempt, &storedRunUpdated, &runTerminal, &runDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, receiptIntegrityError(ReceiptKindRun)
	}
	if err != nil {
		return nil, false, err
	}
	if storedTag != tag || storedTask != taskID || storedOwner != r.owner || storedClaimID != claimID || currentAttempt < attempt {
		return nil, false, receiptIntegrityError(ReceiptKindRun)
	}
	if r.outcome == "succeeded" && (storedRunStatus != "succeeded" || currentAttempt != attempt || storedRunUpdated != committedAt ||
		!runTerminal.Valid || runTerminal.String != committedAt || runDigest.String != r.terminalResultDigest ||
		runDigest.Valid != (r.terminalResultDigest != "")) {
		return nil, false, receiptIntegrityError(ReceiptKindRun)
	}
	if r.outcome == "failed" && currentAttempt == attempt {
		atCommit := storedRunStatus == "retryable" && storedRunUpdated == committedAt && !runTerminal.Valid && !runDigest.Valid
		laterCancel := storedRunStatus == "cancelled" && runTerminal.Valid && runTerminal.String == storedRunUpdated && !runDigest.Valid
		if !atCommit && !laterCancel {
			return nil, false, receiptIntegrityError(ReceiptKindRun)
		}
	}
	var storedAttemptID, storedAttemptStatus, storedAttemptExpires, storedAttemptUpdated, storedAttemptTerminal string
	var storedGeneration int64
	var storedAttemptDigest sql.NullString
	err = t.QueryRow(`SELECT acquisition_id,status,lease_generation,expires_at,updated_at,terminal_at,terminal_result_digest
		FROM attempts WHERE run_id=? AND attempt_no=?`, runID, attempt).Scan(&storedAttemptID, &storedAttemptStatus,
		&storedGeneration, &storedAttemptExpires, &storedAttemptUpdated, &storedAttemptTerminal, &storedAttemptDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, receiptIntegrityError(ReceiptKindRun)
	}
	if err != nil {
		return nil, false, err
	}
	if storedAttemptID != attemptID || storedAttemptStatus != attemptStatus || storedGeneration != generation ||
		storedAttemptExpires != attemptExpires || storedAttemptUpdated != committedAt || storedAttemptTerminal != committedAt ||
		storedAttemptDigest.String != r.terminalResultDigest || storedAttemptDigest.Valid != (r.terminalResultDigest != "") {
		return nil, false, receiptIntegrityError(ReceiptKindRun)
	}
	var taskResult *RunTaskResult
	if r.taskStatus != "" {
		taskResult = &RunTaskResult{ID: taskID, Status: resultTaskStatus, Revision: int(taskRevision)}
	}
	return &RunFinishResult{RunID: runID, RunStatus: runStatus, Attempt: attempt,
		AttemptStatus: attemptStatus, Task: taskResult, TerminalResultDigest: optionalRunDigest(r.terminalResultDigest)}, true, nil
}
