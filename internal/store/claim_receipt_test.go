package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestClaimDigestVectors(t *testing.T) {
	inputs := []struct {
		request claimRequest
		want    string
	}{
		{
			request: claimRequest{operation: "claim", requestID: "req-1", tag: "main", task: "7", owner: "agent-a", ttl: 300},
			want:    "sha256:52a1a004dbb2555aba4ff30d436e32227a0cc7455bfd0a5f691700bacf379dd7",
		},
		{
			request: claimRequest{operation: "claim-next", requestID: "root.cn", tag: "main", owner: "scheduler", ttl: 900, milestone: "M1", assignment: "agent-a"},
			want:    "sha256:065e223fdef031d2a6d1a7173e6cf4d2cc8dfe3a116e30e4a6b0b3d8d4738a9e",
		},
	}
	for _, vector := range inputs {
		got, err := claimInputDigest(vector.request)
		if err != nil || got != vector.want {
			t.Errorf("%s input digest = %q, %v; want %q", vector.request.operation, got, err, vector.want)
		}
	}
	got, err := claimResultDigestValues("7", 3, "agent-a", "00112233445566778899aabbccddeeff",
		"2026-08-28T12:00:00Z", "2026-08-28T12:05:00Z")
	want := "sha256:184ca831045fd0bd7aa6b77793498b6151454d11ebf435652f5b3c89541bc468"
	if err != nil || got != want {
		t.Errorf("result digest = %q, %v; want %q", got, err, want)
	}
}

