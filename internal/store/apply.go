package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// §8.6 `migrate apply`: the one-way cutover from the exact legacy live.json
// to the current SQLite authority, with the crash-state recovery protocol.

const (
	// ReceiptKindMigration is the §8.4 receipt kind migrate apply embeds.
	ReceiptKindMigration = "migration"
	// StoreFile names the §8.1 authority inside the store directory.
	StoreFile = "tasks.sqlite"
)

// migrateFaultHook is the deterministic crash-injection point for the §8.8
// migration boundary tests. It fires, in effect order, at "journal-durable"
// (after the journal's fsync), "source-retired" (after the source unlink and
// directory fsync), "target-published" (after the target rename and directory
// fsync), and "journal-removal" (before the journal unlink). Production keeps
// the direct no-op; tests that mutate it must serialize.
var migrateFaultHook = func(op string) error { return nil }

// migrateSourceHook is the deterministic injection point for the §8.8 source
// pathname-swap regression test: it fires after the legacy source has been
// securely re-read, fully validated, and its file identity captured, and
// before apply proceeds. Production keeps the direct no-op; tests that
// mutate it must serialize.
var migrateSourceHook = func(legacyPath string) {}

// migrateRetireHook is the deterministic final-window injection point. It
// fires after the last verified source handle is closed and immediately
// before retirement atomically captures the live pathname into a private
// same-directory name. Tests use it to prove a last-moment pathname swap is
// restored and refused, never deleted or followed by target publication.
var migrateRetireHook = func(legacyPath string) {}

// migrateStagedHook is the deterministic injection point for the §8.8
// bundle pathname-swap regression test: it fires after the bundle has been
// securely verified and its exact payload bytes staged from the verified
// open handle, and before apply consumes the staged copy. Production keeps
// the direct no-op; tests that mutate it must serialize.
var migrateStagedHook = func(bundlePath string) {}

// ApplyOptions carries the operator authorizations for §8.6 apply. The CLI
// refuses to construct one without the accepted --digest, --offline, and
// --request-id.
type ApplyOptions struct {
	// RequestID is the operator's request id for the shared (kind,
	// requestId) replay rule.
	RequestID string
	// PlanDigest is the accepted plan digest from `migrate plan`.
	PlanDigest string
}

// ApplyResult reports one apply (or its replay/recovery).
type ApplyResult struct {
	Receipt map[string]any
	// Replayed reports a state 4/5 return of an already-embedded receipt.
	Replayed bool
	// Resumed reports a state 2 resume: a durable journal, source present.
	Resumed bool
	// Reconstructed reports a state 3 reconstruction: no authority on disk,
	// target rebuilt from the verified bundle.
	Reconstructed bool
}

