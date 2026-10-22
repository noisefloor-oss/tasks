package store

import (
	"errors"
	"testing"
)

func TestClaimNextReceiptReplayAndNoWorkReservation(t *testing.T) {
	db, _ := newDB(t)
	if task, claim, replayed, err := db.ClaimNext("main", "agent-a", 300, "", "", "req-empty"); err != nil || task != nil || claim != nil || replayed {
		t.Fatalf("empty claim-next = %v %+v replayed=%v err=%v", task, claim, replayed, err)
	}
	if receipt, err := db.FindReceipt(ReceiptKindClaim, "req-empty"); err != nil || receipt != nil {
		t.Fatalf("empty claim-next reserved a receipt: %v %v", receipt, err)
	}
	id := addTask(t, db, "main", "next")
	task, claim, replayed, err := db.ClaimNext("main", "agent-a", 300, "", "", "req-empty")
	if err != nil || task == nil || task.ID != id || claim == nil || replayed {
		t.Fatalf("claim-next = %v %+v replayed=%v err=%v", task, claim, replayed, err)
	}
	again, againClaim, againReplayed, err := db.ClaimNext("main", "agent-a", 300, "", "", "req-empty")
	if err != nil || !againReplayed || again == nil || again.ID != id || againClaim.Token != claim.Token {
		t.Fatalf("claim-next replay = %v %+v replayed=%v err=%v", again, againClaim, againReplayed, err)
	}
	if _, _, _, err := db.ClaimNext("main", "agent-a", 600, "", "", "req-empty"); !errors.As(err, new(*ReplayConflict)) {
		t.Fatalf("divergent replay = %v, want ReplayConflict", err)
	}
	receipt, err := db.FindReceipt(ReceiptKindClaim, "req-empty")
	if err != nil || receipt["operation"] != "claim-next" || receipt["task"] != id {
		t.Fatalf("claim-next receipt = %v %v", receipt, err)
	}
}

func TestClaimNextReplayRejectsSameSecondReacquisition(t *testing.T) {
	db, _ := newDB(t)
	id := addTask(t, db, "main", "identity")
	first, firstClaim, _, err := db.ClaimNext("main", "agent-a", 300, "", "", "req-first-next")
	if err != nil || first == nil {
		t.Fatalf("first claim-next = %v %v", first, err)
	}
	if err := db.ReleaseClaim("main", id, "agent-a", firstClaim.Token, false); err != nil {
		t.Fatal(err)
	}
	second, secondClaim, _, err := db.ClaimNext("main", "agent-a", 300, "", "", "req-second-next")
	if err != nil || second == nil {
		t.Fatalf("second claim-next = %v %v", second, err)
	}
	if _, err := db.db.Exec(`UPDATE tasks SET claim_acquired_at=? WHERE tag='main' AND id=?`, firstClaim.AcquiredAt, id); err != nil {
		t.Fatal(err)
	}
	task, claim, _, err := db.ClaimNext("main", "agent-a", 300, "", "", "req-first-next")
	assertRedactedReplayConflict(t, err)
	if task != nil || claim != nil || secondClaim.Token == firstClaim.Token {
		t.Fatalf("superseded replay exposed a claim: task=%v claim=%+v", task, claim)
	}
}

func TestClaimNextOwnerValidationAtStoreBoundary(t *testing.T) {
	db, _ := newDB(t)
	addTask(t, db, "main", "validation")
	if _, _, _, err := db.ClaimNext("main", " padded ", 300, "", "", "req-owner"); err == nil {
		t.Fatal("ClaimNext accepted an invalid owner")
	}
	if _, _, _, err := db.ClaimNext("main", "agent-a", 300, "", " padded ", "req-assignment"); err == nil {
		t.Fatal("ClaimNext accepted an invalid assignment owner")
	}
}
