package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

type runRequest struct {
	operation            string
	requestID            string
	tag                  string
	task                 string
	owner                string
	runID                int64
	attempt              int64
	ttl                  int
	outcome              string
	revision             int
	taskStatus           string
	terminalResultDigest string
	force                bool
}

var runReceiptFields = []string{
	"schemaVersion", "kind", "receiptId", "familyId", "requestId", "inputDigest", "resultDigest",
	"operation", "tag", "task", "runId", "owner", "sourceAttempt", "attempt", "ttlSeconds",
	"outcome", "revision", "taskStatus", "taskRevision", "terminalResultDigest", "force",
	"claimAcquisitionId", "attemptAcquisitionId", "leaseGeneration", "claimExpiresAt",
	"attemptExpiresAt", "runStatus", "attemptStatus", "inventory", "resultTaskStatus",
	"eventSeq", "committedAt", "receiptDigest",
}

func validateRunReceiptRequest(receipt map[string]any, r runRequest, inputDigest string) error {
	if len(receipt) != len(runReceiptFields) {
		return receiptIntegrityError(ReceiptKindRun)
	}
	for _, key := range runReceiptFields {
		if _, ok := receipt[key]; !ok {
			return receiptIntegrityError(ReceiptKindRun)
		}
	}
	storedInput, ok := receipt["inputDigest"].(string)
	if !ok {
		return receiptIntegrityError(ReceiptKindRun)
	}
	if storedInput != inputDigest {
		return &ReplayConflict{Kind: ReceiptKindRun, RequestID: r.requestID}
	}
	if receipt["receiptId"] != runReceiptID(r.requestID, inputDigest) {
		return receiptIntegrityError(ReceiptKindRun)
	}
	return nil
}

func runInputValue(r runRequest) map[string]any {
	return map[string]any{
		"attempt":              r.attempt,
		"force":                r.force,
		"operation":            r.operation,
		"outcome":              r.outcome,
		"owner":                r.owner,
		"requestId":            r.requestID,
		"revision":             int64(r.revision),
		"runId":                r.runID,
		"tag":                  r.tag,
		"task":                 r.task,
		"taskStatus":           r.taskStatus,
		"terminalResultDigest": r.terminalResultDigest,
		"ttlSeconds":           int64(r.ttl),
	}
}

