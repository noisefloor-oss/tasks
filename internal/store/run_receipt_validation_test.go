package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type runReceiptCorruption struct {
	name   string
	mutate func(*sql.DB) error
}

var runReceiptCorruptions = []runReceiptCorruption{
	{
		name: "tampered start receipt",
		mutate: func(db *sql.DB) error {
			_, err := db.Exec(`UPDATE receipts SET body=body||' ' WHERE kind='run' AND request_id='run.history'`)
			return err
		},
	},
	{
		name: "missing start receipt",
		mutate: func(db *sql.DB) error {
			_, err := db.Exec(`DELETE FROM receipts WHERE kind='run' AND request_id='run.history'`)
			return err
		},
	},
	{
		name: "missing retry receipt",
		mutate: func(db *sql.DB) error {
			_, err := db.Exec(`DELETE FROM receipts WHERE kind='run' AND request_id='retry.history'`)
			return err
		},
	},
}

func TestValidateStoreRejectsRunReceiptCorruption(t *testing.T) {
	for _, tc := range runReceiptCorruptions {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := completedRunHistoryFixture(t)
			if err := db.ValidateStore(); err != nil {
				t.Fatalf("baseline validation: %v", err)
			}
			if err := tc.mutate(db.db); err != nil {
				t.Fatal(err)
			}
			if err := db.ValidateStore(); err == nil {
				t.Fatal("ValidateStore accepted corrupt run receipts")
			}
		})
	}
}

func TestCreateBackupRejectsRunReceiptCorruption(t *testing.T) {
	for _, tc := range runReceiptCorruptions {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := completedRunHistoryFixture(t)
			if err := tc.mutate(db.db); err != nil {
				t.Fatal(err)
			}
			bundle := filepath.Join(t.TempDir(), "corrupt.ntbackup")
			if _, err := db.CreateBackup(bundle, "noise-tasks test"); err == nil {
				t.Fatal("CreateBackup accepted corrupt run receipts")
			}
		})
	}
}

func TestVerifyBundleAndRestoreRejectRunReceiptCorruption(t *testing.T) {
	for _, tc := range runReceiptCorruptions {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := completedRunHistoryFixture(t)
			_, ownerBoundary, err := db.Identity()
			if err != nil {
				t.Fatal(err)
			}
			bundle := filepath.Join(t.TempDir(), "corrupt.ntbackup")
			if _, err := db.CreateBackup(bundle, "noise-tasks test"); err != nil {
				t.Fatal(err)
			}
			tamperPayload(t, bundle, tc.mutate)
			if _, err := VerifyBundle(bundle); err == nil || !strings.Contains(err.Error(), "payload run receipts are invalid") {
				t.Fatalf("VerifyBundle error = %v", err)
			}
			target := filepath.Join(t.TempDir(), "noise-tasks", StoreFile)
			if _, err := Restore(bundle, target, RestoreOptions{RequestID: "restore.corrupt", OwnerBoundary: ownerBoundary}); err == nil {
				t.Fatal("Restore accepted corrupt run receipts")
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("corrupt restore published a target: %v", err)
			}
		})
	}
}

func TestRunReceiptValidationAllowsCompactedHistory(t *testing.T) {
	db, runID := completedRunHistoryFixture(t)
	if _, err := db.db.Exec(`DELETE FROM events WHERE seq < (SELECT MAX(seq) FROM events)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetRun(runID); err != nil {
		t.Fatalf("GetRun with compacted events: %v", err)
	}
	if err := db.ValidateStore(); err != nil {
		t.Fatalf("ValidateStore with compacted events: %v", err)
	}
	_, ownerBoundary, err := db.Identity()
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "compacted.ntbackup")
	if _, err := db.CreateBackup(bundle, "noise-tasks test"); err != nil {
		t.Fatalf("CreateBackup with compacted events: %v", err)
	}
	if _, err := VerifyBundle(bundle); err != nil {
		t.Fatalf("VerifyBundle with compacted events: %v", err)
	}
	target := filepath.Join(t.TempDir(), "noise-tasks", StoreFile)
	if _, err := Restore(bundle, target, RestoreOptions{RequestID: "restore.compacted", OwnerBoundary: ownerBoundary}); err != nil {
		t.Fatalf("Restore with compacted events: %v", err)
	}
	restored, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if _, err := restored.GetRun(runID); err != nil {
		t.Fatalf("restored GetRun with compacted events: %v", err)
	}
	if err := restored.ValidateStore(); err != nil {
		t.Fatalf("restored ValidateStore with compacted events: %v", err)
	}
}
