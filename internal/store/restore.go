package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// MigrationJournalFile is the §8.6 apply journal whose presence fences
// restore: restore is never an alternate cutover path.
const MigrationJournalFile = "migration-apply.json"

// restoreFaultHook is the deterministic crash-injection point for the §8.8
// restore fsync/crash-boundary tests. It fires, in publication order, at
// "checkpoint" (existing target), "checkpoint-staged" (fresh database),
// "chmod", "fsync-file", "revalidate", "rename", and "fsync-dir". Production
// keeps the direct no-op; tests that mutate it must serialize.
var restoreFaultHook = func(op string) error { return nil }

// restoreStagedHook is the deterministic injection point for the §8.5
// pathname-swap regression test: it fires after the bundle has been securely
// verified and its exact payload bytes staged from the verified open handle,
// and before restore consumes the staged copy. Production keeps the direct
// no-op; tests that mutate it must serialize.
var restoreStagedHook = func(bundlePath string) {}

// RestoreOptions carries the operator authorizations for §8.5 restore. The
// CLI refuses to construct one without --offline (acknowledging unsupported
// writers are stopped) and --request-id.
type RestoreOptions struct {
	// Overwrite explicitly authorizes replacing an existing target. It never
	// authorizes a wrong-family or wrong-boundary restore.
	Overwrite bool
	// RequestID is the operator's request id for the shared (kind,
	// requestId) replay rule.
	RequestID string
	// OwnerBoundary is the currently derived §8.1 owner boundary. An absent
	// target adopts the bundle's family only when this equals the payload's.
	OwnerBoundary string
}

// RestoreResult reports one restore (or its replay).
type RestoreResult struct {
	Receipt  map[string]any
	Replayed bool
	// Adopted reports the recovery case: no target existed and the bundle's
	// family id was adopted onto the matching derived owner boundary.
	Adopted bool
}