func TestClaimReceiptAcquisitionAndReplay(t *testing.T) {
	db, _ := newDB(t)
	id := addTask(t, db, "main", "receipt")
	claim, revision, replayed, err := db.AcquireClaim("main", id, "agent-a", 300, "req-receipt")
	if err != nil || replayed {
		t.Fatalf("acquire: claim=%+v replayed=%v err=%v", claim, replayed, err)
	}
	receipt, err := db.FindReceipt(ReceiptKindClaim, "req-receipt")
	if err != nil || receipt == nil {
		t.Fatalf("receipt: %v %v", receipt, err)
	}
	for _, field := range []string{"schemaVersion", "kind", "receiptId", "familyId", "requestId",
		"inputDigest", "resultDigest", "operation", "tag", "task", "owner", "ttlSeconds",
		"acquisitionId", "revision", "acquiredAt", "expiresAt", "inventory", "eventSeq",
		"committedAt", "receiptDigest"} {
		if _, ok := receipt[field]; !ok {
			t.Fatalf("receipt missing %q: %v", field, receipt)
		}
	}
	if receipt["operation"] != "claim" || receipt["task"] != id || receipt["owner"] != "agent-a" ||
		receipt["revision"] != json.Number("1") || receipt["acquisitionId"] != claimAcquisitionID(claim) {
		t.Fatalf("receipt identity = %v", receipt)
	}
	body, err := canonicalJSON(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "token") || strings.Contains(string(body), "fencing") {
		t.Fatalf("receipt leaked a fencing capability: %s", body)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	var acquired *Event
	for i := range feed.Events {
		if feed.Events[i].Kind == "claim" && feed.Events[i].Task == id {
			acquired = &feed.Events[i]
		}
	}
	if acquired == nil || acquired.Action != "acquired" {
		t.Fatalf("acquisition event = %+v", acquired)
	}
	var eventReceiptID, eventDigest string
	if err := json.Unmarshal(acquired.Extra["receiptId"], &eventReceiptID); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(acquired.Extra["receiptDigest"], &eventDigest); err != nil {
		t.Fatal(err)
	}
	if eventReceiptID != receipt["receiptId"] || eventDigest != receipt["receiptDigest"] {
		t.Fatalf("event linkage %q/%q != receipt %v/%v", eventReceiptID, eventDigest,
			receipt["receiptId"], receipt["receiptDigest"])
	}
	beforeFence, err := db.Fencing()
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents := len(feed.Events)
	again, replayRevision, replayed, err := db.AcquireClaim("main", id, "agent-a", 300, "req-receipt")
	if err != nil || !replayed || again.Token != claim.Token || replayRevision != revision {
		t.Fatalf("replay: claim=%+v revision=%d replayed=%v err=%v", again, replayRevision, replayed, err)
	}
	afterFence, _ := db.Fencing()
	afterFeed, _ := db.Feed(0)
	if afterFence != beforeFence || len(afterFeed.Events) != beforeEvents {
		t.Fatalf("replay repeated effects: fence %d->%d events %d->%d",
			beforeFence, afterFence, beforeEvents, len(afterFeed.Events))
	}
	if _, _, _, err := db.AcquireClaim("main", id, "agent-a", 600, "req-receipt"); !errors.As(err, new(*ReplayConflict)) {
		t.Fatalf("divergent replay: %v, want ReplayConflict", err)
	}
}

func TestClaimReplayBindsExactAcquisitionAndRecordedRevision(t *testing.T) {
	db, _ := newDB(t)
	id := addTask(t, db, "main", "identity")
	first, _, _, err := db.AcquireClaim("main", id, "agent-a", 300, "req-first")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReleaseClaim("main", id, "agent-a", first.Token, false); err != nil {
		t.Fatal(err)
	}
	second, _, _, err := db.AcquireClaim("main", id, "agent-a", 300, "req-second")
	if err != nil {
		t.Fatal(err)
	}
	if claimAcquisitionID(first) == claimAcquisitionID(second) || !validAcquisitionID(claimAcquisitionID(second)) {
		t.Fatalf("reacquisition identity = %q after %q", claimAcquisitionID(second), claimAcquisitionID(first))
	}
	if _, err := db.db.Exec(`UPDATE tasks SET claim_acquired_at=? WHERE tag='main' AND id=?`, first.AcquiredAt, id); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = db.AcquireClaim("main", id, "agent-a", 300, "req-first")
	assertRedactedReplayConflict(t, err)
	if err := db.ReleaseClaim("main", id, "agent-a", second.Token, false); err != nil {
		t.Fatal(err)
	}

	current, recordedRevision, _, err := db.AcquireClaim("main", id, "agent-a", 300, "req-renew")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTask("main", id, recordedRevision, &ClaimGate{Owner: "agent-a", Token: current.Token},
		func(task *Task) error { task.Title = "mutated"; return nil }); err != nil {
		t.Fatal(err)
	}
	renewed, _, err := db.RenewClaim("main", id, "agent-a", current.Token, 900)
	if err != nil {
		t.Fatal(err)
	}
	replayedClaim, replayRevision, replayed, err := db.AcquireClaim("main", id, "agent-a", 300, "req-renew")
	if err != nil || !replayed || replayedClaim.Token != renewed.Token ||
		replayedClaim.ExpiresAt != renewed.ExpiresAt || replayRevision != recordedRevision {
		t.Fatalf("renewed replay: claim=%+v revision=%d replayed=%v err=%v", replayedClaim, replayRevision, replayed, err)
	}
}

func TestClaimReplayRejectsMalformedIdentityMetadata(t *testing.T) {
	cases := map[string]func(map[string]any){
		"missing revision": func(receipt map[string]any) { delete(receipt, "revision") },
		"string revision":  func(receipt map[string]any) { receipt["revision"] = "1" },
		"zero revision":    func(receipt map[string]any) { receipt["revision"] = int64(0) },
		"missing identity": func(receipt map[string]any) { delete(receipt, "acquisitionId") },
		"bad identity":     func(receipt map[string]any) { receipt["acquisitionId"] = "not-128-bit-hex" },
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			db, _ := newDB(t)
			id := addTask(t, db, "main", name)
			if _, _, _, err := db.AcquireClaim("main", id, "agent-a", 300, "req-corrupt"); err != nil {
				t.Fatal(err)
			}
			receipt, err := db.FindReceipt(ReceiptKindClaim, "req-corrupt")
			if err != nil {
				t.Fatal(err)
			}
			corrupt(receipt)
			body, err := canonicalJSON(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.db.Exec(`UPDATE receipts SET body=? WHERE kind=? AND request_id=?`,
				string(body), ReceiptKindClaim, "req-corrupt"); err != nil {
				t.Fatal(err)
			}
			_, _, _, err = db.AcquireClaim("main", id, "agent-a", 300, "req-corrupt")
			assertRedactedReceiptRefusal(t, err)
		})
	}
}

