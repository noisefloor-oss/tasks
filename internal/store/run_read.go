package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/noisefloor-oss/tasks/internal/backlog"
)

type RunAttempt struct {
	Attempt              int64   `json:"attempt"`
	AcquisitionID        string  `json:"acquisitionId"`
	Status               string  `json:"status"`
	TTLSeconds           int64   `json:"ttlSeconds"`
	LeaseGeneration      int64   `json:"leaseGeneration"`
	StartedAt            string  `json:"startedAt"`
	ExpiresAt            string  `json:"expiresAt"`
	UpdatedAt            string  `json:"updatedAt"`
	TerminalAt           *string `json:"terminalAt"`
	TerminalResultDigest *string `json:"terminalResultDigest"`
}

type RunReceiptRef struct {
	Operation     string `json:"operation"`
	RequestID     string `json:"requestId"`
	ReceiptID     string `json:"receiptId"`
	ReceiptDigest string `json:"receiptDigest"`
	CommittedAt   string `json:"committedAt"`
}

type RunRecord struct {
	RunID                int64           `json:"runId"`
	Tag                  string          `json:"tag"`
	Task                 string          `json:"task"`
	Owner                string          `json:"owner"`
	Status               string          `json:"status"`
	CurrentAttempt       int64           `json:"currentAttempt"`
	StartedAt            string          `json:"startedAt"`
	UpdatedAt            string          `json:"updatedAt"`
	TerminalAt           *string         `json:"terminalAt"`
	TerminalResultDigest *string         `json:"terminalResultDigest"`
	Attempts             []RunAttempt    `json:"attempts"`
	Receipts             []RunReceiptRef `json:"receipts"`
}

type RunSummary struct {
	RunID          int64  `json:"runId"`
	Tag            string `json:"tag"`
	Task           string `json:"task"`
	Owner          string `json:"owner"`
	Status         string `json:"status"`
	CurrentAttempt int64  `json:"currentAttempt"`
	UpdatedAt      string `json:"updatedAt"`
}

type sequencedRunReceipt struct {
	seq int64
	ref RunReceiptRef
}

type runAttemptLineage struct {
	acquisitionID   string
	createdRequest  string
	leaseGeneration int64
	expiresAt       string
}

type runReadLineage struct {
	runID            int64
	tag              string
	task             string
	owner            string
	claimAcquisition string
	startRequest     string
	attempts         map[int64]runAttemptLineage
}

func nullableStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	result := value.String
	return &result
}

func validateStoredRunReceipt(t *tx, receipt map[string]any) error {
	requestID, requestOK := receipt["requestId"].(string)
	operation, operationOK := receipt["operation"].(string)
	tag, tagOK := receipt["tag"].(string)
	task, taskOK := receipt["task"].(string)
	owner, ownerOK := receipt["owner"].(string)
	outcome, outcomeOK := receipt["outcome"].(string)
	taskStatus, taskStatusOK := receipt["taskStatus"].(string)
	terminalDigest, digestOK := receipt["terminalResultDigest"].(string)
	force, forceOK := receipt["force"].(bool)
	runID, runOK := receiptInt64(receipt["runId"])
	sourceAttempt, attemptOK := receiptInt64(receipt["sourceAttempt"])
	ttl, ttlOK := receiptInt64(receipt["ttlSeconds"])
	revision, revisionOK := receiptInt64(receipt["revision"])
	if !requestOK || !operationOK || !tagOK || !taskOK || !ownerOK || !outcomeOK || !taskStatusOK ||
		!digestOK || !forceOK || !runOK || !attemptOK || !ttlOK || !revisionOK ||
		ttl < 0 || ttl > MaxRunTTL || revision < 0 || int64(int(ttl)) != ttl || int64(int(revision)) != revision {
		return receiptIntegrityError(ReceiptKindRun)
	}
	r := runRequest{operation: operation, requestID: requestID, owner: owner,
		runID: runID, attempt: sourceAttempt, ttl: int(ttl), outcome: outcome,
		revision: int(revision), taskStatus: taskStatus, terminalResultDigest: terminalDigest, force: force}
	if operation == "start" {
		r.tag, r.task, r.runID = tag, task, 0
	}
	if err := validateStoredRunInput(r, tag, task, runID); err != nil {
		return receiptIntegrityError(ReceiptKindRun)
	}
	inputDigest, err := runInputDigest(r)
	if err != nil {
		return receiptIntegrityError(ReceiptKindRun)
	}
	switch operation {
	case "start":
		err = validateStartRunReceipt(t, receipt, r, inputDigest)
	case "retry":
		err = validateRetryRunReceipt(t, receipt, r, inputDigest)
	case "renew":
		err = validateRenewRunReceipt(t, receipt, r, inputDigest)
	case "finish":
		err = validateFinishRunReceipt(t, receipt, r, inputDigest)
	case "cancel":
		err = validateCancelRunReceipt(t, receipt, r, inputDigest)
	default:
		return receiptIntegrityError(ReceiptKindRun)
	}
	if err != nil {
		var replay *ReplayConflict
		if errors.As(err, &replay) {
			return receiptIntegrityError(ReceiptKindRun)
		}
		return err
	}
	return nil
}