// Restore performs the §8.5 offline atomic restore: it re-verifies the
// bundle, holds the exclusive maintenance lock, builds and verifies a fresh
// temporary database through SQLite's backup interface in the target's own
// directory, embeds the immutable restore receipt, and publishes by fsync and
// atomic rename. It always produces the current tasks.sqlite, never JSON.
func Restore(bundlePath, targetPath string, opts RestoreOptions) (*RestoreResult, error) {
	// The request id's §9.1/§9.2 grammar is checked before any bundle or
	// store effect: an invalid id is a usage refusal, not a bundle verdict.
	if !ValidLocalKey(opts.RequestID) {
		return nil, fmt.Errorf("restore request id is invalid: must match %s", localKeyGrammar)
	}
	// Stage the verified payload before anything else: verification copies
	// the exact hashed bytes from the securely opened payload handle, so
	// restore never reopens a bundle member by pathname and owns the staged
	// copy's cleanup.
	scratch, err := os.MkdirTemp("", "noise-tasks-restore-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	payloadCopy := filepath.Join(scratch, PayloadFile)
	manifest, err := verifyBundle(bundlePath, payloadCopy)
	if err != nil {
		return nil, err
	}
	// Deterministic regression point: anything that swaps the bundle payload
	// by pathname from here on cannot influence the staged verified bytes.
	restoreStagedHook(bundlePath)
	storeDir := filepath.Dir(targetPath)
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create task store directory: %w", err)
	}
	release, err := LockExclusive(storeDir)
	if err != nil {
		return nil, err
	}
	defer release()
	// Restore is never an alternate cutover path (SPEC §8.5). Checked only
	// after the exclusive lock is held so a cooperating migration cannot
	// create a fence between the check and lock acquisition.
	for _, fence := range []string{MigrationJournalFile, AdoptionJournalFile, LegacyFile} {
		if _, err := os.Lstat(filepath.Join(storeDir, fence)); err == nil {
			return nil, fmt.Errorf("restore refused: %s exists in %s; restore is not a migration cutover path", fence, storeDir)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}

	payload, err := openUnlocked(payloadCopy, false, "")
	if err != nil {
		return nil, err
	}
	defer payload.Close()
	familyID, ownerBoundary, err := payload.Identity()
	if err != nil {
		return nil, err
	}
	if familyID != manifest.FamilyID || ownerBoundary == "" {
		return nil, bundleErrorf("payload identity does not match the verified manifest")
	}

	inputDigest, err := restoreInputDigest(opts.RequestID, manifest)
	if err != nil {
		return nil, err
	}

	// Inspect the existing target under the lock before building anything.
	targetInfo, err := os.Lstat(targetPath)
	targetExists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	adopted := false
	var targetIdent [2]uint64
	if targetExists {
		if !targetInfo.Mode().IsRegular() {
			return nil, fmt.Errorf("restore target %s is not a regular file", targetPath)
		}
		targetIdent = fileIdentity(targetInfo)
		target, err := openUnlocked(targetPath, false, "")
		if err != nil {
			return nil, fmt.Errorf("open existing restore target: %w", err)
		}
		targetFamily, targetBoundary, err := target.Identity()
		if err != nil {
			target.Close()
			return nil, err
		}
		if targetFamily == "" || targetBoundary == "" {
			target.Close()
			return nil, fmt.Errorf("existing task store %s has no family identity; run any mutating noise-tasks command to upgrade it before restore", targetPath)
		}
		if targetFamily != familyID || targetBoundary != ownerBoundary {
			target.Close()
			return nil, fmt.Errorf("restore refused: existing store family %s on boundary %s does not match bundle family %s on boundary %s; --overwrite never authorizes a wrong-family or wrong-boundary restore",
				targetFamily, targetBoundary, familyID, ownerBoundary)
		}
		// Replay is consulted before --overwrite is required (SPEC §8.4/§8.5):
		// re-running the normal absent-target restore — including after a
		// post-rename directory-fsync failure — returns the original receipt.
		// The same request with a different bundle is a typed conflict, and a
		// different request never guesses whether overwrite was intended.
		var replay map[string]any
		err = target.deferred(func(t *tx) error {
			var err error
			replay, err = t.findVerifiedReceipt(ReceiptKindRestore, opts.RequestID)
			return err
		})
		if err != nil {
			target.Close()
			return nil, err
		}
		if replay != nil {
			target.Close()
			if replay["inputDigest"] == inputDigest {
				return &RestoreResult{Receipt: replay, Replayed: true}, nil
			}
			return nil, &ReplayConflict{Kind: ReceiptKindRestore, RequestID: opts.RequestID}
		}
		if !opts.Overwrite {
			target.Close()
			return nil, fmt.Errorf("restore target %s already exists; pass --overwrite to explicitly authorize replacing it", targetPath)
		}
		if err := restoreFaultHook("checkpoint"); err != nil {
			target.Close()
			return nil, err
		}
		if err := checkpointTruncate(target); err != nil {
			target.Close()
			return nil, fmt.Errorf("existing target's WAL must checkpoint cleanly before restore: %w", err)
		}
		if err := target.Close(); err != nil {
			return nil, err
		}
		// Remove only checkpointed stale sidecars; the main file is complete.
		if err := removeSQLiteSidecars(targetPath); err != nil {
			return nil, err
		}
	} else {
		if opts.OwnerBoundary == "" || opts.OwnerBoundary != ownerBoundary {
			return nil, fmt.Errorf("restore refused: no store exists at %s and the derived owner boundary %q does not match the bundle's %q", targetPath, opts.OwnerBoundary, ownerBoundary)
		}
		adopted = true
	}

	// Construct and verify a fresh temporary database through SQLite's backup
	// interface in the target's own directory, so publication is a
	// same-directory atomic rename (SPEC §8.5).
	tmpFile, err := os.CreateTemp(storeDir, ".tasks.sqlite.restore-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	os.Remove(tmpPath) // VACUUM INTO requires a nonexistent destination
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			os.Remove(tmpPath)
			os.Remove(tmpPath + "-wal")
			os.Remove(tmpPath + "-shm")
		}
	}()
	if err := payload.BackupTo(tmpPath); err != nil {
		return nil, err
	}
	fresh, err := openUnlocked(tmpPath, false, "")
	if err != nil {
		return nil, err
	}
	if err := fresh.IntegrityCheck(); err != nil {
		fresh.Close()
		return nil, err
	}
	if err := fresh.ForeignKeyCheck(); err != nil {
		fresh.Close()
		return nil, err
	}
	semantic, _, _, err := fresh.ClassDigests()
	if err != nil {
		fresh.Close()
		return nil, err
	}
	if semantic != manifest.SemanticDigest {
		fresh.Close()
		return nil, fmt.Errorf("rebuilt database semantic digest does not match the verified bundle")
	}
	if err := fresh.enableWAL(); err != nil {
		fresh.Close()
		return nil, err
	}
	// A legacy bundle's comparison proof covers its exact v3 compatibility
	// payload. Only the private restore stage receives deterministic claim
	// acquisition ids, before the first ordinary immediate transaction
	// upgrades it to v4; the verified bundle itself remains unchanged.
	if manifest.SourceKind == SourceKindLegacyJSON {
		if err := fresh.useLegacyClaimAcquisitionIDs(); err != nil {
			fresh.Close()
			return nil, err
		}
	}
	// The verified source may be a released v2 or v3 bundle. Configure its
	// proven owner so the staged copy traverses the ordinary transactional
	// path to v4 before the restore receipt is embedded. The source bundle and
	// its staged verification copy remain untouched.
	if err := fresh.SetOwnerBoundary(ownerBoundary); err != nil {
		fresh.Close()
		return nil, err
	}

	// Embed the immutable restore receipt and its feed event in one
	// transaction before publication.
	committedAt := nowUTC()
	receiptID := restoreReceiptID(opts.RequestID, manifest.PayloadDigest)
	var receipt map[string]any
	stagedReplay := false
	err = fresh.immediate(func(t *tx) error {
		receipt, stagedReplay, err = writeRestoreReceipt(t, manifest, opts.RequestID, familyID, inputDigest, receiptID, committedAt)
		if err != nil {
			return err
		}
		return validateWorktreeSagaState(t)
	})
	if err != nil {
		fresh.Close()
		return nil, err
	}
	if err := fresh.validateCurrentSchema(); err != nil {
		fresh.Close()
		return nil, err
	}
	if err := fresh.ValidateRunState(); err != nil {
		fresh.Close()
		return nil, err
	}
	if err := fresh.ValidateRunReceipts(); err != nil {
		fresh.Close()
		return nil, err
	}
	if err := fresh.ValidateWorktreeSagaState(); err != nil {
		fresh.Close()
		return nil, err
	}
	if err := restoreFaultHook("checkpoint-staged"); err != nil {
		fresh.Close()
		return nil, err
	}
	if err := checkpointTruncate(fresh); err != nil {
		fresh.Close()
		return nil, err
	}
	if err := fresh.Close(); err != nil {
		return nil, err
	}
	// Publish: chmod the temporary database to its final private mode
	// BEFORE the final fsync (so the durable bytes are the final-mode
	// bytes), atomically rename it onto the derived target, fsync the
	// directory (SPEC §8.5).
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(tmpPath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if err := restoreFaultHook("chmod"); err != nil {
		return nil, err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return nil, err
	}
	if err := restoreFaultHook("fsync-file"); err != nil {
		return nil, err
	}
	if err := fsyncFile(tmpPath); err != nil {
		return nil, err
	}
	if err := restoreFaultHook("revalidate"); err != nil {
		return nil, err
	}
	if err := restoreFaultHook("rename"); err != nil {
		return nil, err
	}
	if targetExists {
		// Overwrite revalidation: the target must still be the same regular
		// file identity that was validated under the exclusive lock; any
		// change fails closed rather than replacing something unverified.
		recheck, err := os.Lstat(targetPath)
		if err != nil {
			return nil, fmt.Errorf("restore refused: target %s changed during restore: %w", targetPath, err)
		}
		if !recheck.Mode().IsRegular() || fileIdentity(recheck) != targetIdent {
			return nil, fmt.Errorf("restore refused: target %s changed during restore; re-check the store and re-run", targetPath)
		}
		if err := os.Rename(tmpPath, targetPath); err != nil {
			return nil, fmt.Errorf("publish restored task store %s: %w", targetPath, err)
		}
	} else {
		// An absent target publishes with a no-replace rename: a target that
		// appeared in the race window is never overwritten without
		// --overwrite authorization.
		if err := removeSQLiteSidecars(targetPath); err != nil {
			return nil, err
		}
		if err := renameNoReplace(tmpPath, targetPath); err != nil {
			if errors.Is(err, os.ErrExist) {
				return nil, fmt.Errorf("restore refused: target %s appeared during restore without --overwrite authorization", targetPath)
			}
			return nil, fmt.Errorf("publish restored task store %s: %w", targetPath, err)
		}
	}
	cleanupTmp = false
	if err := restoreFaultHook("fsync-dir"); err != nil {
		return nil, err
	}
	if err := fsyncDir(storeDir); err != nil {
		return nil, err
	}
	return &RestoreResult{Receipt: receipt, Adopted: adopted, Replayed: stagedReplay}, nil
}

// writeRestoreReceipt embeds the immutable restore receipt and its feed
// event in the caller's transaction. The staged payload's own receipt table
// participates in the store-wide (kind, requestId) rule: a payload already
// carrying this request's receipt with an identical input digest returns
// that record without a duplicate event or receipt row, and a different
// input is a typed conflict, never a unique-index error.
func writeRestoreReceipt(t *tx, manifest *Manifest, requestID, familyID, inputDigest, receiptID, committedAt string) (map[string]any, bool, error) {
	staged, err := t.findVerifiedReceipt(ReceiptKindRestore, requestID)
	if err != nil {
		return nil, false, err
	}
	if staged != nil {
		if staged["inputDigest"] != inputDigest {
			return nil, false, &ReplayConflict{Kind: ReceiptKindRestore, RequestID: requestID}
		}
		return staged, true, nil
	}
	if err := t.appendEvent(Event{
		Kind:   "restore",
		Action: "restored",
		At:     committedAt,
		Extra:  map[string]json.RawMessage{"receiptId": json.RawMessage(strconv.Quote(receiptID))},
	}); err != nil {
		return nil, false, err
	}
	var seq int64
	if err := t.QueryRow(`SELECT last_insert_rowid()`).Scan(&seq); err != nil {
		return nil, false, fmt.Errorf("read restore event sequence: %w", err)
	}
	inv, err := countClassInventory(t)
	if err != nil {
		return nil, false, fmt.Errorf("count restored state inventory: %w", err)
	}
	// The transaction already contains the restore event. Account for the
	// receipt this function is about to insert.
	inv.Receipts++
	receipt := map[string]any{
		"schemaVersion": int64(ReceiptSchemaVersion),
		"kind":          ReceiptKindRestore,
		"receiptId":     receiptID,
		"familyId":      familyID,
		"requestId":     requestID,
		"inputDigest":   inputDigest,
		"resultDigest":  manifest.SemanticDigest,
		"inventory":     inventoryRecord(inv),
		"eventSeq":      map[string]any{"first": seq, "last": seq},
		"committedAt":   committedAt,
	}
	if _, err := t.insertReceipt(ReceiptKindRestore, receiptID, requestID, receipt, committedAt); err != nil {
		return nil, false, err
	}
	return receipt, false, nil
}

// checkpointTruncate checkpoints and truncates a WAL database, requiring a
// clean (non-busy) checkpoint.
func checkpointTruncate(d *DB) error {
	var busy, log, checkpointed int
	if err := d.db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &log, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("checkpoint of %s is busy", d.path)
	}
	return nil
}

// restoreInputDigest digests the operation's normalized, non-secret inputs:
// the request id and the verified bundle's three digests.
func restoreInputDigest(requestID string, manifest *Manifest) (string, error) {
	canonical, err := canonicalJSON(map[string]any{
		"requestId":        requestID,
		"payloadDigest":    manifest.PayloadDigest,
		"semanticDigest":   manifest.SemanticDigest,
		"comparisonDigest": manifest.ComparisonDigest,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// inventoryRecord renders the inventory as a canonical record value.
func inventoryRecord(inv *ClassInventory) map[string]any {
	return map[string]any{
		"tags": int64(inv.Tags), "milestones": int64(inv.Milestones),
		"tasks": int64(inv.Tasks), "dependencies": int64(inv.Dependencies),
		"subtasks": int64(inv.Subtasks), "bindings": int64(inv.Bindings),
		"assignments": int64(inv.Assignments), "claims": int64(inv.Claims),
		"events": int64(inv.Events), "receipts": int64(inv.Receipts),
		"plans": int64(inv.Plans), "planMappings": int64(inv.PlanMappings),
		"runs": int64(inv.Runs), "attempts": int64(inv.Attempts),
		"worktreeSagas": int64(inv.WorktreeSagas),
	}
}
