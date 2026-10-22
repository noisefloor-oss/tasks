package store

import (
	"database/sql"
	"errors"
	"time"
)

func validateRetryRunReceipt(t *tx, receipt map[string]any, r runRequest, inputDigest string) error {
	if err := validateRunReceiptRequest(receipt, r, inputDigest); err != nil {
		return err
	}
	tag, tagOK := receipt["tag"].(string)
	taskID, taskOK := receipt["task"].(string)
	resultTaskStatus, statusOK := receipt["resultTaskStatus"].(string)
	if receipt["operation"] != "retry" || receipt["owner"] != r.owner || !tagOK || tag == "" ||
		!taskOK || taskID == "" || !statusOK || receipt["outcome"] != "" || receipt["taskStatus"] != "" ||
		receipt["terminalResultDigest"] != "" || receipt["force"] != false ||
		receipt["runStatus"] != "running" || receipt["attemptStatus"] != "running" {
		return receiptIntegrityError(ReceiptKindRun)
	}
	revision, revisionOK := receiptInt64(receipt["revision"])
	runID, runOK := receiptInt64(receipt["runId"])
	source, sourceOK := receiptInt64(receipt["sourceAttempt"])
	attempt, attemptOK := receiptInt64(receipt["attempt"])
	ttl, ttlOK := receiptInt64(receipt["ttlSeconds"])
	taskRevision, taskRevisionOK := receiptInt64(receipt["taskRevision"])
	generation, generationOK := receiptInt64(receipt["leaseGeneration"])
	attemptID, attemptIDOK := receipt["attemptAcquisitionId"].(string)
	claimID, claimIDOK := receipt["claimAcquisitionId"].(string)
	claimExpires, claimExpiresOK := receipt["claimExpiresAt"].(string)
	attemptExpires, attemptExpiresOK := receipt["attemptExpiresAt"].(string)
	committedAt, committedOK := receipt["committedAt"].(string)
	if !revisionOK || revision != 0 || !runOK || runID != r.runID || !sourceOK || source != r.attempt ||
		!attemptOK || attempt != r.attempt+1 || !ttlOK || ttl != int64(r.ttl) ||
		!taskRevisionOK || taskRevision < 1 || !generationOK || generation != 1 ||
		!attemptIDOK || !validStoredAcquisitionID(attemptID) || !claimIDOK || !validStoredAcquisitionID(claimID) ||
		!claimExpiresOK || !attemptExpiresOK || !committedOK {
		return receiptIntegrityError(ReceiptKindRun)
	}
	committedTime, err1 := time.Parse(time.RFC3339, committedAt)
	attemptExpiry, err2 := time.Parse(time.RFC3339, attemptExpires)
	claimExpiry, err3 := time.Parse(time.RFC3339, claimExpires)
	if err1 != nil || err2 != nil || err3 != nil ||
		!attemptExpiry.Equal(committedTime.Add(time.Duration(ttl)*time.Second)) || !committedTime.Before(claimExpiry) {
		return receiptIntegrityError(ReceiptKindRun)
	}
	resultDigest, ok := receipt["resultDigest"].(string)
	wantResult, err := runResultDigest(runResultValue{
		Attempt: attempt, AttemptID: attemptID, AttemptExpires: attemptExpires, AttemptStatus: "running",
		ClaimID: claimID, ClaimExpires: claimExpires, CommittedAt: committedAt,
		LeaseGeneration: 1, Operation: "retry", Owner: r.owner, RunID: runID,
		RunStatus: "running", Tag: tag, Task: taskID, TaskRevision: int(taskRevision), TaskStatus: resultTaskStatus,
	})
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
	return validateRunReceiptEvent(t, receipt, runID, attempt, "attempt", "retried", committedAt)
}

func lookupRetryRunReplay(t *tx, r runRequest, inputDigest string, now time.Time) (*RunStartResult, bool, error) {
	receipt, err := t.findVerifiedReceipt(ReceiptKindRun, r.requestID)
	if err != nil {
		return nil, false, err
	}
	if receipt == nil {
		return nil, false, nil
	}
	if err := validateRetryRunReceipt(t, receipt, r, inputDigest); err != nil {
		return nil, false, err
	}
	runID, _ := receiptInt64(receipt["runId"])
	attempt, _ := receiptInt64(receipt["attempt"])
	generation, _ := receiptInt64(receipt["leaseGeneration"])
	attemptID, _ := receipt["attemptAcquisitionId"].(string)
	claimID, _ := receipt["claimAcquisitionId"].(string)
	attemptExpires, _ := receipt["attemptExpiresAt"].(string)
	claimExpires, _ := receipt["claimExpiresAt"].(string)
	committedAt, _ := receipt["committedAt"].(string)
	tag, _ := receipt["tag"].(string)
	taskID, _ := receipt["task"].(string)

	state, err := t.currentRunAttempt(runID)
	if err != nil {
		return nil, false, err
	}
	if state == nil {
		return nil, false, receiptIntegrityError(ReceiptKindRun)
	}
	conflict := runCapabilityConflict("retry replay", r.runID, r.attempt)
	if state.Tag != tag || state.Task != taskID || state.Owner != r.owner ||
		state.ClaimAcquisition != claimID || state.ClaimToken <= 0 || state.RunStatus != "running" ||
		state.CurrentAttempt != attempt || state.RunUpdatedAt != committedAt || state.AttemptID != attemptID ||
		state.AttemptStatus != "running" || state.AttemptTTL != int64(r.ttl) || state.LeaseGeneration != generation ||
		state.CreatedRequestID != r.requestID || state.AttemptStartedAt != committedAt ||
		state.AttemptExpiresAt != attemptExpires || state.AttemptUpdatedAt != committedAt {
		return nil, false, conflict
	}
	attemptExpiry, err := time.Parse(time.RFC3339, attemptExpires)
	if err != nil || !now.Before(attemptExpiry) {
		return nil, false, conflict
	}
	var predecessorStatus string
	err = t.QueryRow(`SELECT status FROM attempts WHERE run_id=? AND attempt_no=?`, runID, r.attempt).Scan(&predecessorStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, receiptIntegrityError(ReceiptKindRun)
	}
	if err != nil {
		return nil, false, err
	}
	if predecessorStatus != "failed" && predecessorStatus != "lost" {
		return nil, false, conflict
	}
	task, err := t.getTask(tag, taskID)
	if err != nil {
		return nil, false, err
	}
	if task == nil {
		return nil, false, receiptIntegrityError(ReceiptKindRun)
	}
	expireClaim(task, now)
	if task.Claim == nil || task.Claim.Owner != r.owner || claimAcquisitionID(task.Claim) != claimID ||
		task.Claim.Token != state.ClaimToken || task.Claim.ExpiresAt != claimExpires {
		return nil, false, conflict
	}
	return &RunStartResult{RunID: runID, Attempt: attempt, LeaseGeneration: generation,
		AttemptToken: state.AttemptToken, ExpiresAt: attemptExpires}, true, nil
}
