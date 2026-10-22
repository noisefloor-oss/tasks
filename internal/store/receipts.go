package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// §8.4 receipts: immutable canonical-JSON proof records retained
// independently of feed compaction. The digest domain is fixed for version 1.
const receiptDigestDomain = "noise-tasks-receipt-v1\x00"

// ReceiptSchemaVersion is the receipt document version this binary writes.
const ReceiptSchemaVersion = 1

// Receipt kinds this binary writes (SPEC §8.4 kind vocabulary).
const ReceiptKindRestore = "restore"

// ReplayConflict reports a (kind, requestId) reuse with different normalized
// inputs (SPEC §8.4, exit 3).
type ReplayConflict struct {
	Kind      string
	RequestID string
}

// ReceiptIntegrityError reports an altered or malformed stored receipt. Its
// deliberately terse message never echoes receipt content or capability-adjacent
// acquisition metadata.
type ReceiptIntegrityError struct {
	Kind string
}

func (e *ReceiptIntegrityError) Error() string {
	return fmt.Sprintf("stored %s receipt failed integrity validation", e.Kind)
}

func receiptIntegrityError(kind string) error {
	return &ReceiptIntegrityError{Kind: kind}
}

func (e *ReplayConflict) Error() string {
	return fmt.Sprintf("request id %q was already used for a different %s request", e.RequestID, e.Kind)
}

// localKeyGrammar is the §9.2 local-key grammar every operation request id
// uses (SPEC §9.1): one leading alphanumeric, then at most 127 further
// alphanumeric, dot, underscore, or hyphen bytes.
const localKeyGrammar = "[A-Za-z0-9][A-Za-z0-9._-]{0,127}"

// ValidLocalKey reports whether s is a well-formed local key / request id.
func ValidLocalKey(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
		if i == 0 {
			if !alnum {
				return false
			}
			continue
		}
		if !alnum && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return len(s) <= 128
}

// receiptID derives the deterministic restore receipt id for one request and
// bundle. Re-running the same request against the same bundle names the same
// receipt, which is what makes replay return the original record.
func restoreReceiptID(requestID, payloadDigest string) string {
	sum := sha256.Sum256([]byte("noise-tasks-restore-v1\x00" + requestID + "\x00" + payloadDigest))
	return "restore-" + hex.EncodeToString(sum[:8])
}

// receiptDigest computes §8.4's receiptDigest: SHA-256 over the domain string
// and the canonical receipt excluding the receiptDigest field itself.
func receiptDigest(receipt map[string]any) (string, error) {
	without := map[string]any{}
	for k, v := range receipt {
		if k != "receiptDigest" {
			without[k] = v
		}
	}
	canonical, err := canonicalJSON(without)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(receiptDigestDomain), canonical...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// insertReceipt persists one immutable receipt inside the effect's own
// transaction (SPEC §8.4: no separate replay table; the receipt is written in
// the same transaction as the effect).
func (t *tx) insertReceipt(kind, receiptID, requestID string, receipt map[string]any, at string) (string, error) {
	digest, err := receiptDigest(receipt)
	if err != nil {
		return "", err
	}
	receipt["receiptDigest"] = digest
	body, err := canonicalJSON(receipt)
	if err != nil {
		return "", err
	}
	if _, err := t.Exec(`INSERT INTO receipts(kind, receipt_id, request_id, body, digest, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		kind, receiptID, nullString(requestID), string(body), digest, at); err != nil {
		return "", fmt.Errorf("record %s receipt: %w", kind, err)
	}
	return digest, nil
}

// findReceipt returns the stored receipt body for (kind, requestId), or nil
// when no request with that id has been recorded. Inspection and legacy replay
// callers use this decoder; capability-returning replay uses the verified
// loader below.
func (t *tx) findReceipt(kind, requestID string) (map[string]any, error) {
	var body string
	err := t.QueryRow(`SELECT body FROM receipts WHERE kind=? AND request_id=?`, kind, requestID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s receipts: %w", kind, err)
	}
	decoded, err := decodeJSONObject([]byte(body))
	if err != nil {
		return nil, fmt.Errorf("decode %s receipt: %w", kind, err)
	}
	return decoded, nil
}

// findVerifiedReceipt verifies the common stored-row envelope before a replay
// may return a reusable capability (SPEC §8.3/§8.4).
func (t *tx) findVerifiedReceipt(kind, requestID string) (map[string]any, error) {
	var receiptID, body, rowDigest, createdAt string
	err := t.QueryRow(`SELECT receipt_id, body, digest, created_at FROM receipts WHERE kind=? AND request_id=?`, kind, requestID).
		Scan(&receiptID, &body, &rowDigest, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s receipts: %w", kind, err)
	}
	decoded, err := decodeJSONObject([]byte(body))
	if err != nil {
		return nil, receiptIntegrityError(kind)
	}
	canonical, err := canonicalJSON(decoded)
	if err != nil || string(canonical) != body {
		return nil, receiptIntegrityError(kind)
	}
	computedDigest, err := receiptDigest(decoded)
	if err != nil {
		return nil, receiptIntegrityError(kind)
	}
	embeddedDigest, embeddedOK := decoded["receiptDigest"].(string)
	schema, schemaOK := decoded["schemaVersion"].(json.Number)
	familyID, _, err := t.requireIdentity()
	if err != nil {
		return nil, err
	}
	if !embeddedOK || rowDigest != computedDigest || embeddedDigest != computedDigest ||
		!schemaOK || schema.String() != strconv.Itoa(ReceiptSchemaVersion) ||
		decoded["kind"] != kind || decoded["receiptId"] != receiptID ||
		decoded["familyId"] != familyID || decoded["requestId"] != requestID ||
		decoded["committedAt"] != createdAt {
		return nil, receiptIntegrityError(kind)
	}
	return decoded, nil
}

// FindReceipt is the read path for receipt inspection surfaces.
func (d *DB) FindReceipt(kind, requestID string) (map[string]any, error) {
	var receipt map[string]any
	err := d.deferred(func(t *tx) error {
		var err error
		receipt, err = t.findReceipt(kind, requestID)
		return err
	})
	return receipt, err
}
