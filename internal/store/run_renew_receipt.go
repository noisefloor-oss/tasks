package store

import "time"

func validateRenewRunReceipt(t *tx, receipt map[string]any, r runRequest, inputDigest string) error {
	if err := validateRunReceiptRequest(receipt, r, inputDigest); err != nil {
		return err
	}
	tag, tagOK := receipt["tag"].(string)
	taskID, taskOK := receipt["task"].(string)
	resultTaskStatus, statusOK := receipt["resultTaskStatus"].(string)
	if receipt["operation"] != "renew" || receipt["owner"] != r.owner || !tagOK || tag == "" ||
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
		!attemptOK || attempt != r.attempt || !ttlOK || ttl != int64(r.ttl) ||
		!taskRevisionOK || taskRevision < 1 || !generationOK || generation < 2 ||
		!attemptIDOK || !validStoredAcquisitionID(attemptID) || !claimIDOK || !validStoredAcquisitionID(claimID) ||
		!claimExpiresOK || !attemptExpiresOK || claimExpires != attemptExpires || !committedOK {
		return receiptIntegrityError(ReceiptKindRun)
	}
	committedTime, err1 := time.Parse(time.RFC3339, committedAt)
	expiry, err2 := time.Parse(time.RFC3339, attemptExpires)
	if err1 != nil || err2 != nil || !expiry.Equal(committedTime.Add(time.Duration(ttl)*time.Second)) {
		return receiptIntegrityError(ReceiptKindRun)
	}
	resultDigest, ok := receipt["resultDigest"].(string)
	wantResult, err := runResultDigest(runResultValue{
		Attempt: attempt, AttemptID: attemptID, AttemptExpires: attemptExpires, AttemptStatus: "running",
		ClaimID: claimID, ClaimExpires: claimExpires, CommittedAt: committedAt,
		LeaseGeneration: generation, Operation: "renew", Owner: r.owner, RunID: runID,
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
	return validateRunReceiptEvent(t, receipt, runID, attempt, "attempt", "renewed", committedAt)
}

func lookupRenewRunReplay(t *tx, r runRequest, inputDigest string, now time.Time) (*RunRenewResult, bool, error) {
	receipt, err := t.findVerifiedReceipt(ReceiptKindRun, r.requestID)
	if err != nil {
		return nil, false, err
	}
	if receipt == nil {
		return nil, false, nil
	}
	if err := validateRenewRunReceipt(t, receipt, r, inputDigest); err != nil {
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
	conflict := runCapabilityConflict("renew replay", r.runID, r.attempt)
	if state.Tag != tag || state.Task != taskID || state.Owner != r.owner ||
		state.ClaimAcquisition != claimID || state.ClaimToken <= 0 || state.RunStatus != "running" ||
		state.CurrentAttempt != attempt || state.RunUpdatedAt != committedAt || state.AttemptID != attemptID ||
		state.AttemptStatus != "running" || state.AttemptTTL != int64(r.ttl) || state.LeaseGeneration != generation ||
		state.AttemptExpiresAt != attemptExpires || state.AttemptUpdatedAt != committedAt {
		return nil, false, conflict
	}
	expiry, err := time.Parse(time.RFC3339, attemptExpires)
	if err != nil || !now.Before(expiry) {
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
	return &RunRenewResult{RunID: runID, Attempt: attempt, LeaseGeneration: generation,
		ClaimToken: state.ClaimToken, ClaimExpiresAt: claimExpires,
		AttemptToken: state.AttemptToken, AttemptExpiresAt: attemptExpires}, true, nil
}
