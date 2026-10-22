package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func writeCancelRunReceipt(t *tx, r runRequest, inputDigest string, task *Task, claim *Claim,
	state *currentRunAttempt, result *RunCancelResult, committedAt string) error {
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
	claimID, claimExpires, attemptID, attemptExpires := "", "", "", ""
	var generation int64
	if !r.force {
		claimID, claimExpires = claimAcquisitionID(claim), claim.ExpiresAt
		attemptID, attemptExpires, generation = state.AttemptID, state.AttemptExpiresAt, state.LeaseGeneration
	}
	resultValue := runResultValue{Attempt: state.CurrentAttempt, AttemptID: attemptID,
		AttemptExpires: attemptExpires, AttemptStatus: result.AttemptStatus, ClaimID: claimID,
		ClaimExpires: claimExpires, CommittedAt: committedAt, LeaseGeneration: generation,
		Operation: "cancel", Owner: r.owner, RunID: state.RunID, RunStatus: "cancelled",
		Tag: task.Tag, Task: task.ID, TaskRevision: task.Revision, TaskStatus: task.Status}
	resultDigest, err := runResultDigest(resultValue)
	if err != nil {
		return err
	}
	var seq int64
	if err := t.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='events'`).Scan(&seq); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read run event sequence: %w", err)
	}
	seq++
	sourceAttempt := r.attempt
	if r.force {
		sourceAttempt = 0
	}
	receipt := map[string]any{
		"schemaVersion": int64(ReceiptSchemaVersion), "kind": ReceiptKindRun,
		"receiptId": receiptID, "familyId": familyID, "requestId": r.requestID,
		"inputDigest": inputDigest, "resultDigest": resultDigest,
		"operation": "cancel", "tag": task.Tag, "task": task.ID, "runId": state.RunID,
		"owner": r.owner, "sourceAttempt": sourceAttempt, "attempt": state.CurrentAttempt,
		"ttlSeconds": int64(0), "outcome": "", "revision": int64(0), "taskStatus": "",
		"taskRevision": int64(task.Revision), "terminalResultDigest": "", "force": r.force,
		"claimAcquisitionId": claimID, "attemptAcquisitionId": attemptID,
		"leaseGeneration": generation, "claimExpiresAt": claimExpires,
		"attemptExpiresAt": attemptExpires, "runStatus": "cancelled",
		"attemptStatus": result.AttemptStatus, "inventory": inventoryRecord(inv),
		"resultTaskStatus": task.Status, "eventSeq": map[string]any{"first": seq, "last": seq},
		"committedAt": committedAt,
	}
	digest, err := receiptDigest(receipt)
	if err != nil {
		return err
	}
	if err := t.appendEvent(Event{Task: task.ID, Tag: task.Tag, Kind: ReceiptKindRun, Action: "cancelled",
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

func validateCancelRunReceipt(t *tx, receipt map[string]any, r runRequest, inputDigest string) error {
	if err := validateRunReceiptRequest(receipt, r, inputDigest); err != nil {
		return err
	}
	tag, tagOK := receipt["tag"].(string)
	taskID, taskOK := receipt["task"].(string)
	resultTaskStatus, statusOK := receipt["resultTaskStatus"].(string)
	attemptStatus, attemptStatusOK := receipt["attemptStatus"].(string)
	if receipt["operation"] != "cancel" || receipt["owner"] != r.owner || !tagOK || tag == "" ||
		!taskOK || taskID == "" || !statusOK || !validStatuses[resultTaskStatus] || !attemptStatusOK ||
		(attemptStatus != "cancelled" && attemptStatus != "failed" && attemptStatus != "lost") ||
		receipt["outcome"] != "" || receipt["taskStatus"] != "" || receipt["terminalResultDigest"] != "" ||
		receipt["force"] != r.force || receipt["runStatus"] != "cancelled" {
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
	if !runOK || runID != r.runID || !attemptOK || attempt <= 0 || !ttlOK || ttl != 0 ||
		!revisionOK || revision != 0 || !taskRevisionOK || taskRevision < 1 || !sourceOK ||
		!generationOK || !attemptIDOK || !claimIDOK || !claimExpiresOK || !attemptExpiresOK || !committedOK {
		return receiptIntegrityError(ReceiptKindRun)
	}
	committedTime, err := time.Parse(time.RFC3339, committedAt)
	if err != nil {
		return receiptIntegrityError(ReceiptKindRun)
	}
	if r.force {
		if source != 0 || r.attempt != 0 || r.owner != "" || generation != 0 || attemptID != "" ||
			claimID != "" || claimExpires != "" || attemptExpires != "" {
			return receiptIntegrityError(ReceiptKindRun)
		}
	} else {
		if source != r.attempt || attempt != r.attempt || generation < 1 ||
			!validStoredAcquisitionID(attemptID) || !validStoredAcquisitionID(claimID) {
			return receiptIntegrityError(ReceiptKindRun)
		}
		claimExpiry, err1 := time.Parse(time.RFC3339, claimExpires)
		attemptExpiry, err2 := time.Parse(time.RFC3339, attemptExpires)
		if err1 != nil || err2 != nil || !committedTime.Before(claimExpiry) ||
			(attemptStatus == "cancelled" && !committedTime.Before(attemptExpiry)) {
			return receiptIntegrityError(ReceiptKindRun)
		}
	}
	resultDigest, ok := receipt["resultDigest"].(string)
	wantResult, err := runResultDigest(runResultValue{Attempt: attempt, AttemptID: attemptID,
		AttemptExpires: attemptExpires, AttemptStatus: attemptStatus, ClaimID: claimID, ClaimExpires: claimExpires,
		CommittedAt: committedAt, LeaseGeneration: generation, Operation: "cancel", Owner: r.owner,
		RunID: runID, RunStatus: "cancelled", Tag: tag, Task: taskID,
		TaskRevision: int(taskRevision), TaskStatus: resultTaskStatus})
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
	return validateRunReceiptEvent(t, receipt, runID, attempt, ReceiptKindRun, "cancelled", committedAt)
}

func verifyFailedAttemptFinishReceipt(t *tx, runID, attempt, generation int64, attemptID,
	attemptExpires, updatedAt string, digest sql.NullString) error {
	rows, err := t.Query(`SELECT request_id FROM receipts WHERE kind=? AND request_id IS NOT NULL`, ReceiptKindRun)
	if err != nil {
		return err
	}
	var requestIDs []string
	for rows.Next() {
		var requestID string
		if err := rows.Scan(&requestID); err != nil {
			rows.Close()
			return err
		}
		requestIDs = append(requestIDs, requestID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	found := 0
	for _, requestID := range requestIDs {
		receipt, err := t.findVerifiedReceipt(ReceiptKindRun, requestID)
		if err != nil {
			return err
		}
		if receipt == nil || receipt["operation"] != "finish" || receipt["outcome"] != "failed" {
			continue
		}
		receiptRun, runOK := receiptInt64(receipt["runId"])
		receiptAttempt, attemptOK := receiptInt64(receipt["attempt"])
		if !runOK || !attemptOK {
			return receiptIntegrityError(ReceiptKindRun)
		}
		if receiptRun != runID || receiptAttempt != attempt {
			continue
		}
		owner, ownerOK := receipt["owner"].(string)
		outcome, outcomeOK := receipt["outcome"].(string)
		taskStatus, statusOK := receipt["taskStatus"].(string)
		terminalDigest, digestOK := receipt["terminalResultDigest"].(string)
		sourceAttempt, sourceOK := receiptInt64(receipt["sourceAttempt"])
		revision, revisionOK := receiptInt64(receipt["revision"])
		if !ownerOK || !outcomeOK || !statusOK || !digestOK || !sourceOK || !revisionOK ||
			sourceAttempt != attempt || revision < 0 || int64(int(revision)) != revision ||
			ValidateOwnerLabel(owner) != nil || !ValidLocalKey(requestID) ||
			(terminalDigest != "" && !ValidDigest(terminalDigest)) {
			return receiptIntegrityError(ReceiptKindRun)
		}
		finishRequest := runRequest{operation: "finish", requestID: requestID, owner: owner,
			runID: runID, attempt: attempt, outcome: outcome, revision: int(revision),
			taskStatus: taskStatus, terminalResultDigest: terminalDigest}
		inputDigest, err := runInputDigest(finishRequest)
		if err != nil || validateFinishRunReceipt(t, receipt, finishRequest, inputDigest) != nil {
			return receiptIntegrityError(ReceiptKindRun)
		}
		receiptGeneration, generationOK := receiptInt64(receipt["leaseGeneration"])
		receiptAttemptID, idOK := receipt["attemptAcquisitionId"].(string)
		receiptExpires, expiresOK := receipt["attemptExpiresAt"].(string)
		receiptCommitted, committedOK := receipt["committedAt"].(string)
		if !generationOK || !idOK || !expiresOK || !committedOK || receiptGeneration != generation ||
			receiptAttemptID != attemptID || receiptExpires != attemptExpires || receiptCommitted != updatedAt ||
			terminalDigest != digest.String || digest.Valid != (terminalDigest != "") {
			return receiptIntegrityError(ReceiptKindRun)
		}
		found++
	}
	if found != 1 {
		return receiptIntegrityError(ReceiptKindRun)
	}
	return nil
}

func lookupCancelRunReplay(t *tx, r runRequest, inputDigest string) (*RunCancelResult, bool, error) {
	receipt, err := t.findVerifiedReceipt(ReceiptKindRun, r.requestID)
	if err != nil {
		return nil, false, err
	}
	if receipt == nil {
		return nil, false, nil
	}
	if err := validateCancelRunReceipt(t, receipt, r, inputDigest); err != nil {
		return nil, false, err
	}
	runID, _ := receiptInt64(receipt["runId"])
	attempt, _ := receiptInt64(receipt["attempt"])
	generation, _ := receiptInt64(receipt["leaseGeneration"])
	tag, _ := receipt["tag"].(string)
	taskID, _ := receipt["task"].(string)
	attemptID, _ := receipt["attemptAcquisitionId"].(string)
	claimID, _ := receipt["claimAcquisitionId"].(string)
	attemptExpires, _ := receipt["attemptExpiresAt"].(string)
	committedAt, _ := receipt["committedAt"].(string)
	attemptStatus, _ := receipt["attemptStatus"].(string)

	var storedTag, storedTask, storedOwner, storedClaimID, storedStatus, storedUpdated, storedTerminal string
	var currentAttempt int64
	var runDigest sql.NullString
	var forceFence sql.NullInt64
	err = t.QueryRow(`SELECT tag,task_id,owner,claim_acquisition_id,status,current_attempt,updated_at,
		terminal_at,terminal_result_digest,force_cancel_fence FROM runs WHERE run_id=?`, runID).
		Scan(&storedTag, &storedTask, &storedOwner, &storedClaimID, &storedStatus, &currentAttempt,
			&storedUpdated, &storedTerminal, &runDigest, &forceFence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, receiptIntegrityError(ReceiptKindRun)
	}
	if err != nil {
		return nil, false, err
	}
	if storedTag != tag || storedTask != taskID || storedStatus != "cancelled" || currentAttempt != attempt ||
		storedUpdated != committedAt || storedTerminal != committedAt || runDigest.Valid ||
		(r.force && (!forceFence.Valid || forceFence.Int64 <= 0)) || (!r.force && forceFence.Valid) ||
		(!r.force && (storedOwner != r.owner || storedClaimID != claimID)) {
		return nil, false, receiptIntegrityError(ReceiptKindRun)
	}
	var storedAttemptID, storedAttemptStatus, storedAttemptExpires, storedAttemptUpdated string
	var storedAttemptTerminal sql.NullString
	var storedAttemptDigest sql.NullString
	var storedGeneration int64
	err = t.QueryRow(`SELECT acquisition_id,status,lease_generation,expires_at,updated_at,terminal_at,terminal_result_digest
		FROM attempts WHERE run_id=? AND attempt_no=?`, runID, attempt).Scan(&storedAttemptID, &storedAttemptStatus,
		&storedGeneration, &storedAttemptExpires, &storedAttemptUpdated, &storedAttemptTerminal, &storedAttemptDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, receiptIntegrityError(ReceiptKindRun)
	}
	if err != nil {
		return nil, false, err
	}
	if storedAttemptStatus != attemptStatus {
		return nil, false, receiptIntegrityError(ReceiptKindRun)
	}
	if !validStoredAcquisitionID(storedAttemptID) || storedGeneration < 1 {
		return nil, false, receiptIntegrityError(ReceiptKindRun)
	}
	if _, err := time.Parse(time.RFC3339, storedAttemptExpires); err != nil {
		return nil, false, receiptIntegrityError(ReceiptKindRun)
	}
	if attemptStatus == "cancelled" && (storedAttemptUpdated != committedAt || !storedAttemptTerminal.Valid ||
		storedAttemptTerminal.String != committedAt || storedAttemptDigest.Valid) {
		return nil, false, receiptIntegrityError(ReceiptKindRun)
	}
	if attemptStatus == "failed" || attemptStatus == "lost" {
		updatedTime, err1 := time.Parse(time.RFC3339, storedAttemptUpdated)
		terminalTime, err2 := time.Parse(time.RFC3339, storedAttemptTerminal.String)
		cancelTime, err3 := time.Parse(time.RFC3339, committedAt)
		if !storedAttemptTerminal.Valid || err1 != nil || err2 != nil || err3 != nil ||
			!updatedTime.Equal(terminalTime) || terminalTime.After(cancelTime) ||
			(attemptStatus == "lost" && storedAttemptDigest.Valid) ||
			(attemptStatus == "failed" && storedAttemptDigest.Valid && !ValidDigest(storedAttemptDigest.String)) {
			return nil, false, receiptIntegrityError(ReceiptKindRun)
		}
		if attemptStatus == "failed" {
			if err := verifyFailedAttemptFinishReceipt(t, runID, attempt, storedGeneration, storedAttemptID,
				storedAttemptExpires, storedAttemptUpdated, storedAttemptDigest); err != nil {
				return nil, false, err
			}
		}
	}
	if !r.force && (storedAttemptID != attemptID || storedGeneration != generation || storedAttemptExpires != attemptExpires) {
		return nil, false, receiptIntegrityError(ReceiptKindRun)
	}
	return &RunCancelResult{RunID: runID, RunStatus: "cancelled", Attempt: attempt,
		AttemptStatus: attemptStatus, Forced: r.force}, true, nil
}
