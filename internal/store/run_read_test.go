package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func completedRunHistoryFixture(t *testing.T) (*DB, int64) {
	t.Helper()
	db, _ := newDB(t)
	id := addTask(t, db, "main", "history")
	claim, _, _, err := db.AcquireClaim("main", id, "agent-a", 3600, "claim.history")
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := db.StartRun("main", id, "agent-a", claim.Token, "run.history", 300)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.FinishRun(first.RunID, "agent-a", claim.Token, 1, first.AttemptToken,
		"failed", "finish.history.failed", "", 0, testResultDigest); err != nil {
		t.Fatal(err)
	}
	second, _, err := db.RetryRun(first.RunID, "agent-a", claim.Token, 1, first.AttemptToken, "retry.history", 600)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.FinishRun(first.RunID, "agent-a", claim.Token, 2, second.AttemptToken,
		"succeeded", "finish.history.succeeded", "", 0, ""); err != nil {
		t.Fatal(err)
	}
	return db, first.RunID
}

func TestGetRunReturnsOrderedTokenFreeHistory(t *testing.T) {
	db, runID := completedRunHistoryFixture(t)
	record, err := db.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if record == nil || record.RunID != runID || record.Tag != "main" || record.Task != "1" ||
		record.Owner != "agent-a" || record.Status != "succeeded" || record.CurrentAttempt != 2 ||
		record.TerminalAt == nil || record.TerminalResultDigest != nil {
		t.Fatalf("run record = %+v", record)
	}
	if len(record.Attempts) != 2 || record.Attempts[0].Attempt != 1 || record.Attempts[0].Status != "failed" ||
		record.Attempts[0].TerminalResultDigest == nil || *record.Attempts[0].TerminalResultDigest != testResultDigest ||
		record.Attempts[1].Attempt != 2 || record.Attempts[1].Status != "succeeded" ||
		record.Attempts[1].TerminalResultDigest != nil {
		t.Fatalf("attempt history = %+v", record.Attempts)
	}
	wantOperations := []string{"start", "finish", "retry", "finish"}
	var operations []string
	for _, receipt := range record.Receipts {
		operations = append(operations, receipt.Operation)
		if receipt.RequestID == "" || receipt.ReceiptID == "" || receipt.ReceiptDigest == "" || receipt.CommittedAt == "" {
			t.Fatalf("receipt reference = %+v", receipt)
		}
	}
	if !reflect.DeepEqual(operations, wantOperations) {
		t.Fatalf("receipt operations = %v; want %v", operations, wantOperations)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"attemptToken", "claimToken", "forceCancelFence", "claimAcquisitionId", "startRequestId"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("run show leaked %s: %s", secret, raw)
		}
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 12 {
		t.Fatalf("run show keys = %v", body)
	}
	attempts := body["attempts"].([]any)
	if len(attempts[0].(map[string]any)) != 10 {
		t.Fatalf("attempt keys = %v", attempts[0])
	}
	receipts := body["receipts"].([]any)
	if len(receipts[0].(map[string]any)) != 5 {
		t.Fatalf("receipt keys = %v", receipts[0])
	}
}

func TestGetRunAbsentAndReceiptCorruption(t *testing.T) {
	db, runID := completedRunHistoryFixture(t)
	missing, err := db.GetRun(runID + 99)
	if err != nil || missing != nil {
		t.Fatalf("missing run = %+v, %v", missing, err)
	}
	if _, err := db.db.Exec(`UPDATE receipts SET body=body||' ' WHERE kind='run' AND request_id='retry.history'`); err != nil {
		t.Fatal(err)
	}
	_, err = db.GetRun(runID)
	var integrity *ReceiptIntegrityError
	if !errors.As(err, &integrity) {
		t.Fatalf("corrupt receipt show = %v", err)
	}
}

func TestGetRunRequiresCreationReceiptCompleteness(t *testing.T) {
	for _, requestID := range []string{"run.history", "retry.history"} {
		t.Run(requestID, func(t *testing.T) {
			db, runID := completedRunHistoryFixture(t)
			if _, err := db.db.Exec(`DELETE FROM receipts WHERE kind='run' AND request_id=?`, requestID); err != nil {
				t.Fatal(err)
			}
			_, err := db.GetRun(runID)
			if !errors.As(err, new(*ReceiptIntegrityError)) {
				t.Fatalf("missing creation receipt %s = %v", requestID, err)
			}
		})
	}
}