func runInputDigest(r runRequest) (string, error) {
	canonical, err := canonicalJSON(runInputValue(r))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func runReceiptID(requestID, inputDigest string) string {
	sum := sha256.Sum256([]byte("noise-tasks-run-receipt-v1\x00" + requestID + "\x00" + inputDigest))
	return "run-" + hex.EncodeToString(sum[:])
}

type runResultValue struct {
	Attempt         int64
	AttemptID       string
	AttemptExpires  string
	AttemptStatus   string
	ClaimID         string
	ClaimExpires    string
	CommittedAt     string
	LeaseGeneration int64
	Operation       string
	Owner           string
	RunID           int64
	RunStatus       string
	Tag             string
	Task            string
	TaskRevision    int
	TaskStatus      string
	TerminalDigest  string
}

func (v runResultValue) value() map[string]any {
	return map[string]any{
		"attempt":              v.Attempt,
		"attemptAcquisitionId": v.AttemptID,
		"attemptExpiresAt":     v.AttemptExpires,
		"attemptStatus":        v.AttemptStatus,
		"claimAcquisitionId":   v.ClaimID,
		"claimExpiresAt":       v.ClaimExpires,
		"committedAt":          v.CommittedAt,
		"leaseGeneration":      v.LeaseGeneration,
		"operation":            v.Operation,
		"owner":                v.Owner,
		"runId":                v.RunID,
		"runStatus":            v.RunStatus,
		"tag":                  v.Tag,
		"task":                 v.Task,
		"taskRevision":         int64(v.TaskRevision),
		"taskStatus":           v.TaskStatus,
		"terminalResultDigest": v.TerminalDigest,
	}
}

func runResultDigest(v runResultValue) (string, error) {
	canonical, err := canonicalJSON(v.value())
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func writeStartRunReceipt(t *tx, r runRequest, inputDigest string, task *Task, claim *Claim, result *RunStartResult, attemptID, committedAt string) error {
	return writeRunningRunReceipt(t, r, inputDigest, task, claim, result, attemptID,
		ReceiptKindRun, "started", committedAt)
}

func writeRunningRunReceipt(t *tx, r runRequest, inputDigest string, task *Task, claim *Claim,
	result *RunStartResult, attemptID, eventKind, eventAction, committedAt string) error {
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
		Attempt: result.Attempt, AttemptID: attemptID, AttemptExpires: result.ExpiresAt, AttemptStatus: "running",
		ClaimID: claimAcquisitionID(claim), ClaimExpires: claim.ExpiresAt,
		CommittedAt: committedAt, LeaseGeneration: result.LeaseGeneration, Operation: r.operation, Owner: r.owner,
		RunID: result.RunID, RunStatus: "running", Tag: task.Tag, Task: task.ID,
		TaskRevision: task.Revision, TaskStatus: task.Status,
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
		"operation": r.operation, "tag": task.Tag, "task": task.ID, "runId": result.RunID,
		"owner": r.owner, "sourceAttempt": r.attempt, "attempt": result.Attempt,
		"ttlSeconds": int64(r.ttl), "outcome": "", "revision": int64(0),
		"taskStatus": "", "taskRevision": int64(task.Revision),
		"terminalResultDigest": "", "force": false,
		"claimAcquisitionId": claimAcquisitionID(claim), "attemptAcquisitionId": attemptID,
		"leaseGeneration": result.LeaseGeneration, "claimExpiresAt": claim.ExpiresAt,
		"attemptExpiresAt": result.ExpiresAt, "runStatus": "running", "attemptStatus": "running",
		"inventory": inventoryRecord(inv), "resultTaskStatus": task.Status,
		"eventSeq": map[string]any{"first": seq, "last": seq}, "committedAt": committedAt,
	}
	digest, err := receiptDigest(receipt)
	if err != nil {
		return err
	}
	if err := t.appendEvent(Event{
		Task: task.ID, Tag: task.Tag, Kind: eventKind, Action: eventAction, Owner: r.owner,
		At: committedAt, Extra: runEventExtra(result.RunID, result.Attempt, receiptID, digest),
	}); err != nil {
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

func lookupStartRunReplay(t *tx, r runRequest, inputDigest string, now time.Time) (*RunStartResult, bool, error) {
	receipt, err := t.findVerifiedReceipt(ReceiptKindRun, r.requestID)
	if err != nil {
		return nil, false, err
	}
	if receipt == nil {
		return nil, false, nil
	}
	if err := validateStartRunReceipt(t, receipt, r, inputDigest); err != nil {
		return nil, false, err
	}
	runID, _ := receiptInt64(receipt["runId"])
	attemptNo, _ := receiptInt64(receipt["attempt"])
	leaseGeneration, _ := receiptInt64(receipt["leaseGeneration"])
	attemptID, _ := receipt["attemptAcquisitionId"].(string)
	claimID, _ := receipt["claimAcquisitionId"].(string)
	attemptExpires, _ := receipt["attemptExpiresAt"].(string)
	claimExpires, _ := receipt["claimExpiresAt"].(string)
	committedAt, _ := receipt["committedAt"].(string)

	var tag, taskID, owner, storedClaimID, runStatus, startRequestID, runStartedAt, runUpdatedAt string
	var storedAttemptID, attemptStatus, createdRequestID, attemptStartedAt, storedAttemptExpires, attemptUpdatedAt string
	var storedClaimToken, currentAttempt, storedTTL, storedGeneration, attemptToken int64
	err = t.QueryRow(`SELECT r.tag,r.task_id,r.owner,r.claim_acquisition_id,r.claim_token,
		r.status,r.current_attempt,r.start_request_id,r.started_at,r.updated_at,
		a.acquisition_id,a.status,a.ttl_seconds,a.lease_generation,a.attempt_token,
		a.created_request_id,a.started_at,a.expires_at,a.updated_at
		FROM runs r JOIN attempts a ON a.run_id=r.run_id AND a.attempt_no=r.current_attempt
		WHERE r.run_id=?`, runID).Scan(&tag, &taskID, &owner, &storedClaimID, &storedClaimToken,
		&runStatus, &currentAttempt, &startRequestID, &runStartedAt, &runUpdatedAt,
		&storedAttemptID, &attemptStatus, &storedTTL, &storedGeneration, &attemptToken,
		&createdRequestID, &attemptStartedAt, &storedAttemptExpires, &attemptUpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, receiptIntegrityError(ReceiptKindRun)
	}
	if err != nil {
		return nil, false, err
	}
	conflict := &RunConflict{Operation: "start replay", Tag: r.tag, Task: r.task, RunID: runID, Attempt: attemptNo}
	if tag != r.tag || taskID != r.task || owner != r.owner || storedClaimID != claimID ||
		storedClaimToken <= 0 || runStatus != "running" || startRequestID != r.requestID ||
		runStartedAt != committedAt || runUpdatedAt != committedAt ||
		currentAttempt != attemptNo || attemptStatus != "running" || storedAttemptID != attemptID ||
		storedTTL != int64(r.ttl) || storedGeneration != leaseGeneration || createdRequestID != r.requestID ||
		attemptStartedAt != committedAt || storedAttemptExpires != attemptExpires || attemptUpdatedAt != committedAt {
		return nil, false, conflict
	}
	attemptExpiry, err := time.Parse(time.RFC3339, attemptExpires)
	if err != nil || !now.Before(attemptExpiry) {
		return nil, false, conflict
	}
	task, err := t.getTask(r.tag, r.task)
	if err != nil {
		return nil, false, err
	}
	if task == nil {
		return nil, false, receiptIntegrityError(ReceiptKindRun)
	}
	expireClaim(task, now)
	if task.Claim == nil || task.Claim.Owner != r.owner || claimAcquisitionID(task.Claim) != claimID ||
		task.Claim.Token != storedClaimToken || task.Claim.ExpiresAt != claimExpires {
		return nil, false, conflict
	}
	return &RunStartResult{RunID: runID, Attempt: attemptNo, LeaseGeneration: leaseGeneration,
		AttemptToken: attemptToken, ExpiresAt: attemptExpires}, true, nil
}

func validateStartRunReceipt(t *tx, receipt map[string]any, r runRequest, inputDigest string) error {
	if err := validateRunReceiptRequest(receipt, r, inputDigest); err != nil {
		return err
	}
	if receipt["operation"] != "start" ||
		receipt["tag"] != r.tag || receipt["task"] != r.task || receipt["owner"] != r.owner ||
		receipt["outcome"] != "" || receipt["taskStatus"] != "" || receipt["terminalResultDigest"] != "" ||
		receipt["force"] != false || receipt["runStatus"] != "running" || receipt["attemptStatus"] != "running" {
		return receiptIntegrityError(ReceiptKindRun)
	}
	zeroFields := []string{"sourceAttempt", "revision"}
	for _, key := range zeroFields {
		if n, ok := receiptInt64(receipt[key]); !ok || n != 0 {
			return receiptIntegrityError(ReceiptKindRun)
		}
	}
	runID, runOK := receiptInt64(receipt["runId"])
	attempt, attemptOK := receiptInt64(receipt["attempt"])
	ttl, ttlOK := receiptInt64(receipt["ttlSeconds"])
	taskRevision, revisionOK := receiptInt64(receipt["taskRevision"])
	generation, generationOK := receiptInt64(receipt["leaseGeneration"])
	attemptID, attemptIDOK := receipt["attemptAcquisitionId"].(string)
	claimID, claimIDOK := receipt["claimAcquisitionId"].(string)
	claimExpires, claimExpiresOK := receipt["claimExpiresAt"].(string)
	attemptExpires, attemptExpiresOK := receipt["attemptExpiresAt"].(string)
	committedAt, committedOK := receipt["committedAt"].(string)
	resultTaskStatus, statusOK := receipt["resultTaskStatus"].(string)
	if !runOK || runID <= 0 || !attemptOK || attempt != 1 || !ttlOK || ttl != int64(r.ttl) ||
		!revisionOK || taskRevision < 1 || !generationOK || generation != 1 ||
		!attemptIDOK || !validStoredAcquisitionID(attemptID) || !claimIDOK || !validStoredAcquisitionID(claimID) ||
		!claimExpiresOK || !attemptExpiresOK || !committedOK || !statusOK {
		return receiptIntegrityError(ReceiptKindRun)
	}
	committedTime, err1 := time.Parse(time.RFC3339, committedAt)
	attemptExpiry, err2 := time.Parse(time.RFC3339, attemptExpires)
	if err1 != nil || err2 != nil || !attemptExpiry.Equal(committedTime.Add(time.Duration(ttl)*time.Second)) {
		return receiptIntegrityError(ReceiptKindRun)
	}
	claimExpiry, err := time.Parse(time.RFC3339, claimExpires)
	if err != nil || !committedTime.Before(claimExpiry) {
		return receiptIntegrityError(ReceiptKindRun)
	}
	resultDigest, ok := receipt["resultDigest"].(string)
	wantResult, err := runResultDigest(runResultValue{
		Attempt: 1, AttemptID: attemptID, AttemptExpires: attemptExpires, AttemptStatus: "running",
		ClaimID: claimID, ClaimExpires: claimExpires, CommittedAt: committedAt,
		LeaseGeneration: 1, Operation: "start", Owner: r.owner, RunID: runID,
		RunStatus: "running", Tag: r.tag, Task: r.task, TaskRevision: int(taskRevision), TaskStatus: resultTaskStatus,
	})
	if !ok || err != nil || resultDigest != wantResult {
		return receiptIntegrityError(ReceiptKindRun)
	}
	invObj, ok := receipt["inventory"].(map[string]any)
	if !ok {
		return receiptIntegrityError(ReceiptKindRun)
	}
	inv, err := parseInventory(invObj)
	if err != nil || inv.Runs < 1 || inv.Attempts < 1 || inv.Receipts < 1 || inv.Events < 1 {
		return receiptIntegrityError(ReceiptKindRun)
	}
	return validateRunReceiptEvent(t, receipt, runID, 1, ReceiptKindRun, "started", committedAt)
}

func validateRunReceiptEvent(t *tx, receipt map[string]any, runID, attempt int64, expectedKind, action, committedAt string) error {
	seqObj, ok := receipt["eventSeq"].(map[string]any)
	if !ok || len(seqObj) != 2 {
		return receiptIntegrityError(ReceiptKindRun)
	}
	first, firstOK := receiptInt64(seqObj["first"])
	last, lastOK := receiptInt64(seqObj["last"])
	if !firstOK || !lastOK || first <= 0 || first != last {
		return receiptIntegrityError(ReceiptKindRun)
	}
	var oldest sql.NullInt64
	if err := t.QueryRow(`SELECT MIN(seq) FROM events`).Scan(&oldest); err != nil {
		return err
	}
	var tag, taskID, storedKind, storedAction, owner, at string
	var extra sql.NullString
	err := t.QueryRow(`SELECT COALESCE(tag,''),COALESCE(task_id,''),kind,COALESCE(action,''),COALESCE(owner,''),created_at,extra
		FROM events WHERE seq=?`, first).Scan(&tag, &taskID, &storedKind, &storedAction, &owner, &at, &extra)
	if errors.Is(err, sql.ErrNoRows) {
		if oldest.Valid && first < oldest.Int64 {
			return nil
		}
		return receiptIntegrityError(ReceiptKindRun)
	}
	if err != nil {
		return err
	}
	if tag != receipt["tag"] || taskID != receipt["task"] || storedKind != expectedKind ||
		storedAction != action || owner != receipt["owner"] || at != committedAt || !extra.Valid {
		return receiptIntegrityError(ReceiptKindRun)
	}
	extraObj, err := decodeJSONObject([]byte(extra.String))
	if err != nil || len(extraObj) != 4 {
		return receiptIntegrityError(ReceiptKindRun)
	}
	eventRun, runOK := receiptInt64(extraObj["runId"])
	eventAttempt, attemptOK := receiptInt64(extraObj["attempt"])
	if !runOK || !attemptOK || eventRun != runID || eventAttempt != attempt ||
		extraObj["receiptId"] != receipt["receiptId"] || extraObj["receiptDigest"] != receipt["receiptDigest"] {
		return receiptIntegrityError(ReceiptKindRun)
	}
	return nil
}
