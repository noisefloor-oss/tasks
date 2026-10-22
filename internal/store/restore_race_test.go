package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// restoreHookMu serializes tests that mutate restoreFaultHook: the hook is a
// package-level injection point and production keeps it a direct no-op.
var restoreHookMu sync.Mutex

// failRestoreAt installs a deterministic fault at one publication op.
func failRestoreAt(t *testing.T, op string) {
	t.Helper()
	restoreHookMu.Lock()
	t.Cleanup(func() {
		restoreFaultHook = func(string) error { return nil }
		restoreHookMu.Unlock()
	})
	restoreFaultHook = func(fired string) error {
		if fired == op {
			return fmt.Errorf("injected %s failure", fired)
		}
		return nil
	}
}

// actOnRestoreOp runs fn exactly when the publication path reaches op.
func actOnRestoreOp(t *testing.T, op string, fn func()) {
	t.Helper()
	restoreHookMu.Lock()
	t.Cleanup(func() {
		restoreFaultHook = func(string) error { return nil }
		restoreHookMu.Unlock()
	})
	restoreFaultHook = func(fired string) error {
		if fired == op {
			fn()
		}
		return nil
	}
}

// restoreFixture builds a populated store and a verified bundle of it.
func restoreFixture(t *testing.T, dir string) (target, bundle string) {
	t.Helper()
	db := newBackupStore(t, filepath.Join(dir, "src"), dir)
	target = db.Path()
	bundle = filepath.Join(dir, "out.ntbackup")
	if _, err := db.CreateBackup(bundle, "test"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	return target, bundle
}

// sidecarCheck asserts no restore temporaries or stale sidecars remain in
// the target's directory.
func sidecarCheck(t *testing.T, target string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == filepath.Base(target) || name == LegacyFile || name == MigrationJournalFile ||
			name == "maintenance.lock" {
			continue
		}
		t.Fatalf("unexpected sidecar %s remains beside %s", name, target)
	}
}