func TestClaimReplayVerifiesStoredReceipt(t *testing.T) {
	type corruption struct {
		name   string
		mutate func(map[string]any)
		rowSQL string
		rowArg any
	}
	cases := []corruption{
		{name: "noncanonical body", rowSQL: `UPDATE receipts SET body=body||' ' WHERE kind=? AND request_id=?`},
		{name: "row digest", rowSQL: `UPDATE receipts SET digest=? WHERE kind=? AND request_id=?`, rowArg: "sha256:" + strings.Repeat("0", 64)},
		{name: "row receipt id", rowSQL: `UPDATE receipts SET receipt_id=? WHERE kind=? AND request_id=?`, rowArg: "claim-altered"},
		{name: "row commit time", rowSQL: `UPDATE receipts SET created_at=? WHERE kind=? AND request_id=?`, rowArg: "2027-01-01T00:00:00Z"},
		{name: "embedded digest", mutate: func(r map[string]any) { r["receiptDigest"] = "sha256:" + strings.Repeat("1", 64) }},
		{name: "schema", mutate: func(r map[string]any) { r["schemaVersion"] = int64(2) }},
		{name: "kind", mutate: func(r map[string]any) { r["kind"] = "run" }},
		{name: "embedded receipt id", mutate: func(r map[string]any) { r["receiptId"] = "claim-altered" }},
		{name: "family", mutate: func(r map[string]any) { r["familyId"] = "00000000-0000-4000-8000-000000000000" }},
		{name: "request", mutate: func(r map[string]any) { r["requestId"] = "other-request" }},
		{name: "commit time", mutate: func(r map[string]any) { r["committedAt"] = "2027-01-01T00:00:00Z" }},
		{name: "input digest", mutate: func(r map[string]any) { r["inputDigest"] = "sha256:" + strings.Repeat("2", 64) }},
		{name: "result digest", mutate: func(r map[string]any) { r["resultDigest"] = "sha256:" + strings.Repeat("3", 64) }},
		{name: "unknown key", mutate: func(r map[string]any) { r["unexpected"] = true }},
		{name: "wrong field type", mutate: func(r map[string]any) { r["ttlSeconds"] = "300" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := newDB(t)
			id := addTask(t, db, "main", tc.name)
			_, _, _, err := db.AcquireClaim("main", id, "agent-a", 300, "req-verify")
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := db.FindReceipt(ReceiptKindClaim, "req-verify")
			if err != nil {
				t.Fatal(err)
			}
			if tc.mutate != nil {
				tc.mutate(receipt)
				// Re-sign the altered body unless the embedded digest itself is
				// the corruption under test. This reaches semantic envelope checks
				// instead of relying only on a stale row digest.
				if tc.name != "embedded digest" {
					digest, err := receiptDigest(receipt)
					if err != nil {
						t.Fatal(err)
					}
					receipt["receiptDigest"] = digest
				}
				body, err := canonicalJSON(receipt)
				if err != nil {
					t.Fatal(err)
				}
				rowDigest, err := receiptDigest(receipt)
				if err != nil {
					t.Fatal(err)
				}
				if tc.name == "embedded digest" {
					rowDigest, _ = receipt["receiptDigest"].(string)
				}
				if _, err := db.db.Exec(`UPDATE receipts SET body=?, digest=? WHERE kind=? AND request_id=?`,
					string(body), rowDigest, ReceiptKindClaim, "req-verify"); err != nil {
					t.Fatal(err)
				}
			} else if tc.rowArg == nil {
				if _, err := db.db.Exec(tc.rowSQL, ReceiptKindClaim, "req-verify"); err != nil {
					t.Fatal(err)
				}
			} else if _, err := db.db.Exec(tc.rowSQL, tc.rowArg, ReceiptKindClaim, "req-verify"); err != nil {
				t.Fatal(err)
			}

			beforeFence, _ := db.Fencing()
			beforeFeed, _ := db.Feed(0)
			replayed, _, _, err := db.AcquireClaim("main", id, "agent-a", 300, "req-verify")
			if replayed != nil {
				t.Fatalf("altered receipt returned capability token %d", replayed.Token)
			}
			assertRedactedReceiptRefusal(t, err)
			afterFence, _ := db.Fencing()
			afterFeed, _ := db.Feed(0)
			if afterFence != beforeFence || len(afterFeed.Events) != len(beforeFeed.Events) {
				t.Fatalf("altered replay repeated effects: fence %d->%d events %d->%d",
					beforeFence, afterFence, len(beforeFeed.Events), len(afterFeed.Events))
			}
		})
	}
}

func assertRedactedReceiptRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("altered receipt replay succeeded")
	}
	var integrity *ReceiptIntegrityError
	var conflict *ReplayConflict
	if !errors.As(err, &integrity) && !errors.As(err, &conflict) {
		t.Fatalf("altered receipt replay error = %T %v, want integrity or replay conflict", err, err)
	}
	if strings.Contains(err.Error(), "agent-a") || strings.Contains(strings.ToLower(err.Error()), "token") {
		t.Fatalf("altered receipt refusal disclosed capability metadata: %v", err)
	}
}

func assertRedactedReplayConflict(t *testing.T, err error) {
	t.Helper()
	var conflict *ClaimConflict
	if !errors.As(err, &conflict) || conflict.Kind != "replay" {
		t.Fatalf("replay error = %v, want ClaimConflict", err)
	}
	if conflict.Owner != "" || conflict.Expiry != "" || conflict.FencingToken != 0 {
		t.Fatalf("replay conflict disclosed claim state: %+v", conflict)
	}
}

