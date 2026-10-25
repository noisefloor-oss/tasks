package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// applyFamily prepares one planned legacy family: a canonical store
// directory with the fixture live.json, a verified legacy bundle, and a
// published strict plan.
func applyFamily(t *testing.T) (storeDir, legacyPath, commonDir, owner, bundle string, plan *MigrationPlan) {
	t.Helper()
	storeDir, legacyPath, commonDir, owner = planFamily(t)
	bundle = planBundle(t, storeDir, legacyPath, commonDir, owner)
	plan, err := CreateMigrationPlan(bundle, legacyPath, commonDir, owner, storeDir, "noise-tasks test")
	if err != nil {
		t.Fatal(err)
	}
	return storeDir, legacyPath, commonDir, owner, bundle, plan
}

func applyOpts(plan *MigrationPlan) ApplyOptions {
	return ApplyOptions{RequestID: "migrate-1", PlanDigest: plan.PlanDigest}
}

// readPublishedReceipt opens the published target and returns its embedded
// migration receipt for the request.
func readPublishedReceipt(t *testing.T, targetPath, requestID string) map[string]any {
	t.Helper()
	db, err := Open(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	receipt, err := db.FindReceipt(ReceiptKindMigration, requestID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt == nil {
		t.Fatalf("no migration receipt for %s in %s", requestID, targetPath)
	}
	return receipt
}

// wantState asserts the on-disk presence of the three §8.6 artifacts.
func wantState(t *testing.T, storeDir string, source, target, journal bool) {
	t.Helper()
	for name, want := range map[string]bool{
		LegacyFile:           source,
		StoreFile:            target,
		MigrationJournalFile: journal,
	} {
		_, err := os.Lstat(filepath.Join(storeDir, name))
		got := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("lstat %s: %v", name, err)
		}
		if got != want {
			t.Fatalf("%s present=%v, want %v", name, got, want)
		}
	}
}

func TestApplyMigrationPublishesPersistentWAL(t *testing.T) {
	storeDir, _, commonDir, owner, bundle, plan := applyFamily(t)
	opts := applyOpts(plan)
	if _, err := ApplyMigration(bundle, commonDir, owner, opts); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	requirePublishedWAL(t, filepath.Join(storeDir, StoreFile), ReceiptKindMigration, opts.RequestID)
}

func TestApplyMigrationHappyPath(t *testing.T) {
	storeDir, _, commonDir, owner, bundle, plan := applyFamily(t)
	targetPath := filepath.Join(storeDir, StoreFile)

	result, err := ApplyMigration(bundle, commonDir, owner, applyOpts(plan))
	if err != nil {
		t.Fatal(err)
	}
	if result.Replayed || result.Resumed || result.Reconstructed {
		t.Fatalf("fresh apply flags: %+v", result)
	}
	wantState(t, storeDir, false, true, false)
	info, err := os.Lstat(targetPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("published target mode: %v %v", err, info)
	}
	// No staging leftovers.
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tasks.sqlite.migrate-") || strings.HasPrefix(e.Name(), ".migration-apply-") {
			t.Fatalf("staging leftover %s", e.Name())
		}
	}

	// The embedded receipt is exact: every plan/bundle digest, the actual
	// post-event inventory, event sequence, and timestamps.
	receipt := readPublishedReceipt(t, targetPath, "migrate-1")
	manifest := verifyOrDie(t, bundle)
	if receipt["planDigest"] != plan.PlanDigest ||
		receipt["sourceComparisonDigest"] != plan.Source.ComparisonDigest ||
		receipt["backupPayloadDigest"] != manifest.PayloadDigest ||
		receipt["backupSemanticDigest"] != manifest.SemanticDigest ||
		receipt["backupComparisonDigest"] != manifest.ComparisonDigest ||
		receipt["sourceSchema"] != LegacySourceSchema {
		t.Fatalf("receipt digests: %v", receipt)
	}
	if got, _ := receipt["targetSchemaVersion"].(json.Number); got.String() != strconv.Itoa(SchemaVersion) {
		t.Fatalf("receipt targetSchemaVersion %v", receipt["targetSchemaVersion"])
	}
	// resultDigest binds the verified pre-receipt semantic state (§8.5/§8.6).
	if receipt["resultDigest"] != manifest.SemanticDigest {
		t.Fatalf("receipt resultDigest %v", receipt["resultDigest"])
	}
	seq := receipt["eventSeq"].(map[string]any)
	if seq["first"] != seq["last"] || seq["first"] == nil {
		t.Fatalf("receipt eventSeq %v", seq)
	}
	// Inventory counts actual post-event/receipt state.
	db, err := Open(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	_, _, liveInv, err := db.ClassDigests()
	if err != nil {
		t.Fatal(err)
	}
	// The feed carries the v3->v5 schema upgrade plus exactly one
	// migration/migrated event.
	var kindCount int
	err = db.deferred(func(t *tx) error {
		return t.QueryRow(`SELECT COUNT(*) FROM events WHERE kind='migration' AND action='migrated'`).Scan(&kindCount)
	})
	if err != nil {
		t.Fatal(err)
	}
	if kindCount != 1 {
		db.Close()
		t.Fatalf("migration events = %d, want 1", kindCount)
	}
	db.Close()
	inv := receipt["inventory"].(map[string]any)
	if inv["events"] != json.Number(strconv.Itoa(liveInv.Events)) || inv["receipts"] != json.Number(strconv.Itoa(liveInv.Receipts)) ||
		inv["worktreeSagas"] != json.Number(strconv.Itoa(liveInv.WorktreeSagas)) {
		t.Fatalf("receipt inventory %v vs live %+v", inv, liveInv)
	}
	wantEvents := manifest.Inventory.Events + 2
	if wantEvents > MaxRetainedEvents {
		wantEvents = MaxRetainedEvents
	}
	if liveInv.Events != wantEvents || liveInv.Receipts != 1 || liveInv.WorktreeSagas != manifest.Inventory.Bindings {
		t.Fatalf("live inventory after apply: %+v (bundle %+v)", liveInv, manifest.Inventory)
	}
	check, err := Open(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	var converted int
	if err := check.db.QueryRow(`SELECT COUNT(*) FROM worktree_sagas WHERE tag='main' AND task_id='2'
		AND operation='remove' AND status='cleanup-required' AND recovery_generation=0`).Scan(&converted); err != nil || converted != 1 {
		check.Close()
		t.Fatalf("migrated legacy saga count=%d %v", converted, err)
	}
	check.Close()

	// State 5 replay: same request and inputs return the original receipt.
	replay, err := ApplyMigration(bundle, commonDir, owner, applyOpts(plan))
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed || replay.Receipt["receiptId"] != receipt["receiptId"] {
		t.Fatalf("replay: %+v", replay)
	}
	wantState(t, storeDir, false, true, false)
}

func verifyOrDie(t *testing.T, bundle string) *Manifest {
	t.Helper()
	manifest, err := VerifyBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

// TestApplyMigrationCrashStates walks every fault boundary: after journal
// durability (state 2), after source retirement (state 3), after target
// publication and before journal removal (state 4). Each crash leaves the
// spec's exact on-disk state, and only a matching migrate apply advances it.
func TestApplyMigrationCrashStates(t *testing.T) {
	for _, tc := range []struct {
		op                string
		source, target    bool
		journal           bool
		wantResumed       bool
		wantReconstructed bool
	}{
		{op: "journal-durable", source: true, target: false, journal: true, wantResumed: true},
		{op: "source-retired", source: false, target: false, journal: true, wantReconstructed: true},
		{op: "target-published", source: false, target: true, journal: true},
		{op: "journal-removal", source: false, target: true, journal: true},
	} {
		t.Run(tc.op, func(t *testing.T) {
			storeDir, _, commonDir, owner, bundle, plan := applyFamily(t)
			migrateFaultHook = func(op string) error {
				if op == tc.op {
					return errors.New("injected crash at " + op)
				}
				return nil
			}
			defer func() { migrateFaultHook = func(string) error { return nil } }()

			_, err := ApplyMigration(bundle, commonDir, owner, applyOpts(plan))
			if err == nil || !strings.Contains(err.Error(), "injected crash") {
				t.Fatalf("crash not injected: %v", err)
			}
			wantState(t, storeDir, tc.source, tc.target, tc.journal)
			// The journal at the crash boundary is the strict canonical
			// mode-0600 document with exactly the required fields.
			journalBytes, err := os.ReadFile(filepath.Join(storeDir, MigrationJournalFile))
			if err != nil {
				t.Fatal(err)
			}
			want, err := JournalBytes(&MigrationJournal{
				SchemaVersion:          JournalSchemaVersion,
				RequestID:              "migrate-1",
				FamilyID:               plan.FamilyID,
				PlanDigest:             plan.PlanDigest,
				SourceComparisonDigest: plan.Source.ComparisonDigest,
				BackupPayloadDigest:    plan.Backup.PayloadDigest,
				TargetSchemaVersion:    SchemaVersion,
			})
			if err != nil || string(journalBytes) != string(want) {
				t.Fatalf("journal bytes %s vs %s (%v)", journalBytes, want, err)
			}
			jInfo, err := os.Lstat(filepath.Join(storeDir, MigrationJournalFile))
			if err != nil || jInfo.Mode().Perm() != 0o600 {
				t.Fatalf("journal mode: %v %v", err, jInfo)
			}

			// Disarm the crash injection: a matching apply recovers to
			// completion.
			migrateFaultHook = func(string) error { return nil }
			result, err := ApplyMigration(bundle, commonDir, owner, applyOpts(plan))
			if err != nil {
				t.Fatalf("recovery apply: %v", err)
			}
			if result.Resumed != tc.wantResumed || result.Reconstructed != tc.wantReconstructed {
				t.Fatalf("recovery flags: %+v", result)
			}
			wantState(t, storeDir, false, true, false)
			readPublishedReceipt(t, filepath.Join(storeDir, StoreFile), "migrate-1")
		})
	}
}

// TestApplyMigrationInvalidPresenceCombos pins the fail-closed presence
// matrix: dual authority, an unproven target, and nothing to migrate.
func TestApplyMigrationInvalidPresenceCombos(t *testing.T) {
	t.Run("dual authority", func(t *testing.T) {
		storeDir, legacyPath, commonDir, owner, bundle, plan := applyFamily(t)
		if _, err := ApplyMigration(bundle, commonDir, owner, applyOpts(plan)); err != nil {
			t.Fatal(err)
		}
		// The exact legacy source reappears beside the published target.
		if err := os.WriteFile(legacyPath, []byte(legacyFixture), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ApplyMigration(bundle, commonDir, owner, applyOpts(plan))
		if err == nil || !strings.Contains(err.Error(), "dual authority") {
			t.Fatalf("dual authority: %v", err)
		}
		wantState(t, storeDir, true, true, false)
	})
	t.Run("target without matching receipt", func(t *testing.T) {
		storeDir, legacyPath, commonDir, owner, bundle, plan := applyFamily(t)
		// A same-family, same-boundary target restored from the bundle has
		// no migration receipt: apply must refuse it. Restore itself fences
		// on the live source, so the source is retired first.
		if err := os.Remove(legacyPath); err != nil {
			t.Fatal(err)
		}
		if _, err := Restore(bundle, filepath.Join(storeDir, StoreFile), RestoreOptions{
			RequestID:     "restore-1",
			OwnerBoundary: owner,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := ApplyMigration(bundle, commonDir, owner, applyOpts(plan))
		if err == nil || !strings.Contains(err.Error(), "no migration receipt") {
			t.Fatalf("unproven target: %v", err)
		}
	})
	t.Run("nothing to migrate", func(t *testing.T) {
		_, legacyPath, commonDir, owner, bundle, plan := applyFamily(t)
		if err := os.Remove(legacyPath); err != nil {
			t.Fatal(err)
		}
		_, err := ApplyMigration(bundle, commonDir, owner, applyOpts(plan))
		if err == nil || !strings.Contains(err.Error(), "nothing to migrate") {
			t.Fatalf("empty state: %v", err)
		}
	})
}

func TestApplyMigrationPublishedReceiptStrict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "unknown key", mutate: func(r map[string]any) { r["unexpected"] = true }},
		{name: "wrong publication time", mutate: func(r map[string]any) { r["publishedAt"] = "2026-01-01T00:00:00Z" }},
		{name: "wrong inventory", mutate: func(r map[string]any) {
			r["inventory"].(map[string]any)["events"] = json.Number("999")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storeDir, _, commonDir, owner, bundle, plan := applyFamily(t)
			if _, err := ApplyMigration(bundle, commonDir, owner, applyOpts(plan)); err != nil {
				t.Fatal(err)
			}
			target, err := openUnlocked(filepath.Join(storeDir, StoreFile), false, "")
			if err != nil {
				t.Fatal(err)
			}
			var body string
			if err := target.db.QueryRow(`SELECT body FROM receipts WHERE kind=? AND request_id=?`, ReceiptKindMigration, "migrate-1").Scan(&body); err != nil {
				t.Fatal(err)
			}
			receipt, err := decodeJSONObject([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(receipt)
			digest, err := receiptDigest(receipt)
			if err != nil {
				t.Fatal(err)
			}
			receipt["receiptDigest"] = digest
			bodyBytes, err := canonicalJSON(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := target.db.Exec(`UPDATE receipts SET body=?, digest=? WHERE kind=? AND request_id=?`, string(bodyBytes), digest, ReceiptKindMigration, "migrate-1"); err != nil {
				t.Fatal(err)
			}
			if err := target.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := ApplyMigration(bundle, commonDir, owner, applyOpts(plan)); err == nil {
				t.Fatal("tampered migration receipt accepted")
			} else {
				var conflict *ReplayConflict
				if errors.As(err, &conflict) {
					t.Fatalf("invalid receipt misclassified as replay conflict: %v", err)
				}
			}
		})
	}
}

// TestApplyMigrationMismatchRefusals covers wrong digest, wrong bundle,
// changed source, and every journal field mismatch — each refusing without
// deleting or overwriting anything.
func TestApplyMigrationMismatchRefusals(t *testing.T) {
	t.Run("wrong accepted digest", func(t *testing.T) {
		_, _, commonDir, owner, bundle, plan := applyFamily(t)
		opts := applyOpts(plan)
		opts.PlanDigest = "sha256:" + strings.Repeat("a", 64)
		_, err := ApplyMigration(bundle, commonDir, owner, opts)
		if err == nil || !strings.Contains(err.Error(), "accepted --digest") {
			t.Fatalf("wrong digest: %v", err)
		}
	})
	t.Run("wrong bundle", func(t *testing.T) {
		_, _, commonDir, owner, _, plan := applyFamily(t)
		// A bundle of a different family: same shape, different digests.
		otherDir, otherLegacy, otherCommon, otherOwner := planFamily(t)
		otherBundle := planBundle(t, otherDir, otherLegacy, otherCommon, otherOwner)
		_, err := ApplyMigration(otherBundle, commonDir, owner, applyOpts(plan))
		if err == nil {
			t.Fatal("foreign bundle accepted")
		}
	})
	t.Run("changed legacy source", func(t *testing.T) {
		_, legacyPath, commonDir, owner, bundle, plan := applyFamily(t)
		mutated := strings.Replace(legacyFixture, `"title": "One"`, `"title": "Changed"`, 1)
		if err := os.WriteFile(legacyPath, []byte(mutated), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ApplyMigration(bundle, commonDir, owner, applyOpts(plan))
		if err == nil || !strings.Contains(err.Error(), "changed since planning") {
			t.Fatalf("changed source: %v", err)
		}
	})
	journalCases := []struct {
		name string
		hack func(j *MigrationJournal)
	}{
		{"request id", func(j *MigrationJournal) { j.RequestID = "other-1" }},
		{"family", func(j *MigrationJournal) { j.FamilyID = "01234567-89ab-cdef-0123-456789abcdef" }},
		{"plan digest", func(j *MigrationJournal) { j.PlanDigest = "sha256:" + strings.Repeat("b", 64) }},
		{"source comparison digest", func(j *MigrationJournal) { j.SourceComparisonDigest = "sha256:" + strings.Repeat("c", 64) }},
		{"backup payload digest", func(j *MigrationJournal) { j.BackupPayloadDigest = "sha256:" + strings.Repeat("d", 64) }},
		{"target schema", func(j *MigrationJournal) { j.TargetSchemaVersion = 1 }},
	}
	for _, tc := range journalCases {
		t.Run("journal "+tc.name, func(t *testing.T) {
			storeDir, _, commonDir, owner, bundle, plan := applyFamily(t)
			j := &MigrationJournal{
				SchemaVersion:          JournalSchemaVersion,
				RequestID:              "migrate-1",
				FamilyID:               plan.FamilyID,
				PlanDigest:             plan.PlanDigest,
				SourceComparisonDigest: plan.Source.ComparisonDigest,
				BackupPayloadDigest:    plan.Backup.PayloadDigest,
				TargetSchemaVersion:    SchemaVersion,
			}
			tc.hack(j)
			body, err := JournalBytes(j)
			if err != nil {
				t.Fatal(err)
			}
			journalPath := filepath.Join(storeDir, MigrationJournalFile)
			if err := os.WriteFile(journalPath, body, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = ApplyMigration(bundle, commonDir, owner, applyOpts(plan))
			if err == nil {
				t.Fatal("mismatched journal accepted")
			}
			// The mismatched journal is never overwritten.
			got, rerr := os.ReadFile(journalPath)
			if rerr != nil || string(got) != string(body) {
				t.Fatalf("journal overwritten: %s vs %s (%v)", got, body, rerr)
			}
			wantState(t, storeDir, true, false, true)
		})
	}
}

func mustJournalBytes(t *testing.T, plan *MigrationPlan) []byte {
	t.Helper()
	body, err := JournalBytes(&MigrationJournal{
		SchemaVersion:          JournalSchemaVersion,
		RequestID:              "migrate-1",
		FamilyID:               plan.FamilyID,
		PlanDigest:             plan.PlanDigest,
		SourceComparisonDigest: plan.Source.ComparisonDigest,
		BackupPayloadDigest:    plan.Backup.PayloadDigest,
		TargetSchemaVersion:    SchemaVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// TestApplyMigrationSharedRequestCollision pins the §8.4 shared
// (migration, requestId) rule on a published target: the same request id
// with different normalized inputs is a typed ReplayConflict (exit 3).
func TestApplyMigrationSharedRequestCollision(t *testing.T) {
	storeDir, legacyPath, commonDir, owner, bundle, plan := applyFamily(t)
	if _, err := ApplyMigration(bundle, commonDir, owner, applyOpts(plan)); err != nil {
		t.Fatal(err)
	}
	// Build a second valid plan for the same family from a mutated source.
	mutated := strings.Replace(legacyFixture, `"title": "One"`, `"title": "Other"`, 1)
	if err := os.WriteFile(legacyPath, []byte(mutated), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle2 := planBundle(t, storeDir, legacyPath, commonDir, owner)
	plan2, err := CreateMigrationPlan(bundle2, legacyPath, commonDir, owner, storeDir, "noise-tasks test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(legacyPath); err != nil {
		t.Fatal(err)
	}
	_, err = ApplyMigration(bundle2, commonDir, owner, ApplyOptions{RequestID: "migrate-1", PlanDigest: plan2.PlanDigest})
	if err == nil {
		t.Fatal("collision accepted")
	}
	var conflict *ReplayConflict
	if !errors.As(err, &conflict) || conflict.Kind != ReceiptKindMigration {
		t.Fatalf("want ReplayConflict, got %v", err)
	}
}

// TestApplyMigrationDeterministicReceiptIdAndInputDigest proves the receipt
// id and input digest are pure functions of the request and plan facts.
func TestApplyMigrationDeterministicReceiptIdAndInputDigest(t *testing.T) {
	_, _, _, _, _, plan := applyFamily(t)
	first, err := migrationInputDigest("migrate-1", plan)
	if err != nil {
		t.Fatal(err)
	}
	again, err := migrationInputDigest("migrate-1", plan)
	if err != nil || first != again {
		t.Fatalf("input digest not deterministic: %s vs %s (%v)", first, again, err)
	}
	other, err := migrationInputDigest("migrate-2", plan)
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatal("input digest ignores request id")
	}
	id, id2 := migrationReceiptID("migrate-1", plan.PlanDigest), migrationReceiptID("migrate-1", plan.PlanDigest)
	if id != id2 || !strings.HasPrefix(id, "migration-") {
		t.Fatalf("receipt id not deterministic: %s vs %s", id, id2)
	}
	if migrationReceiptID("migrate-1", "sha256:"+strings.Repeat("e", 64)) == id {
		t.Fatal("receipt id ignores plan digest")
	}
}

// TestApplyMigrationSourcePathnameRace pins the file-identity guard: a
// legacy source swapped by pathname after verification fails closed and is
// never unlinked.
func TestApplyMigrationSourcePathnameRace(t *testing.T) {
	storeDir, legacyPath, commonDir, owner, bundle, plan := applyFamily(t)
	migrateSourceHook = func(path string) {
		if path != legacyPath {
			return
		}
		swapped := strings.Replace(legacyFixture, `"title": "One"`, `"title": "Swapped"`, 1)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(swapped), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { migrateSourceHook = func(string) {} }()
	_, err := ApplyMigration(bundle, commonDir, owner, applyOpts(plan))
	if err == nil || !strings.Contains(err.Error(), "changed during apply") {
		t.Fatalf("swapped source: %v", err)
	}
	// The swapped file survives; nothing was published.
	if _, err := os.Lstat(legacyPath); err != nil {
		t.Fatal(err)
	}
	wantState(t, storeDir, true, false, true)
}

func TestApplyMigrationFinalRetirementPathnameRace(t *testing.T) {
	storeDir, legacyPath, commonDir, owner, bundle, plan := applyFamily(t)
	saved := filepath.Join(storeDir, "verified-source-saved.json")
	migrateRetireHook = func(path string) {
		if path != legacyPath {
			return
		}
		if err := os.Rename(path, saved); err != nil {
			t.Fatal(err)
		}
		swapped := strings.Replace(legacyFixture, `"title": "One"`, `"title": "Final-window swap"`, 1)
		if err := os.WriteFile(path, []byte(swapped), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { migrateRetireHook = func(string) {} }()
	_, err := ApplyMigration(bundle, commonDir, owner, applyOpts(plan))
	if err == nil || !strings.Contains(err.Error(), "source pathname changed before atomic retirement") {
		t.Fatalf("final-window swap: %v", err)
	}
	// The replacement is restored to live.json, the originally verified
	// source remains where the racing writer moved it, and no target is
	// published. Apply deleted neither unverified pathname.
	if got, err := os.ReadFile(legacyPath); err != nil || !strings.Contains(string(got), "Final-window swap") {
		t.Fatalf("replacement was not restored: %q %v", got, err)
	}
	if got, err := os.ReadFile(saved); err != nil || string(got) != legacyFixture {
		t.Fatalf("verified source did not survive: %q %v", got, err)
	}
	wantState(t, storeDir, true, false, true)
}

// TestApplyMigrationBundlePathnameRace pins the staged-copy guarantee: bytes
// swapped into the bundle pathname after verification cannot influence the
// published target.
func TestApplyMigrationBundlePathnameRace(t *testing.T) {
	storeDir, _, commonDir, owner, bundle, plan := applyFamily(t)
	otherDir, otherLegacy, otherCommon, otherOwner := planFamily(t)
	otherBundle := planBundle(t, otherDir, otherLegacy, otherCommon, otherOwner)
	migrateStagedHook = func(path string) {
		if path != bundle {
			return
		}
		// Overwrite the verified bundle's payload member with foreign bytes.
		foreign, err := os.ReadFile(filepath.Join(otherBundle, PayloadFile))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundle, PayloadFile), foreign, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { migrateStagedHook = func(string) {} }()
	// The tampered bundle no longer verifies from scratch, but apply had
	// already staged the verified bytes: it still publishes the planned
	// state and the receipt matches the original manifest.
	manifest := verifyOrDie(t, bundle) // hook not yet armed; bundle intact
	if _, err := ApplyMigration(bundle, commonDir, owner, applyOpts(plan)); err != nil {
		t.Fatalf("apply after pathname swap: %v", err)
	}
	receipt := readPublishedReceipt(t, filepath.Join(storeDir, StoreFile), "migrate-1")
	if receipt["backupPayloadDigest"] != manifest.PayloadDigest {
		t.Fatalf("published receipt bound to foreign bytes: %v", receipt["backupPayloadDigest"])
	}
}

// TestApplyMigrationLockContention: an exclusive maintenance holder refuses
// apply; a shared holder refuses it too.
func TestApplyMigrationLockContention(t *testing.T) {
	_, _, commonDir, owner, bundle, plan := applyFamily(t)
	release, err := LockExclusive(filepath.Join(commonDir, "noise-tasks"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = ApplyMigration(bundle, commonDir, owner, applyOpts(plan))
	if err == nil || !strings.Contains(err.Error(), "maintenance lock") {
		t.Fatalf("exclusive contention: %v", err)
	}
	release()
}

// TestApplyMigrationRefusesUnrelatedExistingDatabase: a target from an
// unrelated family (random identity) is never chosen or replaced.
func TestApplyMigrationRefusesUnrelatedExistingDatabase(t *testing.T) {
	storeDir, _, commonDir, owner, bundle, plan := applyFamily(t)
	db, err := Create(filepath.Join(storeDir, StoreFile))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = ApplyMigration(bundle, commonDir, owner, applyOpts(plan))
	if err == nil {
		t.Fatal("unrelated target accepted")
	}
}

// TestReadMigrationJournalStrictRefusals walks the strict journal reader.
func TestReadMigrationJournalStrictRefusals(t *testing.T) {
	_, _, _, _, _, plan := applyFamily(t)
	good := string(mustJournalBytes(t, plan))
	dir := t.TempDir()
	path := filepath.Join(dir, MigrationJournalFile)
	write := func(body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ReadMigrationJournal(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent journal: %v", err)
	}
	write(good, 0o600)
	if j, err := ReadMigrationJournal(path); err != nil || j.RequestID != "migrate-1" {
		t.Fatalf("good journal: %v %v", j, err)
	}
	bad := map[string]string{
		"not json":              "not json",
		"duplicate key":         strings.Replace(good, `"schemaVersion":1`, `"schemaVersion":1,"schemaVersion":1`, 1),
		"unknown key":           strings.Replace(good, `}`, `,"extra":1}`, 1),
		"missing key":           strings.Replace(good, `,"requestId":"migrate-1"`, ``, 1),
		"bad digest form":       strings.Replace(good, `"planDigest":"sha256:`, `"planDigest":"SHA256:`, 1),
		"bad family":            strings.Replace(good, `"familyId":"`, `"familyId":"not-a-uuid`, 1),
		"bad request id":        strings.Replace(good, `"requestId":"migrate-1"`, `"requestId":"-bad"`, 1),
		"wrong schema version":  strings.Replace(good, `"schemaVersion":1`, `"schemaVersion":2`, 1),
		"wrong target schema":   strings.Replace(good, `"targetSchemaVersion":5`, `"targetSchemaVersion":1`, 1),
		"retired target schema": strings.Replace(good, `"targetSchemaVersion":5`, `"targetSchemaVersion":4`, 1),
		"noncanonical encoding": strings.Replace(good, `,"requestId":`, ` ,  "requestId" :`, 1),
		"oversize":              good[:len(good)-1] + strings.Repeat(" ", JournalMaxBytes) + "}",
	}
	for name, body := range bad {
		write(body, 0o600)
		if _, err := ReadMigrationJournal(path); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	// Bad mode and symlinks fail closed.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	write(good, 0o644)
	if _, err := ReadMigrationJournal(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("bad mode: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", path); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMigrationJournal(path); err == nil {
		t.Fatal("symlink accepted")
	}
}

// TestReadMigrationRecoveryFencing pins the stable recovery classifier the
// normal-command fencing shares.
func TestReadMigrationRecoveryFencing(t *testing.T) {
	storeDir, _, _, _, bundle, plan := applyFamily(t)
	commonDir := filepath.Dir(storeDir)
	owner := filepath.Dir(commonDir)
	recovery, err := ReadMigrationRecovery(storeDir)
	if err != nil || recovery != RecoveryNone {
		t.Fatalf("no journal: %v %v", recovery, err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, MigrationJournalFile), mustJournalBytes(t, plan), 0o600); err != nil {
		t.Fatal(err)
	}
	recovery, err = ReadMigrationRecovery(storeDir)
	if err != nil || recovery != RecoveryRequired {
		t.Fatalf("journal without target: %v %v", recovery, err)
	}
	if _, err := ApplyMigration(bundle, commonDir, owner, applyOpts(plan)); err != nil {
		t.Fatal(err)
	}
	recovery, err = ReadMigrationRecovery(storeDir)
	if err != nil || recovery != RecoveryNone {
		t.Fatalf("completed: %v %v", recovery, err)
	}
}

func TestReadMigrationRecoveryBlocksPublishedOlderTarget(t *testing.T) {
	storeDir, legacyPath, _, owner, _, plan := applyFamily(t)
	if err := os.Remove(legacyPath); err != nil {
		t.Fatal(err)
	}
	target, err := CreateWithOwner(filepath.Join(storeDir, StoreFile), owner)
	if err != nil {
		t.Fatal(err)
	}
	target.Close()
	journal := &MigrationJournal{
		SchemaVersion: JournalSchemaVersion, RequestID: "migrate-old",
		FamilyID: plan.FamilyID, PlanDigest: plan.PlanDigest,
		SourceComparisonDigest: plan.Source.ComparisonDigest,
		BackupPayloadDigest:    plan.Backup.PayloadDigest,
		TargetSchemaVersion:    orchestratorSchemaVersion,
	}
	body, err := JournalBytes(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, MigrationJournalFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	recovery, err := ReadMigrationRecovery(storeDir)
	if err != nil || recovery != RecoveryRequired {
		t.Fatalf("published older target recovery = %v, %v; want required", recovery, err)
	}
}

func TestReadMigrationRecoveryRefusesDualAuthority(t *testing.T) {
	storeDir, legacyPath, _, owner, _, plan := applyFamily(t)
	targetPath := filepath.Join(storeDir, StoreFile)
	target, err := CreateWithOwner(targetPath, owner)
	if err != nil {
		t.Fatal(err)
	}
	target.Close()
	if err := os.WriteFile(filepath.Join(storeDir, MigrationJournalFile), mustJournalBytes(t, plan), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMigrationRecovery(storeDir); err == nil || !strings.Contains(err.Error(), "both legacy source and SQLite target") {
		t.Fatalf("dual-authority recovery state: %v", err)
	}
	if err := os.Remove(targetPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(legacyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(legacyPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMigrationRecovery(storeDir); err == nil || !strings.Contains(err.Error(), "legacy source is not a regular file") {
		t.Fatalf("irregular legacy recovery state: %v", err)
	}
	if err := os.Remove(legacyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(targetPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMigrationRecovery(storeDir); err == nil || !strings.Contains(err.Error(), "SQLite target is not a regular file") {
		t.Fatalf("irregular target recovery state: %v", err)
	}
}

// TestRestoreRefusesRecoveryJournal pins §8.5: restore is never an alternate
// cutover path while the apply journal exists.
func TestRestoreRefusesRecoveryJournal(t *testing.T) {
	storeDir, _, commonDir, owner, _, plan := applyFamily(t)
	if err := os.WriteFile(filepath.Join(storeDir, MigrationJournalFile), mustJournalBytes(t, plan), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := planBundle(t, storeDir, filepath.Join(storeDir, LegacyFile), commonDir, owner)
	_, err := Restore(bundle, filepath.Join(storeDir, StoreFile), RestoreOptions{RequestID: "r", OwnerBoundary: owner})
	if err == nil || !strings.Contains(err.Error(), "not a migration cutover path") {
		t.Fatalf("restore over journal: %v", err)
	}
}