func fileSum(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRestoreReplayBeforeOverwriteRequirement proves the §8.4/§8.5 replay
// rule is consulted before --overwrite is required: re-running the normal
// absent-target restore returns the original receipt without the flag —
// including after a post-rename directory-fsync failure — while a different
// normalized input is a typed ReplayConflict.
func TestRestoreReplayBeforeOverwriteRequirement(t *testing.T) {
	dir := t.TempDir()
	target, bundle := restoreFixture(t, dir)
	_ = target
	absentDir := filepath.Join(dir, "recovery")
	fresh := filepath.Join(absentDir, "noise-tasks", "tasks.sqlite")

	first, err := Restore(bundle, fresh, RestoreOptions{RequestID: "rr-1", OwnerBoundary: dir})
	if err != nil {
		t.Fatalf("absent-target restore: %v", err)
	}
	if !first.Adopted || first.Replayed {
		t.Fatalf("unexpected flags %+v", first)
	}

	// The normal rerun without --overwrite returns the original receipt.
	again, err := Restore(bundle, fresh, RestoreOptions{RequestID: "rr-1", OwnerBoundary: dir})
	if err != nil {
		t.Fatalf("rerun without --overwrite: %v", err)
	}
	if !again.Replayed || again.Receipt["receiptId"] != first.Receipt["receiptId"] ||
		again.Receipt["committedAt"] != first.Receipt["committedAt"] {
		t.Fatalf("expected replay of the original receipt, got %+v", again)
	}
	sidecarCheck(t, fresh)

	// A different bundle with the same request id is a typed conflict even
	// without --overwrite: identity is validated, replay is consulted, and
	// only then would overwrite be demanded.
	db, err := Open(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddTask("main", &Task{Title: "extra"}); err != nil {
		t.Fatal(err)
	}
	bundle2 := filepath.Join(dir, "second.ntbackup")
	if _, err := db.CreateBackup(bundle2, "test"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	var conflict *ReplayConflict
	if _, err := Restore(bundle2, fresh, RestoreOptions{RequestID: "rr-1", OwnerBoundary: dir}); err == nil ||
		!errors.As(err, &conflict) {
		t.Fatalf("expected typed replay conflict, got %v", err)
	}
}

// TestRestoreReplayAfterDirFsyncFailure proves that a restore whose
// directory fsync fails after the rename still published a complete
// receipt-bearing target, and that re-running the identical original command
// (no --overwrite) replays instead of demanding overwrite.
func TestRestoreReplayAfterDirFsyncFailure(t *testing.T) {
	dir := t.TempDir()
	_, bundle := restoreFixture(t, dir)
	target := filepath.Join(dir, "recovery", "noise-tasks", "tasks.sqlite")
	opts := RestoreOptions{RequestID: "fsd-1", OwnerBoundary: dir}

	failRestoreAt(t, "fsync-dir")
	if _, err := Restore(bundle, target, opts); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("expected injected dir-fsync failure, got %v", err)
	}

	// The target is complete and carries its receipt.
	db, err := Open(target)
	if err != nil {
		t.Fatalf("open post-rename target: %v", err)
	}
	receipt, err := db.FindReceipt(ReceiptKindRestore, "fsd-1")
	if err != nil || receipt == nil {
		t.Fatalf("expected embedded receipt, got %v / %v", receipt, err)
	}
	db.Close()
	sidecarCheck(t, target)

	// The identical original command replays without --overwrite.
	result, err := Restore(bundle, target, opts)
	if err != nil {
		t.Fatalf("rerun after dir-fsync failure: %v", err)
	}
	if !result.Replayed || result.Receipt["receiptId"] != receipt["receiptId"] {
		t.Fatalf("expected replay, got %+v", result)
	}
	sidecarCheck(t, target)
}

// TestRestorePreRenameFailuresPreserveAuthority walks every injectable
// failure point before the publishing rename and proves the old target (or
// absence) stays intact with no published receipt and no sidecars.
func TestRestorePreRenameFailuresPreserveAuthority(t *testing.T) {
	preRenameOps := []string{"chmod", "fsync-file", "revalidate", "rename", "checkpoint-staged"}

	for _, op := range preRenameOps {
		t.Run("absent/"+op, func(t *testing.T) {
			dir := t.TempDir()
			_, bundle := restoreFixture(t, dir)
			target := filepath.Join(dir, "recovery", "noise-tasks", "tasks.sqlite")
			failRestoreAt(t, op)
			if _, err := Restore(bundle, target, RestoreOptions{RequestID: "pf-1", OwnerBoundary: dir}); err == nil {
				t.Fatalf("expected injected %s failure, got success", op)
			}
			if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("target must stay absent after pre-rename failure, got %v", err)
			}
			sidecarCheck(t, target)
		})
	}

	for _, op := range append([]string{"checkpoint"}, preRenameOps...) {
		t.Run("existing/"+op, func(t *testing.T) {
			dir := t.TempDir()
			target, bundle := restoreFixture(t, dir)
			before := fileSum(t, target)
			failRestoreAt(t, op)
			if _, err := Restore(bundle, target, RestoreOptions{Overwrite: true, RequestID: "pf-2", OwnerBoundary: dir}); err == nil {
				t.Fatalf("expected injected %s failure, got success", op)
			}
			if got := fileSum(t, target); string(got) != string(before) {
				t.Fatal("old target must be byte-identical after a pre-rename failure")
			}
			db, err := Open(target)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := db.FindReceipt(ReceiptKindRestore, "pf-2")
			db.Close()
			if err != nil || receipt != nil {
				t.Fatalf("no receipt may be published before rename, got %v / %v", receipt, err)
			}
			sidecarCheck(t, target)
		})
	}
}

// TestRestorePostRenameFailureLeavesCompleteTarget proves a failure after
// the rename leaves a complete receipt-bearing target, then the identical
// rerun replays without --overwrite and no sidecars remain.
func TestRestorePostRenameFailureLeavesCompleteTarget(t *testing.T) {
	dir := t.TempDir()
	_, bundle := restoreFixture(t, dir)
	target := filepath.Join(dir, "recovery", "noise-tasks", "tasks.sqlite")
	opts := RestoreOptions{RequestID: "pr-1", OwnerBoundary: dir}

	failRestoreAt(t, "fsync-dir")
	if _, err := Restore(bundle, target, opts); err == nil {
		t.Fatal("expected injected dir-fsync failure")
	}
	db, err := Open(target)
	if err != nil {
		t.Fatalf("published target must open: %v", err)
	}
	if _, err := db.ListTasks("main"); err != nil {
		t.Fatalf("published target must be complete: %v", err)
	}
	receipt, err := db.FindReceipt(ReceiptKindRestore, "pr-1")
	db.Close()
	if err != nil || receipt == nil {
		t.Fatalf("published target must carry its receipt, got %v / %v", receipt, err)
	}
	sidecarCheck(t, target)

	result, err := Restore(bundle, target, opts)
	if err != nil {
		t.Fatalf("identical rerun must replay: %v", err)
	}
	if !result.Replayed || result.Receipt["receiptId"] != receipt["receiptId"] {
		t.Fatalf("expected replay, got %+v", result)
	}
	sidecarCheck(t, target)
}

// TestRestoreStagedPayloadReceiptRule proves the staged payload's own
// receipt table participates in the store-wide (kind, requestId) rule: an
// identical staged receipt returns that record without a duplicate effect
// (unit level — a verified bundle can never carry one, since the payload
// digest covers the receipt bytes), while a different input restores as a
// typed ReplayConflict, never a unique-index error.
func TestRestoreStagedPayloadReceiptRule(t *testing.T) {
	dir := t.TempDir()
	db := newBackupStore(t, filepath.Join(dir, "src"), dir)
	familyID, _, _ := db.Identity()

	manifest, err := db.CreateBackup(filepath.Join(dir, "probe.ntbackup"), "test")
	if err != nil {
		t.Fatal(err)
	}
	inputDigest, err := restoreInputDigest("sp-1", manifest)
	if err != nil {
		t.Fatal(err)
	}
	rid := restoreReceiptID("sp-1", manifest.PayloadDigest)
	seed := func(input string) error {
		return db.immediate(func(t *tx) error {
			_, err := t.insertReceipt(ReceiptKindRestore, rid, "sp-1", map[string]any{
				"schemaVersion": int64(ReceiptSchemaVersion),
				"kind":          ReceiptKindRestore,
				"receiptId":     rid,
				"requestId":     "sp-1",
				"inputDigest":   input,
				"committedAt":   "2026-01-01T00:00:00Z",
			}, "2026-01-01T00:00:00Z")
			return err
		})
	}

	// Identical staged receipt: no duplicate event or receipt row.
	if err := seed(inputDigest); err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	var stagedReplay bool
	err = db.immediate(func(t *tx) error {
		receipt, stagedReplay, err = writeRestoreReceipt(t, manifest, "sp-1", familyID, inputDigest, rid, "2026-01-02T00:00:00Z")
		return err
	})
	if err != nil {
		t.Fatalf("identical staged receipt: %v", err)
	}
	if !stagedReplay || receipt["receiptId"] != rid {
		t.Fatalf("expected staged replay of the original receipt, got %+v", receipt)
	}
	var receipts, events int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM receipts WHERE kind='restore' AND request_id='sp-1'`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind='restore'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 || events != 0 {
		t.Fatalf("identical staged receipt must not duplicate the effect, got %d receipts / %d events", receipts, events)
	}

	// Different staged input digest: typed conflict, no unique-index error.
	if err := db.immediate(func(t *tx) error {
		_, err := t.Exec(`DELETE FROM receipts WHERE kind='restore'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := seed("sha256:different"); err != nil {
		t.Fatal(err)
	}
	var conflict *ReplayConflict
	err = db.immediate(func(t *tx) error {
		_, _, err = writeRestoreReceipt(t, manifest, "sp-1", familyID, inputDigest, rid, "2026-01-02T00:00:00Z")
		return err
	})
	if err == nil || !errors.As(err, &conflict) {
		t.Fatalf("expected typed replay conflict, got %v", err)
	}

	// End to end: a real bundle whose payload carries a restore receipt for
	// this request id (captured from a restored store) cannot repeat the
	// effect under the same request id — the staged digest no longer
	// matches the new bundle, so restore fails typed, and a different
	// request id restores normally without a unique-index error.
	db.Close()
	fixture := filepath.Join(dir, "chain")
	target, bundle := restoreFixture(t, fixture)
	first, err := Restore(bundle, target, RestoreOptions{Overwrite: true, RequestID: "sp-9", OwnerBoundary: fixture})
	if err != nil {
		t.Fatalf("initial restore: %v", err)
	}
	chain, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	bundle2 := filepath.Join(dir, "chain2.ntbackup")
	if _, err := chain.CreateBackup(bundle2, "test"); err != nil {
		t.Fatal(err)
	}
	chain.Close()
	var e2eConflict *ReplayConflict
	if _, err := Restore(bundle2, filepath.Join(dir, "other", "noise-tasks", "tasks.sqlite"),
		RestoreOptions{RequestID: "sp-9", OwnerBoundary: fixture}); err == nil || !errors.As(err, &e2eConflict) {
		t.Fatalf("expected typed end-to-end replay conflict, got %v", err)
	}
	fresh, err := Restore(bundle2, filepath.Join(dir, "other", "noise-tasks", "tasks.sqlite"),
		RestoreOptions{RequestID: "sp-10", OwnerBoundary: fixture})
	if err != nil {
		t.Fatalf("different request id must restore normally: %v", err)
	}
	if fresh.Replayed || fresh.Receipt["receiptId"] == first.Receipt["receiptId"] {
		t.Fatalf("expected a fresh receipt, got %+v", fresh)
	}
}

// TestRestoreReceiptInventoryAtRetentionBoundary proves the receipt
// inventory reflects actual post-event retention at the 10,000-event
// boundary and the receipt count computed from transaction state.
func TestRestoreReceiptInventoryAtRetentionBoundary(t *testing.T) {
	dir := t.TempDir()
	db := newBackupStore(t, filepath.Join(dir, "src"), dir)
	target := db.Path()

	// Top the feed up to exactly MaxRetainedEvents retained rows.
	err := db.immediate(func(t *tx) error {
		var count int64
		if err := t.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&count); err != nil {
			return err
		}
		for i := int64(0); i < MaxRetainedEvents-count; i++ {
			if err := t.appendEvent(Event{Kind: "task", Action: "updated", At: "2026-01-01T00:00:00Z"}); err != nil {
				return err
			}
		}
		return t.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&count)
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "out.ntbackup")
	if _, err := db.CreateBackup(bundle, "test"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	result, err := Restore(bundle, target, RestoreOptions{Overwrite: true, RequestID: "b-1", OwnerBoundary: dir})
	if err != nil {
		t.Fatalf("boundary restore: %v", err)
	}
	inv, _ := result.Receipt["inventory"].(map[string]any)
	if inv == nil {
		t.Fatalf("receipt has no inventory: %+v", result.Receipt)
	}
	if got := asInt64(inv["events"]); got != MaxRetainedEvents {
		t.Fatalf("inventory events must reflect post-compaction retention %d, got %v", MaxRetainedEvents, inv["events"])
	}
	if got := asInt64(inv["receipts"]); got != 2 {
		t.Fatalf("inventory receipts must be the computed new count (the seeded §8.4 claim receipt plus this restore receipt), got %v", inv["receipts"])
	}
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return -1
}

// TestRestoreOverwriteRevalidationFailsClosed proves an overwrite
// publication revalidates the target identity immediately before the rename
// and refuses when the validated file changed.
func TestRestoreOverwriteRevalidationFailsClosed(t *testing.T) {
	dir := t.TempDir()
	target, bundle := restoreFixture(t, dir)

	actOnRestoreOp(t, "revalidate", func() {
		raced := target + ".raced"
		if err := os.WriteFile(raced, []byte("swapped"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(raced, target); err != nil {
			t.Fatal(err)
		}
	})
	_, err := Restore(bundle, target, RestoreOptions{Overwrite: true, RequestID: "rv-1", OwnerBoundary: dir})
	if err == nil || !strings.Contains(err.Error(), "changed during restore") {
		t.Fatalf("expected fail-closed revalidation error, got %v", err)
	}
	if got := string(fileSum(t, target)); got != "swapped" {
		t.Fatalf("the raced-in file must not be overwritten, got %q", got)
	}
}

// TestRestoreAbsentTargetRaceNeverOverwrites proves an absent-target
// publication uses a no-replace rename: a target that appears inside the
// race window is left untouched without --overwrite authorization.
func TestRestoreAbsentTargetRaceNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	_, bundle := restoreFixture(t, dir)
	target := filepath.Join(dir, "recovery", "noise-tasks", "tasks.sqlite")

	actOnRestoreOp(t, "revalidate", func() {
		if err := os.WriteFile(target, []byte("raced"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	_, err := Restore(bundle, target, RestoreOptions{RequestID: "rc-1", OwnerBoundary: dir})
	if err == nil || !strings.Contains(err.Error(), "--overwrite") {
		t.Fatalf("expected no-overwrite race refusal, got %v", err)
	}
	if got := string(fileSum(t, target)); got != "raced" {
		t.Fatalf("the raced-in target must be preserved, got %q", got)
	}
	sidecarCheck(t, target)
}