func validateStoredRunInput(r runRequest, resultTag, resultTask string, resultRunID int64) error {
	if !ValidLocalKey(r.requestID) || resultRunID <= 0 || resultTag == "" || resultTask == "" {
		return receiptIntegrityError(ReceiptKindRun)
	}
	if r.terminalResultDigest != "" && !ValidDigest(r.terminalResultDigest) {
		return receiptIntegrityError(ReceiptKindRun)
	}
	zeroFinishFields := r.outcome == "" && r.revision == 0 && r.taskStatus == "" && r.terminalResultDigest == ""
	switch r.operation {
	case "start":
		if ValidateOwnerLabel(r.owner) != nil || r.runID != 0 || r.attempt != 0 || r.ttl < 1 ||
			r.ttl > MaxRunTTL || !zeroFinishFields || r.force || r.tag != resultTag || r.task != resultTask {
			return receiptIntegrityError(ReceiptKindRun)
		}
	case "retry", "renew":
		if ValidateOwnerLabel(r.owner) != nil || r.runID <= 0 || r.attempt <= 0 || r.ttl < 1 ||
			r.ttl > MaxRunTTL || !zeroFinishFields || r.force || r.tag != "" || r.task != "" {
			return receiptIntegrityError(ReceiptKindRun)
		}
	case "finish":
		paired := (r.taskStatus == "") == (r.revision == 0)
		if ValidateOwnerLabel(r.owner) != nil || r.runID <= 0 || r.attempt <= 0 || r.ttl != 0 || r.force ||
			(r.outcome != "succeeded" && r.outcome != "failed") || !paired ||
			(r.taskStatus != "" && (!validStatuses[r.taskStatus] || r.revision < 1)) ||
			(r.outcome == "failed" && r.taskStatus == "done") || r.tag != "" || r.task != "" {
			return receiptIntegrityError(ReceiptKindRun)
		}
	case "cancel":
		if r.runID <= 0 || r.ttl != 0 || !zeroFinishFields || r.tag != "" || r.task != "" {
			return receiptIntegrityError(ReceiptKindRun)
		}
		if r.force {
			if r.owner != "" || r.attempt != 0 {
				return receiptIntegrityError(ReceiptKindRun)
			}
		} else if ValidateOwnerLabel(r.owner) != nil || r.attempt <= 0 {
			return receiptIntegrityError(ReceiptKindRun)
		}
	default:
		return receiptIntegrityError(ReceiptKindRun)
	}
	return nil
}