// ApplyMigration performs the §8.6 cutover. commonDir and ownerBoundary are
// the canonical derived identity paths; the plan, source, journal, and
// target are always the exact derived store-directory paths. Every invalid
// presence combination fails closed without deleting or choosing data.
func ApplyMigration(bundlePath, commonDir, ownerBoundary string, opts ApplyOptions) (*ApplyResult, error) {
	// All authorizations validate before any effect (§8.6/§9.1).
	if !ValidLocalKey(opts.RequestID) {
		return nil, fmt.Errorf("migrate apply request id is invalid: must match %s", localKeyGrammar)
	}
	if !digestForm(opts.PlanDigest) {
		return nil, planErrorf("accepted --digest %q is not a lowercase SHA-256 digest", opts.PlanDigest)
	}
	storeDir := filepath.Join(commonDir, "noise-tasks")
	legacyPath := filepath.Join(storeDir, LegacyFile)
	targetPath := filepath.Join(storeDir, StoreFile)
	planPath := filepath.Join(storeDir, PlanFile)
	journalPath := filepath.Join(storeDir, MigrationJournalFile)

	// Stage the verified payload from one securely opened bundle member
	// before anything else: the staged bytes are bound to the manifest's
	// payload digest, so a later pathname swap of the bundle cannot
	// influence what apply publishes.
	scratch, err := os.MkdirTemp("", "noise-tasks-migrate-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	payloadCopy := filepath.Join(scratch, PayloadFile)
	manifest, err := verifyBundle(bundlePath, payloadCopy)
	if err != nil {
		return nil, err
	}
	if manifest.SourceKind != SourceKindLegacyJSON {
		return nil, planErrorf("bundle source kind %q is not %q; migrate apply requires a legacy backup", manifest.SourceKind, SourceKindLegacyJSON)
	}

	// Deterministic regression point: anything that swaps the bundle payload
	// by pathname from here on cannot influence the staged verified bytes.
	migrateStagedHook(bundlePath)

	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create task store directory: %w", err)
	}
	release, err := LockExclusive(storeDir)
	if err != nil {
		return nil, err
	}
	defer release()

	// The strict plan, its accepted digest, and its exact currently derived
	// identity (§8.6): a plan naming any other common dir, store dir,
	// owner boundary, or family refuses.
	plan, err := ReadMigrationPlan(planPath)
	if err != nil {
		return nil, err
	}
	if plan.PlanDigest != opts.PlanDigest {
		return nil, planErrorf("plan digest %s does not match the accepted --digest %s", plan.PlanDigest, opts.PlanDigest)
	}
	familyID := LegacyFamilyUUID(commonDir, ownerBoundary)
	if err := planMatchesDerivedIdentity(plan, legacyPath, targetPath, ownerBoundary, familyID); err != nil {
		return nil, err
	}
	// Every plan↔bundle schema, digest, and inventory fact must agree.
	if err := planMatchesBundle(plan, manifest); err != nil {
		return nil, err
	}

	// Source state, determined only under the lock: a present source is
	// securely re-read, fully validated, and proven unchanged against the
	// plan's source-comparison digest and shared inventory.
	sourceInfo, err := os.Lstat(legacyPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	sourcePresent := err == nil
	if sourcePresent && !sourceInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("legacy source %s is not a regular file", legacyPath)
	}
	var sourceIdent [2]uint64
	var sourceSum [32]byte
	if sourcePresent {
		// Securely re-read the exact source bytes from one open handle and
		// fully validate them (§8.5 legacy surface); the content hash is
		// the retirement guard — (dev, ino) alone can be reused by the
		// filesystem after a pathname swap.
		vf, _, err := openRegularNoFollow(legacyPath)
		if err != nil {
			if err == errNotRegular {
				return nil, fmt.Errorf("legacy source %s is not a regular file", legacyPath)
			}
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(vf, legacyMaxBytes+1))
		vf.Close()
		if err != nil {
			return nil, fmt.Errorf("read legacy source %s: %w", legacyPath, err)
		}
		if len(data) > legacyMaxBytes {
			return nil, legacyRefusalf("legacy document %s exceeds %d bytes", legacyPath, legacyMaxBytes)
		}
		sourceSum = sha256.Sum256(data)
		doc, err := parseLegacyDocument(data)
		if err != nil {
			return nil, err
		}
		snap, err := buildLegacySnapshot(doc)
		if err != nil {
			return nil, err
		}
		comparison, inv, err := legacyComparison(snap)
		if err != nil {
			return nil, err
		}
		if comparison != plan.Source.ComparisonDigest {
			return nil, planErrorf("legacy source comparison digest %s does not match the plan's %s; the legacy live store changed since planning", comparison, plan.Source.ComparisonDigest)
		}
		for _, class := range comparisonClassCounts {
			if want, got := inventoryCount(inv, class), inventoryCount(&plan.Inventory, class); want != got {
				return nil, planErrorf("legacy source and plan inventory differ on shared class %s: source %d, plan %d", class, want, got)
			}
		}
		// Capture the verified file's identity and let the race hook run:
		// the guard immediately before unlink proves the exact verified
		// file is still at the path.
		verified, err := os.Lstat(legacyPath)
		if err != nil {
			return nil, err
		}
		if !verified.Mode().IsRegular() {
			return nil, fmt.Errorf("legacy source %s is not a regular file", legacyPath)
		}
		sourceIdent = fileIdentity(verified)
		migrateSourceHook(legacyPath)
	}

	// Journal state, determined only under the lock. A present journal must
	// be strict and match this plan, family, and request; a mismatch is
	// never overwritten.
	journalInfo, err := os.Lstat(journalPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	journalExists := err == nil
	var journal *MigrationJournal
	if journalExists {
		if !journalInfo.Mode().IsRegular() {
			return nil, planErrorf("migration journal %s is not a regular file", journalPath)
		}
		if journal, err = ReadMigrationJournal(journalPath); err != nil {
			return nil, err
		}
		if err := journalMatchesPlan(journal, plan, opts.RequestID, familyID); err != nil {
			return nil, err
		}
	}

	// Target state, determined only under the lock.
	targetInfo, err := os.Lstat(targetPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	targetPresent := err == nil
	if targetPresent && !targetInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("task store %s is not a regular file", targetPath)
	}

	inputDigest, err := migrationInputDigest(opts.RequestID, plan)
	if err != nil {
		return nil, err
	}

	// §8.6 states 4/5: source absent, target published. Verify the target
	// under the already-held exclusive lock (no shared-lock deadlock),
	// validate it fully, and return its embedded receipt.
	if targetPresent {
		if sourcePresent {
			return nil, planErrorf("dual authority: both the legacy source %s and the target %s exist", legacyPath, targetPath)
		}
		return applyVerifyPublished(storeDir, targetPath, plan, familyID, ownerBoundary, journalExists, journalPath, opts.RequestID, inputDigest)
	}

	// Target absent. A journal means state 2 (source present) or 3 (source
	// absent); no journal with a source is a fresh state 1; neither source
	// nor journal is nothing to migrate from and fails closed.
	res := &ApplyResult{}
	switch {
	case journalExists && sourcePresent:
		res.Resumed = true
	case journalExists && !sourcePresent:
		res.Reconstructed = true
	case !journalExists && sourcePresent:
	default:
		return nil, planErrorf("nothing to migrate: no legacy source, no journal, and no target in %s", storeDir)
	}

	receipt, tmpPath, cleanup, err := buildMigrationTarget(storeDir, payloadCopy, manifest, plan, familyID, ownerBoundary, opts.RequestID, inputDigest)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// The recovery journal is durable before the source is retired. It is
	// written only when absent — a mismatched journal was already refused,
	// and a matching one is never rewritten.
	if !journalExists {
		j := &MigrationJournal{
			SchemaVersion:          JournalSchemaVersion,
			RequestID:              opts.RequestID,
			FamilyID:               familyID,
			PlanDigest:             plan.PlanDigest,
			SourceComparisonDigest: plan.Source.ComparisonDigest,
			BackupPayloadDigest:    plan.Backup.PayloadDigest,
			TargetSchemaVersion:    SchemaVersion,
		}
		if err := writeMigrationJournal(storeDir, j); err != nil {
			return nil, err
		}
	}
	if err := migrateFaultHook("journal-durable"); err != nil {
		return nil, err
	}

	// Retire the exact verified source: reverify identity immediately
	// before the unlink (a pathname swap fails closed), unlink only
	if sourcePresent {
		// Reverify the exact source immediately before the unlink: same
		// regular-file identity and same verified bytes at the path.
		recheck, err := os.Lstat(legacyPath)
		if err != nil {
			return nil, fmt.Errorf("migrate apply refused: legacy source %s changed during apply: %w", legacyPath, err)
		}
		if !recheck.Mode().IsRegular() || fileIdentity(recheck) != sourceIdent {
			return nil, fmt.Errorf("migrate apply refused: legacy source %s changed during apply; re-check the store and re-run", legacyPath)
		}
		vf, verifiedInfo, err := openRegularNoFollow(legacyPath)
		if err != nil {
			return nil, fmt.Errorf("migrate apply refused: legacy source %s changed during apply: %w", legacyPath, err)
		}
		if fileIdentity(verifiedInfo) != sourceIdent {
			vf.Close()
			return nil, fmt.Errorf("migrate apply refused: legacy source %s changed during apply; re-check the store and re-run", legacyPath)
		}
		data, err := io.ReadAll(io.LimitReader(vf, legacyMaxBytes+1))
		vf.Close()
		if err != nil || len(data) > legacyMaxBytes {
			return nil, fmt.Errorf("migrate apply refused: legacy source %s changed during apply; re-check the store and re-run", legacyPath)
		}
		if sum := sha256.Sum256(data); sum != sourceSum {
			return nil, fmt.Errorf("migrate apply refused: legacy source %s changed during apply; re-check the store and re-run", legacyPath)
		}
		migrateRetireHook(legacyPath)
		if err := retireVerifiedLegacySource(storeDir, legacyPath, sourceIdent, sourceSum); err != nil {
			return nil, fmt.Errorf("retire legacy source %s: %w", legacyPath, err)
		}
		if err := fsyncDir(storeDir); err != nil {
			return nil, err
		}
	}
	if err := migrateFaultHook("source-retired"); err != nil {
		return nil, err
	}

	// Publish by atomic no-replace rename, then fsync the directory.
	if err := renameNoReplace(tmpPath, targetPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("migrate apply refused: target %s appeared during apply", targetPath)
		}
		return nil, fmt.Errorf("publish migrated task store %s: %w", targetPath, err)
	}
	if err := fsyncDir(storeDir); err != nil {
		return nil, err
	}
	if err := migrateFaultHook("target-published"); err != nil {
		return nil, err
	}

	// After durable publication the journal is removed and the directory
	// fsynced (§8.6).
	if err := migrateFaultHook("journal-removal"); err != nil {
		return nil, err
	}
	if err := os.Remove(journalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove migration journal: %w", err)
	}
	if err := fsyncDir(storeDir); err != nil {
		return nil, err
	}
	res.Receipt = receipt
	return res, nil
}