func TestClaimOwnerValidationAtStoreBoundaries(t *testing.T) {
	db, _ := newDB(t)
	id := addTask(t, db, "main", "owner validation")
	bad := " padded "
	if _, _, _, err := db.AcquireClaim("main", id, bad, 300, "req-owner"); err == nil {
		t.Fatal("AcquireClaim accepted an invalid owner")
	}
	if _, _, err := db.RenewClaim("main", id, bad, 1, 300); err == nil {
		t.Fatal("RenewClaim accepted an invalid owner")
	}
	if err := db.ReleaseClaim("main", id, bad, 1, true); err == nil {
		t.Fatal("ReleaseClaim accepted an invalid owner")
	}
	if _, _, _, err := db.ClaimWorktree("", "main", id, bad, 300, false); err == nil {
		t.Fatal("ClaimWorktree accepted an invalid owner")
	}
	if _, err := db.ReleaseClaimWorktree("", "main", id, bad, 1, true, false); err == nil {
		t.Fatal("ReleaseClaimWorktree accepted an invalid owner")
	}
}

func TestClaimReceiptInventoryMatchesDigester(t *testing.T) {
	db, _ := newDB(t)
	if err := db.EnsureTag("main"); err != nil {
		t.Fatal(err)
	}
	plan := mustOrchestratorPlan(t, `{
		"schemaVersion":1,"planId":"inventory-plan","tag":"main","milestones":[],
		"tasks":[{"key":"assigned","title":"assigned work","description":"","details":"","priority":"medium","dependencies":[]}],
		"assignments":[{"key":"primary","task":{"key":"assigned"},"owner":"orchestrator"}]
	}`)
	result, err := db.ApplyOrchestratorPlan(plan, plan.Digest, "inventory-plan-apply")
	if err != nil {
		t.Fatal(err)
	}
	mappings, err := receiptMappings(result.Receipt)
	if err != nil || len(mappings) != 1 {
		t.Fatalf("plan mappings = %v %v", mappings, err)
	}
	if _, _, _, err := db.AcquireClaim("main", mappings[0].ID, "orchestrator", 300, "inventory-claim"); err != nil {
		t.Fatal(err)
	}
	receipt, err := db.FindReceipt(ReceiptKindClaim, "inventory-claim")
	if err != nil || receipt == nil {
		t.Fatalf("claim receipt: %v %v", receipt, err)
	}
	inv, ok := receipt["inventory"].(map[string]any)
	if !ok {
		t.Fatalf("inventory = %v", receipt["inventory"])
	}
	_, _, want, err := db.ClassDigests()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{
		"tags": want.Tags, "milestones": want.Milestones, "tasks": want.Tasks,
		"dependencies": want.Dependencies, "subtasks": want.Subtasks, "bindings": want.Bindings,
		"assignments": want.Assignments, "claims": want.Claims, "events": want.Events,
		"receipts": want.Receipts, "plans": want.Plans, "planMappings": want.PlanMappings,
		"runs": want.Runs, "attempts": want.Attempts, "worktreeSagas": want.WorktreeSagas,
	}
	for class, count := range counts {
		got, ok := inv[class].(json.Number)
		if !ok {
			t.Fatalf("inventory %s = %v", class, inv[class])
		}
		n, err := got.Int64()
		if err != nil || int(n) != count {
			t.Fatalf("inventory %s = %v, want %d", class, got, count)
		}
	}
	for _, class := range []string{"assignments", "plans", "planMappings"} {
		if inv[class] == json.Number("0") {
			t.Fatalf("inventory %s unexpectedly zero", class)
		}
	}
}

func TestClaimExpiryReacquisitionDoesNotReplayOldToken(t *testing.T) {
	db, _ := newDB(t)
	id := addTask(t, db, "main", "expiry")
	first, _, _, err := db.AcquireClaim("main", id, "agent-a", 300, "req-expired")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE tasks SET claim_expires_at=? WHERE tag='main' AND id=?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), id); err != nil {
		t.Fatal(err)
	}
	second, _, _, err := db.AcquireClaim("main", id, "agent-a", 300, "req-after-expiry")
	if err != nil {
		t.Fatal(err)
	}
	if second.Token == first.Token {
		t.Fatal("expiry reacquisition reused fencing token")
	}
	_, _, _, err = db.AcquireClaim("main", id, "agent-a", 300, "req-expired")
	assertRedactedReplayConflict(t, err)
}