func TestGetRunRejectsNonContiguousOrMismatchedCurrentAttempt(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*testing.T, *DB, int64)
	}{
		{
			name: "gap",
			corrupt: func(t *testing.T, db *DB, runID int64) {
				if _, err := db.db.Exec(`UPDATE runs SET current_attempt=1 WHERE run_id=?`, runID); err != nil {
					t.Fatal(err)
				}
				if _, err := db.db.Exec(`UPDATE attempts SET attempt_no=3 WHERE run_id=? AND attempt_no=2`, runID); err != nil {
					t.Fatal(err)
				}
				if _, err := db.db.Exec(`UPDATE runs SET current_attempt=3 WHERE run_id=?`, runID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "current is not greatest",
			corrupt: func(t *testing.T, db *DB, runID int64) {
				if _, err := db.db.Exec(`UPDATE runs SET current_attempt=1 WHERE run_id=?`, runID); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, runID := completedRunHistoryFixture(t)
			tt.corrupt(t, db, runID)
			record, err := db.GetRun(runID)
			if record != nil || err == nil || !strings.Contains(err.Error(), "run state invariant failed") {
				t.Fatalf("corrupt attempt history = %+v, %v", record, err)
			}
		})
	}
}

func TestGetRunRejectsCoherentlyRehashedInvalidStoredInput(t *testing.T) {
	db, runID := completedRunHistoryFixture(t)
	receipt, err := db.FindReceipt(ReceiptKindRun, "retry.history")
	if err != nil || receipt == nil {
		t.Fatalf("retry receipt = %v, %v", receipt, err)
	}
	receipt["sourceAttempt"] = int64(0)
	ttl, _ := receiptInt64(receipt["ttlSeconds"])
	owner, _ := receipt["owner"].(string)
	storedRunID, _ := receiptInt64(receipt["runId"])
	requestID, _ := receipt["requestId"].(string)
	inputDigest, err := runInputDigest(runRequest{operation: "retry", requestID: requestID,
		owner: owner, runID: storedRunID, attempt: 0, ttl: int(ttl)})
	if err != nil {
		t.Fatal(err)
	}
	receipt["inputDigest"] = inputDigest
	receiptID := runReceiptID(requestID, inputDigest)
	receipt["receiptId"] = receiptID
	digest, err := receiptDigest(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt["receiptDigest"] = digest
	body, err := canonicalJSON(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE receipts SET receipt_id=?,body=?,digest=? WHERE kind='run' AND request_id=?`,
		receiptID, string(body), digest, requestID); err != nil {
		t.Fatal(err)
	}
	seqObj, _ := receipt["eventSeq"].(map[string]any)
	seq, _ := receiptInt64(seqObj["first"])
	attempt, _ := receiptInt64(receipt["attempt"])
	extra, err := json.Marshal(runEventExtra(storedRunID, attempt, receiptID, digest))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE events SET extra=? WHERE seq=?`, string(extra), seq); err != nil {
		t.Fatal(err)
	}
	_, err = db.GetRun(runID)
	if !errors.As(err, new(*ReceiptIntegrityError)) {
		t.Fatalf("invalid normalized retry input = %v", err)
	}
}

func TestListRunsFiltersAndOrdersWithoutSecrets(t *testing.T) {
	db, _ := newDB(t)
	start := func(tag, owner, request string) int64 {
		id := addTask(t, db, tag, request)
		claim, _, _, err := db.AcquireClaim(tag, id, owner, 3600, "claim."+request)
		if err != nil {
			t.Fatal(err)
		}
		run, _, err := db.StartRun(tag, id, owner, claim.Token, "run."+request, 300)
		if err != nil {
			t.Fatal(err)
		}
		return run.RunID
	}
	first := start("main", "agent-a", "one")
	second := start("aux", "agent-b", "two")
	thirdID := addTask(t, db, "main", "three")
	claim, _, _, err := db.AcquireClaim("main", thirdID, "agent-a", 3600, "claim.three")
	if err != nil {
		t.Fatal(err)
	}
	third, _, err := db.StartRun("main", thirdID, "agent-a", claim.Token, "run.three", 300)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.CancelRun(third.RunID, "agent-a", claim.Token, 1, third.AttemptToken, false, "cancel.three"); err != nil {
		t.Fatal(err)
	}

	all, err := db.ListRuns("", "", "")
	if err != nil || len(all) != 3 || all[0].RunID != first || all[1].RunID != second || all[2].RunID != third.RunID {
		t.Fatalf("all runs = %+v, %v", all, err)
	}
	byTask, err := db.ListRuns("1", "", "")
	if err != nil || len(byTask) != 2 || byTask[0].Tag != "main" || byTask[1].Tag != "aux" {
		t.Fatalf("task-filtered runs = %+v, %v", byTask, err)
	}
	cancelled, err := db.ListRuns("", "cancelled", "agent-a")
	if err != nil || len(cancelled) != 1 || cancelled[0].RunID != third.RunID {
		t.Fatalf("combined filters = %+v, %v", cancelled, err)
	}
	empty, err := db.ListRuns("", "succeeded", "")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty list = %#v, %v", empty, err)
	}
	raw, _ := json.Marshal(all)
	for _, secret := range []string{"attemptToken", "claimToken", "acquisition", "expiresAt", "terminalResultDigest", "receipt"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("run list leaked %s: %s", secret, raw)
		}
	}
	var summaries []map[string]any
	if err := json.Unmarshal(raw, &summaries); err != nil || len(summaries[0]) != 7 {
		t.Fatalf("summary shape = %v, %v", summaries, err)
	}
	if _, err := db.ListRuns("1.2", "", ""); err == nil {
		t.Fatal("subtask run filter succeeded")
	}
	if _, err := db.ListRuns("", "pending", ""); err == nil {
		t.Fatal("invalid run status filter succeeded")
	}
}