func (t *tx) runReceiptRefs(lineage runReadLineage) ([]RunReceiptRef, error) {
	rows, err := t.Query(`SELECT request_id FROM receipts WHERE kind=? ORDER BY created_at,receipt_id`, ReceiptKindRun)
	if err != nil {
		return nil, fmt.Errorf("list run receipts: %w", err)
	}
	var requestIDs []string
	for rows.Next() {
		var requestID sql.NullString
		if err := rows.Scan(&requestID); err != nil {
			rows.Close()
			return nil, err
		}
		if !requestID.Valid {
			rows.Close()
			return nil, receiptIntegrityError(ReceiptKindRun)
		}
		requestIDs = append(requestIDs, requestID.String)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	var refs []sequencedRunReceipt
	seenSeq := map[int64]bool{}
	seenRequests := map[string]string{}
	seenAttempts := map[string]int64{}
	for _, requestID := range requestIDs {
		receipt, err := t.findVerifiedReceipt(ReceiptKindRun, requestID)
		if err != nil {
			return nil, err
		}
		if receipt == nil {
			return nil, receiptIntegrityError(ReceiptKindRun)
		}
		receiptRunID, ok := receiptInt64(receipt["runId"])
		if !ok {
			return nil, receiptIntegrityError(ReceiptKindRun)
		}
		if receiptRunID != lineage.runID {
			continue
		}
		if err := validateStoredRunReceipt(t, receipt); err != nil {
			return nil, fmt.Errorf("validate run receipt %q: %w", requestID, err)
		}
		seqObj, ok := receipt["eventSeq"].(map[string]any)
		seq, seqOK := receiptInt64(seqObj["first"])
		operation, opOK := receipt["operation"].(string)
		receiptID, idOK := receipt["receiptId"].(string)
		digest, digestOK := receipt["receiptDigest"].(string)
		committedAt, atOK := receipt["committedAt"].(string)
		if !ok || !seqOK || !opOK || !idOK || !digestOK || !atOK || seenSeq[seq] {
			return nil, receiptIntegrityError(ReceiptKindRun)
		}
		seenSeq[seq] = true
		seenRequests[requestID] = operation
		receiptTag, tagOK := receipt["tag"].(string)
		receiptTask, taskOK := receipt["task"].(string)
		receiptOwner, ownerOK := receipt["owner"].(string)
		receiptForce, forceOK := receipt["force"].(bool)
		receiptAttempt, attemptOK := receiptInt64(receipt["attempt"])
		receiptGeneration, generationOK := receiptInt64(receipt["leaseGeneration"])
		receiptAttemptID, attemptIDOK := receipt["attemptAcquisitionId"].(string)
		receiptClaimID, claimIDOK := receipt["claimAcquisitionId"].(string)
		receiptAttemptExpires, expiresOK := receipt["attemptExpiresAt"].(string)
		attemptLineage, durableAttempt := lineage.attempts[receiptAttempt]
		if !tagOK || !taskOK || !ownerOK || !forceOK || !attemptOK || !generationOK || !attemptIDOK ||
			!claimIDOK || !expiresOK || receiptTag != lineage.tag || receiptTask != lineage.task || !durableAttempt {
			return nil, receiptIntegrityError(ReceiptKindRun)
		}
		seenAttempts[requestID] = receiptAttempt
		if receiptForce {
			if operation != "cancel" || receiptOwner != "" || receiptGeneration != 0 ||
				receiptAttemptID != "" || receiptClaimID != "" || receiptAttemptExpires != "" {
				return nil, receiptIntegrityError(ReceiptKindRun)
			}
		} else {
			if receiptOwner != lineage.owner || receiptClaimID != lineage.claimAcquisition ||
				receiptAttemptID != attemptLineage.acquisitionID || receiptGeneration < 1 ||
				receiptGeneration > attemptLineage.leaseGeneration ||
				(receiptGeneration == attemptLineage.leaseGeneration && receiptAttemptExpires != attemptLineage.expiresAt) {
				return nil, receiptIntegrityError(ReceiptKindRun)
			}
		}
		refs = append(refs, sequencedRunReceipt{seq: seq, ref: RunReceiptRef{Operation: operation,
			RequestID: requestID, ReceiptID: receiptID, ReceiptDigest: digest, CommittedAt: committedAt}})
	}
	if seenRequests[lineage.startRequest] != "start" || seenAttempts[lineage.startRequest] != 1 {
		return nil, receiptIntegrityError(ReceiptKindRun)
	}
	first, ok := lineage.attempts[1]
	if !ok || first.createdRequest != lineage.startRequest {
		return nil, receiptIntegrityError(ReceiptKindRun)
	}
	for attempt, durable := range lineage.attempts {
		if attempt > 1 && (seenRequests[durable.createdRequest] != "retry" || seenAttempts[durable.createdRequest] != attempt) {
			return nil, receiptIntegrityError(ReceiptKindRun)
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].seq < refs[j].seq })
	result := make([]RunReceiptRef, len(refs))
	for i := range refs {
		result[i] = refs[i].ref
	}
	return result, nil
}