// retireVerifiedLegacySource removes the exact verified source without an
// unchecked pathname unlink. A no-replace atomic rename first captures the
// current live.json directory entry under a private same-directory name. The
// captured entry is deleted only after its identity and bytes match the
// verified source. A last-moment replacement is atomically restored to
// live.json and apply fails closed.
func retireVerifiedLegacySource(storeDir, legacyPath string, sourceIdent [2]uint64, sourceSum [32]byte) error {
	placeholder, err := os.CreateTemp(storeDir, ".live.json.retiring-*")
	if err != nil {
		return fmt.Errorf("reserve retirement capture: %w", err)
	}
	capturedPath := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		os.Remove(capturedPath)
		return fmt.Errorf("close retirement capture placeholder: %w", err)
	}
	if err := os.Remove(capturedPath); err != nil {
		return fmt.Errorf("prepare retirement capture: %w", err)
	}
	if err := renameNoReplace(legacyPath, capturedPath); err != nil {
		return fmt.Errorf("atomically capture verified source: %w", err)
	}
	restore := func(reason error) error {
		if err := renameNoReplace(capturedPath, legacyPath); err != nil {
			return fmt.Errorf("%v; additionally could not restore the captured source to live.json: %w (captured file remains at %s)", reason, err, capturedPath)
		}
		return reason
	}
	f, info, err := openRegularNoFollow(capturedPath)
	if err != nil {
		return restore(fmt.Errorf("captured source is not the verified regular file: %w", err))
	}
	if fileIdentity(info) != sourceIdent {
		f.Close()
		return restore(errors.New("source pathname changed before atomic retirement"))
	}
	data, readErr := io.ReadAll(io.LimitReader(f, legacyMaxBytes+1))
	if readErr != nil || len(data) > legacyMaxBytes || sha256.Sum256(data) != sourceSum {
		f.Close()
		if readErr == nil {
			readErr = errors.New("captured source bytes differ from the verified source")
		}
		return restore(readErr)
	}
	// Keep the verified captured handle open across unlink. The live.json
	// entry was already atomically captured and identity-checked; retirement
	// never closes a verified pathname and later unlinks that pathname.
	if err := os.Remove(capturedPath); err != nil {
		f.Close()
		return fmt.Errorf("unlink captured verified source: %w", err)
	}
	_ = f.Close()
	return nil
}

