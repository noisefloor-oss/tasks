package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

type restoreReceiptTamper struct {
	name   string
	mutate func(map[string]any)
	rowSQL string
	rowArg any
}

var restoreReceiptTampers = []restoreReceiptTamper{
	{name: "noncanonical body", rowSQL: `UPDATE receipts SET body=body||' ' WHERE kind=? AND request_id=?`},
	{name: "row digest", rowSQL: `UPDATE receipts SET digest=? WHERE kind=? AND request_id=?`, rowArg: "sha256:" + strings.Repeat("0", 64)},
	{name: "row receipt id", rowSQL: `UPDATE receipts SET receipt_id=? WHERE kind=? AND request_id=?`, rowArg: "restore-altered"},
	{name: "row commit time", rowSQL: `UPDATE receipts SET created_at=? WHERE kind=? AND request_id=?`, rowArg: "2027-01-01T00:00:00Z"},
	{name: "embedded receipt digest", mutate: func(r map[string]any) { r["receiptDigest"] = "sha256:" + strings.Repeat("1", 64) }},
	{name: "schema", mutate: func(r map[string]any) { r["schemaVersion"] = int64(2) }},
	{name: "kind", mutate: func(r map[string]any) { r["kind"] = "migration" }},
	{name: "embedded receipt id", mutate: func(r map[string]any) { r["receiptId"] = "restore-altered" }},
	{name: "family", mutate: func(r map[string]any) { r["familyId"] = "00000000-0000-4000-8000-000000000000" }},
	{name: "embedded request id", mutate: func(r map[string]any) { r["requestId"] = "other-request" }},
	{name: "embedded commit time", mutate: func(r map[string]any) { r["committedAt"] = "2027-01-01T00:00:00Z" }},
}

func tamperRestoreReceipt(t *testing.T, db *DB, requestID string, tamper restoreReceiptTamper) {
	t.Helper()
	if tamper.mutate != nil {
		receipt, err := db.FindReceipt(ReceiptKindRestore, requestID)
		if err != nil || receipt == nil {
			t.Fatalf("read restore receipt: %v / %v", receipt, err)
		}
		tamper.mutate(receipt)
		if tamper.name != "embedded receipt digest" {
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
		if tamper.name == "embedded receipt digest" {
			rowDigest, _ = receipt["receiptDigest"].(string)
		}
		if _, err := db.db.Exec(`UPDATE receipts SET body=?, digest=? WHERE kind=? AND request_id=?`,
			string(body), rowDigest, ReceiptKindRestore, requestID); err != nil {
			t.Fatal(err)
		}
		return
	}
	if tamper.rowArg == nil {
		if _, err := db.db.Exec(tamper.rowSQL, ReceiptKindRestore, requestID); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := db.db.Exec(tamper.rowSQL, tamper.rowArg, ReceiptKindRestore, requestID); err != nil {
		t.Fatal(err)
	}
}

func requireRestoreReceiptIntegrityError(t *testing.T, err error) {
	t.Helper()
	var integrity *ReceiptIntegrityError
	if !errors.As(err, &integrity) || integrity.Kind != ReceiptKindRestore {
		t.Fatalf("tampered restore replay error = %T %v, want restore ReceiptIntegrityError", err, err)
	}
}

func TestRestoreReplayVerifiesStoredReceipt(t *testing.T) {
	for _, tamper := range restoreReceiptTampers {
		t.Run(tamper.name, func(t *testing.T) {
			dir := t.TempDir()
			_, bundle := restoreFixture(t, dir)
			target := filepath.Join(dir, "restored", "noise-tasks", "tasks.sqlite")
			opts := RestoreOptions{RequestID: "restore-tamper", OwnerBoundary: dir}
			if _, err := Restore(bundle, target, opts); err != nil {
				t.Fatal(err)
			}
			db, err := Open(target)
			if err != nil {
				t.Fatal(err)
			}
			tamperRestoreReceipt(t, db, opts.RequestID, tamper)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			result, err := Restore(bundle, target, opts)
			if result != nil {
				t.Fatalf("tampered restore replay returned %+v", result)
			}
			requireRestoreReceiptIntegrityError(t, err)
		})
	}
}

func TestRestoreStagedPayloadVerifiesStoredReceipt(t *testing.T) {
	for _, tamper := range restoreReceiptTampers {
		t.Run(tamper.name, func(t *testing.T) {
			dir := t.TempDir()
			db := newBackupStore(t, filepath.Join(dir, "src"), dir)
			defer db.Close()
			familyID, _, err := db.Identity()
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := db.CreateBackup(filepath.Join(dir, "source.ntbackup"), "test")
			if err != nil {
				t.Fatal(err)
			}
			const requestID = "staged-tamper"
			const committedAt = "2026-01-01T00:00:00Z"
			inputDigest, err := restoreInputDigest(requestID, manifest)
			if err != nil {
				t.Fatal(err)
			}
			receiptID := restoreReceiptID(requestID, manifest.PayloadDigest)
			if err := db.immediate(func(t *tx) error {
				_, err := t.insertReceipt(ReceiptKindRestore, receiptID, requestID, map[string]any{
					"schemaVersion": int64(ReceiptSchemaVersion),
					"kind":          ReceiptKindRestore,
					"receiptId":     receiptID,
					"familyId":      familyID,
					"requestId":     requestID,
					"inputDigest":   inputDigest,
					"committedAt":   committedAt,
				}, committedAt)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			tamperRestoreReceipt(t, db, requestID, tamper)

			err = db.immediate(func(t *tx) error {
				_, _, err := writeRestoreReceipt(t, manifest, requestID, familyID, inputDigest, receiptID, "2026-01-02T00:00:00Z")
				return err
			})
			requireRestoreReceiptIntegrityError(t, err)
		})
	}
}
