package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// ReceiptKindClaim is the §8.4 receipt kind both claim and claim-next
// acquisitions embed. One kind, one store-wide (kind, requestId) replay rule.
const ReceiptKindClaim = "claim"

// claimRequest is the normalized, non-secret identity of one claim
// acquisition request (SPEC §8.3/§8.4). The fencing token is a result,
// never an input, so it never reaches the input digest or the receipt.
type claimRequest struct {
	operation  string // "claim" | "claim-next"
	requestID  string
	tag        string
	task       string // "" for claim-next: selection is a result, not an input
	owner      string
	ttl        int // normalized: the default is already applied
	milestone  string
	assignment string
}

// validateClaimRequestID enforces the §9.1 local-key grammar every operation
// request id uses, before any effect. Both acquisition verbs share it.
func validateClaimRequestID(requestID string) error {
	if !ValidLocalKey(requestID) {
		return fmt.Errorf("claim request id is invalid: must match %s", localKeyGrammar)
	}
	return nil
}

// claimInputDigest digests the operation's normalized, non-secret inputs:
// request id, operation, tag, task id (claim only), owner, ttl, and the
// claim-next selection filters. The ttl is the normalized value, so an
// omitted --ttl and an explicit default digest identically.
func claimInputDigest(r claimRequest) (string, error) {
	canonical, err := canonicalJSON(map[string]any{
		"requestId":  r.requestID,
		"operation":  r.operation,
		"tag":        r.tag,
		"task":       r.task,
		"owner":      r.owner,
		"ttlSeconds": int64(r.ttl),
		"milestone":  r.milestone,
		"assignment": r.assignment,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// claimReceiptID derives the deterministic claim receipt id for one request.
// Re-running the same request with the same normalized inputs names the same
// receipt, which is what makes replay return the original record.
func claimReceiptID(requestID, inputDigest string) string {
	sum := sha256.Sum256([]byte("noise-tasks-claim-v1\x00" + requestID + "\x00" + inputDigest))
	return "claim-" + hex.EncodeToString(sum[:8])
}

// newAcquisitionID mints one acquisition identity (SPEC §8.3): 128 random
// bits, hex-encoded. It is non-secret and unique per acquisition, and its
// randomness is what keeps the sequential fencing token unguessable from it.
func newAcquisitionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint acquisition identity: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// acquisitionIDExtra renders one acquisition identity as claim extra fields.
func acquisitionIDExtra(id string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"acquisitionId": json.RawMessage(strconv.Quote(id))}
}

// claimAcquisitionID reads the acquisition identity persisted with a live
// claim, or "" when the claim predates acquisition identities.
func claimAcquisitionID(c *Claim) string {
	if c == nil {
		return ""
	}
	raw, ok := c.Extra["acquisitionId"]
	if !ok || len(raw) == 0 {
		return ""
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return ""
	}
	return id
}

// claimResultDigest binds the acquisition's public result: everything the
// acquisition response reports except the fencing token, which must stay
// unguessable from the receipt (§8.4).
func claimResultDigest(task *Task, claim *Claim) (string, error) {
	return claimResultDigestValues(task.ID, int64(task.Revision), claim.Owner,
		claimAcquisitionID(claim), claim.AcquiredAt, claim.ExpiresAt)
}

func claimResultDigestValues(task string, revision int64, owner, acquisitionID, acquiredAt, expiresAt string) (string, error) {
	canonical, err := canonicalJSON(map[string]any{
		"task":          task,
		"revision":      revision,
		"owner":         owner,
		"acquisitionId": acquisitionID,
		"acquiredAt":    acquiredAt,
		"expiresAt":     expiresAt,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// lookupClaimReplay resolves the §8.4 replay rule for one acquisition
// request inside the caller's transaction: a stored receipt with a different
// input digest is the typed conflict; a matching one replays the originally
// recorded acquisition and its recorded revision; no receipt returns
// found=false so the caller acquires. Replay appends no event, writes no
// receipt, and advances no fence.
func lookupClaimReplay(t *tx, r claimRequest, inputDigest string, now time.Time) (*Task, int, bool, error) {
	staged, err := t.findVerifiedReceipt(ReceiptKindClaim, r.requestID)
	if err != nil {
		return nil, 0, false, err
	}
	if staged == nil {
		return nil, 0, false, nil
	}
	if err := validateClaimReplayReceipt(staged, r, inputDigest); err != nil {
		return nil, 0, false, err
	}
	task, revision, err := replayClaimAcquisition(t, r.tag, staged, now)
	if err != nil {
		return nil, 0, false, err
	}
	return task, revision, true, nil
}

// claimReceiptCatalog verifies one claim receipt carries exactly the §8.4
// claim acquisition members — no more, no fewer.
func claimReceiptCatalog(receipt map[string]any) error {
	required := map[string]bool{
		"schemaVersion": true, "kind": true, "receiptId": true,
		"familyId": true, "requestId": true, "inputDigest": true,
		"resultDigest": true, "operation": true, "tag": true,
		"task": true, "owner": true, "ttlSeconds": true,
		"acquisitionId": true, "revision": true, "acquiredAt": true,
		"expiresAt": true, "inventory": true, "eventSeq": true,
		"committedAt": true, "receiptDigest": true,
	}
	if len(receipt) != len(required) {
		return receiptIntegrityError(ReceiptKindClaim)
	}
	for key := range receipt {
		if !required[key] {
			return receiptIntegrityError(ReceiptKindClaim)
		}
	}
	return nil
}

// decodeClaimAcquisitionReceipt is the claim-specific semantic validator: it
// proves one stored claim receipt against its own deterministic contract
// without knowing the caller's request. It enforces the exact catalog, the
// receipt id derived from the stored input digest, the acquisition/timestamp/
// TTL/revision grammar, revision >= 1 representable as int, the expiry
// arithmetic, the deterministic result digest, and the inventory and event
// sequence shapes. For a plain claim acquisition it also re-derives the input
// digest from the reconstructed request; a claim-next request's selection
// filters are inputs that live only inside the stored digest, so there the
// digest is checked for grammar and receipt-id binding instead. It returns
// the reconstructed request and the recorded task revision.
func decodeClaimAcquisitionReceipt(receipt map[string]any) (claimRequest, int64, error) {
	var zero claimRequest
	if err := claimReceiptCatalog(receipt); err != nil {
		return zero, 0, err
	}
	operation, _ := receipt["operation"].(string)
	requestID, _ := receipt["requestId"].(string)
	tag, _ := receipt["tag"].(string)
	task, taskOK := receipt["task"].(string)
	owner, ownerOK := receipt["owner"].(string)
	acquisitionID, _ := receipt["acquisitionId"].(string)
	acquiredAt, acquiredOK := receipt["acquiredAt"].(string)
	expiresAt, expiresOK := receipt["expiresAt"].(string)
	committedAt, committedOK := receipt["committedAt"].(string)
	storedInput, inputOK := receipt["inputDigest"].(string)
	ttl, ttlOK := receiptInt64(receipt["ttlSeconds"])
	revision, revisionOK := receiptInt64(receipt["revision"])
	if operation != "claim" && operation != "claim-next" || !ValidLocalKey(requestID) ||
		!taskOK || task == "" || !ownerOK || !validStoredAcquisitionID(acquisitionID) ||
		!acquiredOK || !expiresOK || !committedOK || committedAt != acquiredAt ||
		!inputOK || !ValidDigest(storedInput) ||
		!ttlOK || ttl < 1 || !revisionOK || revision < 1 || int64(int(revision)) != revision {
		return zero, 0, receiptIntegrityError(ReceiptKindClaim)
	}
	r := claimRequest{operation: operation, requestID: requestID, tag: tag, task: task,
		owner: owner, ttl: int(ttl)}
	if receipt["receiptId"] != claimReceiptID(requestID, storedInput) {
		return zero, 0, receiptIntegrityError(ReceiptKindClaim)
	}
	if operation == "claim" {
		recomputed, err := claimInputDigest(r)
		if err != nil || recomputed != storedInput {
			return zero, 0, receiptIntegrityError(ReceiptKindClaim)
		}
	}
	acquiredTime, acquiredErr := time.Parse(time.RFC3339, acquiredAt)
	expiresTime, expiresErr := time.Parse(time.RFC3339, expiresAt)
	if acquiredErr != nil || expiresErr != nil ||
		!expiresTime.Equal(acquiredTime.Add(time.Duration(ttl)*time.Second)) {
		return zero, 0, receiptIntegrityError(ReceiptKindClaim)
	}
	resultDigest, ok := receipt["resultDigest"].(string)
	if !ok {
		return zero, 0, receiptIntegrityError(ReceiptKindClaim)
	}
	wantResult, err := claimResultDigestValues(task, revision, owner, acquisitionID, acquiredAt, expiresAt)
	if err != nil || resultDigest != wantResult {
		return zero, 0, receiptIntegrityError(ReceiptKindClaim)
	}
	inv, ok := receipt["inventory"].(map[string]any)
	if !ok {
		return zero, 0, receiptIntegrityError(ReceiptKindClaim)
	}
	if _, err := parseInventory(inv); err != nil {
		return zero, 0, receiptIntegrityError(ReceiptKindClaim)
	}
	seq, ok := receipt["eventSeq"].(map[string]any)
	if !ok || len(seq) != 2 {
		return zero, 0, receiptIntegrityError(ReceiptKindClaim)
	}
	first, firstOK := receiptInt64(seq["first"])
	last, lastOK := receiptInt64(seq["last"])
	if !firstOK || !lastOK || first <= 0 || first != last {
		return zero, 0, receiptIntegrityError(ReceiptKindClaim)
	}
	return r, revision, nil
}

func validateClaimReplayReceipt(receipt map[string]any, r claimRequest, inputDigest string) error {
	if err := claimReceiptCatalog(receipt); err != nil {
		return err
	}
	storedInput, ok := receipt["inputDigest"].(string)
	if !ok {
		return receiptIntegrityError(ReceiptKindClaim)
	}
	if storedInput != inputDigest {
		return &ReplayConflict{Kind: ReceiptKindClaim, RequestID: r.requestID}
	}
	decoded, _, err := decodeClaimAcquisitionReceipt(receipt)
	if err != nil {
		return err
	}
	if decoded.requestID != r.requestID || decoded.operation != r.operation ||
		decoded.tag != r.tag || decoded.owner != r.owner || int64(decoded.ttl) != int64(r.ttl) ||
		r.operation == "claim" && decoded.task != r.task {
		return receiptIntegrityError(ReceiptKindClaim)
	}
	return nil
}

func receiptInt64(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	n, err := number.Int64()
	return n, err == nil
}

// replayClaimAcquisition returns the acquisition recorded for one request
// and the revision it was recorded with: the task's live claim whose owner
// and acquisition identity match the receipt exactly. The fencing token and
// the acquisition identity never correlate, so replay binds to (task, owner,
// acquisitionId) — not to second-precision timestamps — and returns the live
// token: the original, or its renewal successor, which preserves both owner
// and acquisition identity. A released, expired, or superseded acquisition —
// including a same-owner same-second reacquisition — is a typed conflict
// that discloses no token and no holder identity.
func replayClaimAcquisition(t *tx, tag string, receipt map[string]any, now time.Time) (*Task, int, error) {
	taskID, _ := receipt["task"].(string)
	conflict := &ClaimConflict{Tag: tag, ID: taskID, Kind: "replay"}
	recorded, ok := receipt["revision"].(json.Number)
	if !ok {
		return nil, 0, conflict
	}
	revision64, err := recorded.Int64()
	if err != nil || revision64 < 1 || int64(int(revision64)) != revision64 {
		return nil, 0, conflict
	}
	revision := int(revision64)
	owner, ownerOK := receipt["owner"].(string)
	acquisitionID, acquisitionIDOK := receipt["acquisitionId"].(string)
	acquiredAt, acquiredAtOK := receipt["acquiredAt"].(string)
	if !ownerOK || !acquisitionIDOK || !validAcquisitionID(acquisitionID) || !acquiredAtOK {
		return nil, 0, conflict
	}
	task, err := t.getTask(tag, taskID)
	if err != nil {
		return nil, 0, err
	}
	if task == nil {
		return nil, 0, conflict
	}
	expireClaim(task, now)
	if task.Claim == nil || task.Claim.Owner != owner ||
		!validAcquisitionID(claimAcquisitionID(task.Claim)) || claimAcquisitionID(task.Claim) != acquisitionID ||
		task.Claim.AcquiredAt != acquiredAt {
		return nil, 0, conflict
	}
	return task, revision, nil
}

func validAcquisitionID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// writeClaimAcquisition performs the fenced acquisition, its claim event,
// and its immutable §8.4 claim receipt inside the caller's immediate
// transaction: the conditional UPDATE never overwrites an unexpired claim,
// the claim persists a fresh unique acquisition identity, and the receipt
// records the acquisition identity and the task revision read in this
// transaction — never the fencing token. The event is kind=claim,
// action=acquired, and carries the receipt id and receipt digest; the
// receipt's eventSeq predicts the event's sequence the same way plan apply
// does, so the digest can cover the receipt before the event cites it
// without circularity. now is the single clock reading the transaction's
// expiry judgments share.
func writeClaimAcquisition(t *tx, r claimRequest, task *Task, now time.Time) (*Claim, error) {
	acquisitionID, err := newAcquisitionID()
	if err != nil {
		return nil, err
	}
	token, err := t.nextFence()
	if err != nil {
		return nil, err
	}
	claim := &Claim{
		Owner:      r.owner,
		Token:      token,
		TTL:        r.ttl,
		AcquiredAt: now.Format(time.RFC3339),
		ExpiresAt:  now.Add(time.Duration(r.ttl) * time.Second).Format(time.RFC3339),
		Extra:      acquisitionIDExtra(acquisitionID),
	}
	// Conditional acquisition: refuse to overwrite an unexpired claim even
	// if the row changed between the read and this update. With the write
	// lock held the condition cannot fail; it stays as defense in depth.
	res, err := t.Exec(`UPDATE tasks SET claim_owner=?, claim_token=?, claim_ttl=?, claim_acquired_at=?, claim_expires_at=?, claim_extra=?
		WHERE tag=? AND id=? AND (claim_owner IS NULL OR claim_expires_at <= ?)`,
		r.owner, token, r.ttl, claim.AcquiredAt, claim.ExpiresAt, encodeExtra(claim.Extra),
		r.tag, task.ID, now.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("acquire claim on task %s: %w", task.ID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		// Unreachable with the write lock held; fail loudly rather than
		// commit a receipt for a claim this transaction did not acquire.
		current, err := t.getTask(r.tag, task.ID)
		if err != nil {
			return nil, err
		}
		if current == nil {
			return nil, &TaskNotFound{Tag: r.tag, ID: task.ID}
		}
		conflict := &ClaimConflict{Tag: r.tag, ID: task.ID, Kind: "claim"}
		if current.Claim != nil {
			conflict.Owner = current.Claim.Owner
			conflict.Expiry = current.Claim.ExpiresAt
			conflict.FencingToken = current.Claim.Token
		}
		return nil, conflict
	}
	task.Claim = claim

	inputDigest, err := claimInputDigest(r)
	if err != nil {
		return nil, err
	}
	receiptID := claimReceiptID(r.requestID, inputDigest)
	familyID, _, err := t.requireIdentity()
	if err != nil {
		return nil, err
	}
	inv, err := countClassInventory(t)
	if err != nil {
		return nil, err
	}
	// The receipt's inventory covers its own committed effect: the claim
	// event about to be appended and this receipt itself.
	if inv.Events < MaxRetainedEvents {
		inv.Events++
	}
	inv.Receipts++
	resultDigest, err := claimResultDigest(task, claim)
	if err != nil {
		return nil, err
	}
	receipt := map[string]any{
		"schemaVersion": int64(ReceiptSchemaVersion),
		"kind":          ReceiptKindClaim,
		"receiptId":     receiptID,
		"familyId":      familyID,
		"requestId":     r.requestID,
		"inputDigest":   inputDigest,
		"resultDigest":  resultDigest,
		"operation":     r.operation,
		"tag":           r.tag,
		"task":          task.ID,
		"owner":         r.owner,
		"ttlSeconds":    int64(r.ttl),
		"acquisitionId": acquisitionID,
		"revision":      int64(task.Revision),
		"acquiredAt":    claim.AcquiredAt,
		"expiresAt":     claim.ExpiresAt,
		"inventory":     inventoryRecord(inv),
		"eventSeq":      nil, // predicted below, before the receipt is complete
		"committedAt":   claim.AcquiredAt,
	}
	var seq int64
	if err := t.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='events'`).Scan(&seq); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read claim event sequence: %w", err)
	}
	seq++
	receipt["eventSeq"] = map[string]any{"first": seq, "last": seq}
	digest, err := receiptDigest(receipt)
	if err != nil {
		return nil, err
	}
	if err := t.appendEvent(Event{
		Task: task.ID, Tag: r.tag, Kind: "claim", Action: "acquired", Owner: r.owner,
		Revision: task.Revision, At: claim.AcquiredAt,
		Extra: map[string]json.RawMessage{
			"receiptId":     json.RawMessage(strconv.Quote(receiptID)),
			"receiptDigest": json.RawMessage(strconv.Quote(digest)),
		},
	}); err != nil {
		return nil, err
	}
	var actualSeq int64
	if err := t.QueryRow(`SELECT last_insert_rowid()`).Scan(&actualSeq); err != nil {
		return nil, fmt.Errorf("read claim event sequence: %w", err)
	}
	if actualSeq != seq {
		return nil, fmt.Errorf("claim event sequence changed from %d to %d", seq, actualSeq)
	}
	if _, err := t.insertReceipt(ReceiptKindClaim, receiptID, r.requestID, receipt, claim.AcquiredAt); err != nil {
		return nil, err
	}
	return claim, nil
}