func readRun(t *tx, runID int64) (*RunRecord, error) {
	var terminalAt, terminalDigest sql.NullString
	var claimAcquisition, startRequest string
	record := RunRecord{Attempts: []RunAttempt{}, Receipts: []RunReceiptRef{}}
	err := t.QueryRow(`SELECT run_id,tag,task_id,owner,claim_acquisition_id,start_request_id,status,
		current_attempt,started_at,updated_at,terminal_at,terminal_result_digest FROM runs WHERE run_id=?`, runID).Scan(&record.RunID,
		&record.Tag, &record.Task, &record.Owner, &claimAcquisition, &startRequest, &record.Status, &record.CurrentAttempt,
		&record.StartedAt, &record.UpdatedAt, &terminalAt, &terminalDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read run %d: %w", runID, err)
	}
	record.TerminalAt = nullableStringPointer(terminalAt)
	record.TerminalResultDigest = nullableStringPointer(terminalDigest)
	lineage := runReadLineage{runID: runID, tag: record.Tag, task: record.Task, owner: record.Owner,
		claimAcquisition: claimAcquisition, startRequest: startRequest, attempts: map[int64]runAttemptLineage{}}
	rows, err := t.Query(`SELECT attempt_no,acquisition_id,created_request_id,status,ttl_seconds,lease_generation,
		started_at,expires_at,updated_at,terminal_at,terminal_result_digest FROM attempts
		WHERE run_id=? ORDER BY attempt_no`, runID)
	if err != nil {
		return nil, fmt.Errorf("read run %d attempts: %w", runID, err)
	}
	for rows.Next() {
		var attempt RunAttempt
		var createdRequest string
		var attemptTerminal, attemptDigest sql.NullString
		if err := rows.Scan(&attempt.Attempt, &attempt.AcquisitionID, &createdRequest, &attempt.Status, &attempt.TTLSeconds,
			&attempt.LeaseGeneration, &attempt.StartedAt, &attempt.ExpiresAt, &attempt.UpdatedAt,
			&attemptTerminal, &attemptDigest); err != nil {
			rows.Close()
			return nil, err
		}
		attempt.TerminalAt = nullableStringPointer(attemptTerminal)
		attempt.TerminalResultDigest = nullableStringPointer(attemptDigest)
		record.Attempts = append(record.Attempts, attempt)
		lineage.attempts[attempt.Attempt] = runAttemptLineage{acquisitionID: attempt.AcquisitionID,
			createdRequest: createdRequest, leaseGeneration: attempt.LeaseGeneration, expiresAt: attempt.ExpiresAt}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(record.Attempts) == 0 {
		return nil, fmt.Errorf("run state invariant failed: run %d has no attempts", runID)
	}
	for i := range record.Attempts {
		want := int64(i + 1)
		if record.Attempts[i].Attempt != want {
			return nil, fmt.Errorf("run state invariant failed: run %d attempt history is not contiguous", runID)
		}
	}
	if record.Attempts[len(record.Attempts)-1].Attempt != record.CurrentAttempt {
		return nil, fmt.Errorf("run state invariant failed: run %d current attempt is not the greatest attempt", runID)
	}
	record.Receipts, err = t.runReceiptRefs(lineage)
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// GetRun returns a token-free run history, or nil when runID is absent.
func (d *DB) GetRun(runID int64) (*RunRecord, error) {
	if runID <= 0 {
		return nil, fmt.Errorf("run id must be a positive integer")
	}
	var result *RunRecord
	err := d.deferred(func(t *tx) error {
		var err error
		result, err = readRun(t, runID)
		return err
	})
	return result, err
}

// RunArgumentTask names one tag whose tasks table carries the bare id, with
// that task's current live (running or retryable) run id, zero when none.
type RunArgumentTask struct {
	Tag       string
	LiveRunID int64
}

// RunArgumentTaskHint resolves the §9.4 task-id-mistake diagnostic for a run
// positional argument that names no run: every tag whose tasks table carries
// that bare id, each with that task's current live run id, if any. Tags are
// returned in ascending name order; several tags reuse one id.
func (d *DB) RunArgumentTaskHint(candidate int64) ([]RunArgumentTask, error) {
	taskID := strconv.FormatInt(candidate, 10)
	var result []RunArgumentTask
	err := d.deferred(func(t *tx) error {
		rows, err := t.Query(`SELECT tag FROM tasks WHERE id=? ORDER BY tag`, taskID)
		if err != nil {
			return fmt.Errorf("run argument hint: %w", err)
		}
		var tags []string
		for rows.Next() {
			var tag string
			if err := rows.Scan(&tag); err != nil {
				rows.Close()
				return fmt.Errorf("run argument hint: %w", err)
			}
			tags = append(tags, tag)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("run argument hint: %w", err)
		}
		rows.Close()
		for _, tag := range tags {
			var liveRunID int64
			err := t.QueryRow(`SELECT run_id FROM runs WHERE tag=? AND task_id=? AND status IN ('running','retryable')
				ORDER BY run_id DESC LIMIT 1`, tag, taskID).Scan(&liveRunID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("run argument hint: %w", err)
			}
			result = append(result, RunArgumentTask{Tag: tag, LiveRunID: liveRunID})
		}
		return nil
	})
	return result, err
}

var validRunStatuses = map[string]bool{"running": true, "retryable": true, "succeeded": true, "cancelled": true}

// ListRuns returns token-free summaries ordered by ascending numeric run id.
// Empty filters are omitted and nonempty filters combine with AND.
func (d *DB) ListRuns(rawTask, status, owner string) ([]RunSummary, error) {
	task := ""
	if rawTask != "" {
		parent, subtask, err := backlog.ParseTaskReference(rawTask)
		if err != nil {
			return nil, err
		}
		if subtask != "" {
			return nil, fmt.Errorf("run list task filter requires a top-level task id")
		}
		task = parent
	}
	if status != "" && !validRunStatuses[status] {
		return nil, fmt.Errorf("invalid run status %q", status)
	}
	if owner != "" {
		if err := ValidateOwnerLabel(owner); err != nil {
			return nil, fmt.Errorf("run owner %v", err)
		}
	}
	result := []RunSummary{}
	err := d.deferred(func(t *tx) error {
		var query strings.Builder
		query.WriteString(`SELECT run_id,tag,task_id,owner,status,current_attempt,updated_at FROM runs WHERE 1=1`)
		var args []any
		if task != "" {
			query.WriteString(` AND task_id=?`)
			args = append(args, task)
		}
		if status != "" {
			query.WriteString(` AND status=?`)
			args = append(args, status)
		}
		if owner != "" {
			query.WriteString(` AND owner=?`)
			args = append(args, owner)
		}
		query.WriteString(` ORDER BY run_id`)
		rows, err := t.Query(query.String(), args...)
		if err != nil {
			return fmt.Errorf("list runs: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var summary RunSummary
			if err := rows.Scan(&summary.RunID, &summary.Tag, &summary.Task, &summary.Owner,
				&summary.Status, &summary.CurrentAttempt, &summary.UpdatedAt); err != nil {
				return err
			}
			result = append(result, summary)
		}
		return rows.Err()
	})
	return result, err
}