// applyVerifyPublished handles §8.6 states 4 and 5: the target is published
// and must carry the matching embedded migration receipt. State 4 (journal
// present) verifies, removes the journal, and fsyncs; state 5 is replay.
func applyVerifyPublished(storeDir, targetPath string, plan *MigrationPlan, familyID, ownerBoundary string, journalExists bool, journalPath, requestID, inputDigest string) (*ApplyResult, error) {
	target, err := openUnlocked(targetPath, false, "")
	if err != nil {
		return nil, err
	}
	defer target.Close()
	receipt, err := validatePublishedMigrationTarget(target, targetPath, plan, familyID, ownerBoundary, requestID, inputDigest)
	if err != nil {
		return nil, err
	}
	if journalExists {
		if err := os.Remove(journalPath); err != nil {
			return nil, fmt.Errorf("remove migration journal: %w", err)
		}
		if err := fsyncDir(storeDir); err != nil {
			return nil, err
		}
	}
	return &ApplyResult{Receipt: receipt, Replayed: true}, nil
}

func validatePublishedMigrationTarget(target *DB, targetPath string, plan *MigrationPlan, familyID, ownerBoundary, requestID, inputDigest string) (map[string]any, error) {
	if err := target.IntegrityCheck(); err != nil {
		return nil, err
	}
	if err := target.ForeignKeyCheck(); err != nil {
		return nil, err
	}
	if err := target.validateCurrentSchema(); err != nil {
		return nil, err
	}
	if err := target.ValidateRunState(); err != nil {
		return nil, err
	}
	if err := target.ValidateWorktreeSagaState(); err != nil {
		return nil, err
	}
	storedFamily, storedOwner, err := target.Identity()
	if err != nil {
		return nil, err
	}
	if storedFamily != familyID || storedOwner != ownerBoundary {
		return nil, planErrorf("published target family %s on boundary %s does not match the derived family %s on boundary %s", storedFamily, storedOwner, familyID, ownerBoundary)
	}
	var receipt map[string]any
	var receiptID, storedDigest, createdAt, receiptBody string
	err = target.deferred(func(t *tx) error {
		err := t.QueryRow(`SELECT receipt_id, body, digest, created_at
			FROM receipts WHERE kind=? AND request_id=?`, ReceiptKindMigration, requestID).
			Scan(&receiptID, &receiptBody, &storedDigest, &createdAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read migration receipt: %w", err)
		}
		receipt, err = decodeJSONObject([]byte(receiptBody))
		if err != nil {
			return fmt.Errorf("decode migration receipt: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if receipt == nil {
		return nil, planErrorf("published target %s carries no migration receipt for request %s; every unproven target fails closed", targetPath, requestID)
	}
	if err := validateMigrationReceiptEnvelope(receipt, receiptBody, storedDigest, createdAt, requestID, familyID); err != nil {
		return nil, err
	}
	if receipt["inputDigest"] != inputDigest {
		return nil, &ReplayConflict{Kind: ReceiptKindMigration, RequestID: requestID}
	}
	if err := validateMigrationReceipt(receipt, receiptBody, receiptID, storedDigest, createdAt, requestID, inputDigest, familyID, plan); err != nil {
		return nil, err
	}
	return receipt, nil
}

// ValidatePublishedMigrationRecovery validates a state-4 journal and its
// already-open target without removing coordination metadata. Normal commands
// may serve the published authority only after this proof succeeds; cleanup
// remains exclusively the matching migrate apply's job.
func ValidatePublishedMigrationRecovery(target *DB, commonDir, ownerBoundary string) error {
	storeDir := filepath.Join(commonDir, "noise-tasks")
	journal, err := ReadMigrationJournal(filepath.Join(storeDir, MigrationJournalFile))
	if err != nil {
		return err
	}
	plan, err := ReadMigrationPlan(filepath.Join(storeDir, PlanFile))
	if err != nil {
		return err
	}
	familyID := LegacyFamilyUUID(commonDir, ownerBoundary)
	if err := planMatchesDerivedIdentity(plan, filepath.Join(storeDir, LegacyFile), filepath.Join(storeDir, StoreFile), ownerBoundary, familyID); err != nil {
		return err
	}
	if err := journalMatchesPlan(journal, plan, journal.RequestID, familyID); err != nil {
		return err
	}
	inputDigest, err := migrationInputDigest(journal.RequestID, plan)
	if err != nil {
		return err
	}
	_, err = validatePublishedMigrationTarget(target, filepath.Join(storeDir, StoreFile), plan, familyID, ownerBoundary, journal.RequestID, inputDigest)
	return err
}

// buildMigrationTarget constructs and fully verifies a fresh temporary
// database inside storeDir from the exact verified staged payload — through
// SQLite's backup API, never a byte copy of a live database — and embeds the
// permanent migration receipt plus its event in one transaction. The caller
// owns the returned cleanup until publication succeeds.
func buildMigrationTarget(storeDir, payloadCopy string, manifest *Manifest, plan *MigrationPlan, familyID, ownerBoundary, requestID, inputDigest string) (map[string]any, string, func(), error) {
	payload, err := openUnlocked(payloadCopy, false, "")
	if err != nil {
		return nil, "", nil, err
	}
	defer payload.Close()
	storedFamily, storedOwner, err := payload.Identity()
	if err != nil {
		return nil, "", nil, err
	}
	if storedFamily != familyID || storedOwner != ownerBoundary {
		return nil, "", nil, bundleErrorf("payload identity does not match the derived family and owner boundary")
	}

	tmpFile, err := os.CreateTemp(storeDir, ".tasks.sqlite.migrate-*")
	if err != nil {
		return nil, "", nil, err
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	os.Remove(tmpPath) // VACUUM INTO requires a nonexistent destination
	cleanup := func() {
		os.Remove(tmpPath)
		os.Remove(tmpPath + "-wal")
		os.Remove(tmpPath + "-shm")
	}
	if err := payload.BackupTo(tmpPath); err != nil {
		return nil, "", cleanup, err
	}
	fresh, err := openUnlocked(tmpPath, false, "")
	if err != nil {
		return nil, "", cleanup, err
	}
	freshOpen := true
	defer func() {
		if freshOpen {
			_ = fresh.Close()
		}
	}()
	if err := fresh.IntegrityCheck(); err != nil {
		return nil, "", cleanup, err
	}
	if err := fresh.ForeignKeyCheck(); err != nil {
		return nil, "", cleanup, err
	}
	if err := fresh.validateSchemaVersion(manifest.CurrentSchemaVersion); err != nil {
		return nil, "", cleanup, err
	}
	semantic, _, _, err := fresh.ClassDigests()
	if err != nil {
		return nil, "", cleanup, err
	}
	if semantic != manifest.SemanticDigest {
		return nil, "", cleanup, fmt.Errorf("rebuilt database semantic digest does not match the verified bundle")
	}
	if err := fresh.useLegacyClaimAcquisitionIDs(); err != nil {
		return nil, "", cleanup, err
	}

	committedAt := nowUTC()
	receiptID := migrationReceiptID(requestID, plan.PlanDigest)
	var receipt map[string]any
	err = fresh.immediate(func(t *tx) error {
		var err error
		receipt, err = writeMigrationReceipt(t, manifest, plan, familyID, requestID, inputDigest, receiptID, committedAt)
		if err != nil {
			return err
		}
		return validateWorktreeSagaState(t)
	})
	if err != nil {
		return nil, "", cleanup, err
	}
	if err := fresh.validateCurrentSchema(); err != nil {
		return nil, "", cleanup, err
	}
	if err := fresh.ValidateRunState(); err != nil {
		return nil, "", cleanup, err
	}
	if err := fresh.ValidateWorktreeSagaState(); err != nil {
		return nil, "", cleanup, err
	}
	if err := checkpointTruncate(fresh); err != nil {
		return nil, "", cleanup, err
	}
	// Publication operates only on a closed SQLite main file. Closing before
	// removing sidecars/chmod/fsync prevents a late close from changing the
	// bytes after their durability point.
	if err := fresh.Close(); err != nil {
		return nil, "", cleanup, err
	}
	freshOpen = false
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(tmpPath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, "", cleanup, err
		}
	}
	// The durable bytes are the final-mode bytes: chmod before the fsync.
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return nil, "", cleanup, err
	}
	if err := fsyncFile(tmpPath); err != nil {
		return nil, "", cleanup, err
	}
	return receipt, tmpPath, cleanup, nil
}

// writeMigrationReceipt embeds the permanent §8.6 migration receipt and its
// feed event in the caller's transaction. Inventory counts actual
// post-event/post-receipt state.
func writeMigrationReceipt(t *tx, manifest *Manifest, plan *MigrationPlan, familyID, requestID, inputDigest, receiptID, committedAt string) (map[string]any, error) {
	if err := t.appendEvent(Event{
		Kind:   ReceiptKindMigration,
		Action: "migrated",
		At:     committedAt,
		Extra:  map[string]json.RawMessage{"receiptId": json.RawMessage(strconv.Quote(receiptID))},
	}); err != nil {
		return nil, err
	}
	var seq int64
	if err := t.QueryRow(`SELECT last_insert_rowid()`).Scan(&seq); err != nil {
		return nil, fmt.Errorf("read migration event sequence: %w", err)
	}
	inv, err := countClassInventory(t)
	if err != nil {
		return nil, fmt.Errorf("count migrated state inventory: %w", err)
	}
	// The transaction already contains the migration event. Account for the
	// receipt this function is about to insert.
	inv.Receipts++
	receipt := map[string]any{
		"schemaVersion":          int64(ReceiptSchemaVersion),
		"kind":                   ReceiptKindMigration,
		"receiptId":              receiptID,
		"familyId":               familyID,
		"requestId":              requestID,
		"inputDigest":            inputDigest,
		"resultDigest":           manifest.SemanticDigest,
		"inventory":              inventoryRecord(inv),
		"eventSeq":               map[string]any{"first": seq, "last": seq},
		"committedAt":            committedAt,
		"planDigest":             plan.PlanDigest,
		"sourceComparisonDigest": plan.Source.ComparisonDigest,
		"backupPayloadDigest":    manifest.PayloadDigest,
		"backupSemanticDigest":   manifest.SemanticDigest,
		"backupComparisonDigest": manifest.ComparisonDigest,
		"sourceSchema":           manifest.SourceSchema,
		"targetSchemaVersion":    int64(SchemaVersion),
		"publishedAt":            committedAt,
	}
	if _, err := t.insertReceipt(ReceiptKindMigration, receiptID, requestID, receipt, committedAt); err != nil {
		return nil, err
	}
	return receipt, nil
}

// migrationInputDigest digests the operation's normalized, non-secret
// inputs: the request id and every plan and bundle identity digest.
func migrationInputDigest(requestID string, plan *MigrationPlan) (string, error) {
	canonical, err := canonicalJSON(map[string]any{
		"requestId":              requestID,
		"planDigest":             plan.PlanDigest,
		"familyId":               plan.FamilyID,
		"sourceComparisonDigest": plan.Source.ComparisonDigest,
		"backupPayloadDigest":    plan.Backup.PayloadDigest,
		"backupSemanticDigest":   plan.Backup.SemanticDigest,
		"backupComparisonDigest": plan.Backup.ComparisonDigest,
		"targetSchemaVersion":    int64(SchemaVersion),
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// migrationReceiptID derives the deterministic migration receipt id for one
// request and plan, which is what makes state 3 reconstruction and state 5
// replay name the same record.
func migrationReceiptID(requestID, planDigest string) string {
	sum := sha256.Sum256([]byte("noise-tasks-migration-v1\x00" + requestID + "\x00" + planDigest))
	return "migration-" + hex.EncodeToString(sum[:8])
}

// planMatchesDerivedIdentity proves the plan names exactly the currently
// derived source, target, owner, and deterministic family (§8.6).
func planMatchesDerivedIdentity(plan *MigrationPlan, legacyPath, targetPath, ownerBoundary, familyID string) error {
	if plan.Source.Path != legacyPath {
		return planErrorf("plan source path %s is not the currently derived %s", plan.Source.Path, legacyPath)
	}
	if plan.Target.Path != targetPath {
		return planErrorf("plan target path %s is not the currently derived %s", plan.Target.Path, targetPath)
	}
	if plan.Target.OwnerBoundary != ownerBoundary {
		return planErrorf("plan owner boundary %s is not the currently derived %s", plan.Target.OwnerBoundary, ownerBoundary)
	}
	if plan.FamilyID != familyID {
		return planErrorf("plan family %s is not the deterministic family %s of this repository", plan.FamilyID, familyID)
	}
	return nil
}

// planMatchesBundle proves every schema, digest, and inventory fact the plan
// recorded against the verified bundle still holds.
func planMatchesBundle(plan *MigrationPlan, manifest *Manifest) error {
	if plan.Backup.Format != manifest.SchemaVersion ||
		plan.Backup.SourceSchema != manifest.SourceSchema ||
		plan.Backup.CurrentSchema != manifest.CurrentSchemaVersion {
		return planErrorf("plan and bundle backup schemas differ")
	}
	if plan.Backup.PayloadDigest != manifest.PayloadDigest ||
		plan.Backup.SemanticDigest != manifest.SemanticDigest ||
		plan.Backup.ComparisonDigest != manifest.ComparisonDigest {
		return planErrorf("plan and bundle digests differ; the plan was made against a different bundle")
	}
	if plan.Inventory != manifest.Inventory {
		return planErrorf("plan and bundle inventories differ")
	}
	if manifest.CurrentSchemaVersion != orchestratorSchemaVersion {
		return planErrorf("legacy bundle current schema %d is not the compatibility schema %d", manifest.CurrentSchemaVersion, orchestratorSchemaVersion)
	}
	return nil
}

// journalMatchesPlan proves a present journal belongs to this exact plan,
// family, and request; a mismatch is never overwritten.
func journalMatchesPlan(j *MigrationJournal, plan *MigrationPlan, requestID, familyID string) error {
	switch {
	case j.RequestID != requestID:
		return planErrorf("migration journal request %s does not match this request %s", j.RequestID, requestID)
	case j.FamilyID != familyID:
		return planErrorf("migration journal family %s is not this repository family %s", j.FamilyID, familyID)
	case j.PlanDigest != plan.PlanDigest:
		return planErrorf("migration journal plan digest %s does not match the plan %s", j.PlanDigest, plan.PlanDigest)
	case j.SourceComparisonDigest != plan.Source.ComparisonDigest:
		return planErrorf("migration journal source comparison digest does not match the plan")
	case j.BackupPayloadDigest != plan.Backup.PayloadDigest:
		return planErrorf("migration journal backup payload digest does not match the plan")
	case j.TargetSchemaVersion == orchestratorSchemaVersion:
		return planErrorf("migration journal targets schema %d; complete recovery with the target-schema-%d binary before using this binary", orchestratorSchemaVersion, orchestratorSchemaVersion)
	case j.TargetSchemaVersion != SchemaVersion:
		return planErrorf("migration journal target schema %d is not the current schema %d", j.TargetSchemaVersion, SchemaVersion)
	}
	return nil
}

// receiptMatchesPlan proves an embedded migration receipt is the one this
// plan and bundle would have written.
func receiptMatchesPlan(receipt map[string]any, plan *MigrationPlan) error {
	check := func(key, want string) error {
		if got, _ := receipt[key].(string); got != want {
			return planErrorf("published receipt %s %v does not match the plan", key, receipt[key])
		}
		return nil
	}
	for key, want := range map[string]string{
		"planDigest":             plan.PlanDigest,
		"sourceComparisonDigest": plan.Source.ComparisonDigest,
		"backupPayloadDigest":    plan.Backup.PayloadDigest,
		"backupSemanticDigest":   plan.Backup.SemanticDigest,
		"backupComparisonDigest": plan.Backup.ComparisonDigest,
		"sourceSchema":           plan.Backup.SourceSchema,
	} {
		if err := check(key, want); err != nil {
			return err
		}
	}
	if got, _ := receipt["targetSchemaVersion"].(json.Number); got.String() != strconv.Itoa(SchemaVersion) {
		return planErrorf("published receipt targetSchemaVersion %v is not the current schema %d", receipt["targetSchemaVersion"], SchemaVersion)
	}
	return nil
}

// validateMigrationReceipt proves that the published receipt row is the
// exact immutable receipt this plan and request would have written. Replay
// cannot require the current task inventory to remain unchanged, but it can
// and must validate the frozen publication inventory, canonical body, row
// digest, identities, versions, and event range recorded at publication.
func validateMigrationReceiptEnvelope(receipt map[string]any, body, rowDigest, rowCreatedAt, requestID, familyID string) error {
	required := map[string]bool{
		"schemaVersion": true, "kind": true, "receiptId": true,
		"familyId": true, "requestId": true, "inputDigest": true,
		"resultDigest": true, "inventory": true, "eventSeq": true,
		"committedAt": true, "planDigest": true,
		"sourceComparisonDigest": true, "backupPayloadDigest": true,
		"backupSemanticDigest": true, "backupComparisonDigest": true,
		"sourceSchema": true, "targetSchemaVersion": true,
		"publishedAt": true, "receiptDigest": true,
	}
	for key := range required {
		if _, ok := receipt[key]; !ok {
			return planErrorf("published migration receipt is missing required key %q", key)
		}
	}
	for key := range receipt {
		if !required[key] {
			return planErrorf("published migration receipt has unknown key %q", key)
		}
	}
	canonical, err := canonicalJSON(receipt)
	if err != nil || string(canonical) != body {
		return planErrorf("published migration receipt body is not canonical")
	}
	computedDigest, err := receiptDigest(receipt)
	if err != nil {
		return err
	}
	if rowDigest != computedDigest || receipt["receiptDigest"] != rowDigest {
		return planErrorf("published migration receipt digest does not match its row or body")
	}
	if got, ok := receipt["schemaVersion"].(json.Number); !ok || got.String() != strconv.Itoa(ReceiptSchemaVersion) {
		return planErrorf("published migration receipt schemaVersion %v is not %d", receipt["schemaVersion"], ReceiptSchemaVersion)
	}
	for key, want := range map[string]string{
		"kind": ReceiptKindMigration, "familyId": familyID,
		"requestId": requestID, "committedAt": rowCreatedAt,
	} {
		if got, _ := receipt[key].(string); got != want {
			return planErrorf("published migration receipt %s %v does not match %s", key, receipt[key], want)
		}
	}
	if _, ok := receipt["inputDigest"].(string); !ok {
		return planErrorf("published migration receipt inputDigest is not a string")
	}
	return nil
}

func validateMigrationReceipt(receipt map[string]any, body, rowReceiptID, rowDigest, rowCreatedAt, requestID, inputDigest, familyID string, plan *MigrationPlan) error {
	if err := validateMigrationReceiptEnvelope(receipt, body, rowDigest, rowCreatedAt, requestID, familyID); err != nil {
		return err
	}
	expectedReceiptID := migrationReceiptID(requestID, plan.PlanDigest)
	for key, want := range map[string]string{
		"receiptId": expectedReceiptID, "inputDigest": inputDigest,
		"resultDigest": plan.Backup.SemanticDigest, "publishedAt": rowCreatedAt,
	} {
		if got, _ := receipt[key].(string); got != want {
			return planErrorf("published migration receipt %s %v does not match %s", key, receipt[key], want)
		}
	}
	if rowReceiptID != expectedReceiptID {
		return planErrorf("published migration receipt row id %s does not match %s", rowReceiptID, expectedReceiptID)
	}
	if err := receiptMatchesPlan(receipt, plan); err != nil {
		return err
	}
	invObj, ok := receipt["inventory"].(map[string]any)
	if !ok {
		return planErrorf("published migration receipt inventory is not an object")
	}
	gotInventory, err := parseInventory(invObj)
	if err != nil {
		return planErrorf("published migration receipt inventory is invalid: %v", err)
	}
	wantInventory := plan.Inventory
	// Publishing a v3 compatibility payload with this v5 binary retains two
	// new events: schema/upgraded during the private v3->v5 augmentation, then
	// migration/migrated when the receipt is committed. Each append applies
	// the same retention cap, which is equivalent to adding two and capping.
	wantInventory.Events += 2
	if wantInventory.Events > MaxRetainedEvents {
		wantInventory.Events = MaxRetainedEvents
	}
	wantInventory.Receipts++
	wantInventory.WorktreeSagas = plan.Inventory.Bindings
	if *gotInventory != wantInventory {
		return planErrorf("published migration receipt inventory does not match the planned publication inventory")
	}
	seq, ok := receipt["eventSeq"].(map[string]any)
	if !ok || len(seq) != 2 {
		return planErrorf("published migration receipt eventSeq is invalid")
	}
	first, firstOK := seq["first"].(json.Number)
	last, lastOK := seq["last"].(json.Number)
	firstN, firstErr := first.Int64()
	lastN, lastErr := last.Int64()
	if !firstOK || !lastOK || firstErr != nil || lastErr != nil || firstN <= 0 || firstN != lastN {
		return planErrorf("published migration receipt eventSeq is invalid")
	}
	return nil
}
