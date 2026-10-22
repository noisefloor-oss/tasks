package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// planFamily prepares one legacy family: a canonical
// <common-dir>/noise-tasks store directory holding the fixture live.json
// plus the identity strings its bundle proves.
func planFamily(t *testing.T) (storeDir, legacyPath, commonDir, owner string) {
	t.Helper()
	commonDir = filepath.Join(t.TempDir(), ".git")
	storeDir = filepath.Join(commonDir, "noise-tasks")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyPath = filepath.Join(storeDir, LegacyFile)
	if err := os.WriteFile(legacyPath, []byte(legacyFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return storeDir, legacyPath, commonDir, filepath.Dir(commonDir)
}

// planBundle backs up the fixture family into a fresh bundle path.
func planBundle(t *testing.T, storeDir, legacyPath, commonDir, owner string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "b.ntbackup")
	if _, err := CreateLegacyBackup(out, legacyPath, commonDir, owner, "noise-tasks test"); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMigrationPlanHappyPath(t *testing.T) {
	storeDir, legacyPath, commonDir, owner := planFamily(t)
	before, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	bundle := planBundle(t, storeDir, legacyPath, commonDir, owner)

	plan, err := CreateMigrationPlan(bundle, legacyPath, commonDir, owner, storeDir, "noise-tasks test")
	if err != nil {
		t.Fatal(err)
	}
	if plan.SchemaVersion != PlanSchemaVersion || plan.FamilyID != LegacyFamilyUUID(commonDir, owner) {
		t.Fatalf("plan header: %+v", plan)
	}
	if plan.Source.Kind != SourceKindLegacyJSON || plan.Source.Schema != LegacySourceSchema {
		t.Fatalf("plan source: %+v", plan.Source)
	}
	if plan.Source.Path != legacyPath || plan.Target.Path != filepath.Join(storeDir, "tasks.sqlite") {
		t.Fatalf("plan identities: %+v %+v", plan.Source, plan.Target)
	}
	if plan.Target.OwnerBoundary != owner || plan.Target.CurrentSchema != SchemaVersion {
		t.Fatalf("plan target: %+v", plan.Target)
	}
	if plan.Backup.Format != ManifestSchemaVersion || plan.Backup.SourceSchema != LegacySourceSchema {
		t.Fatalf("plan backup: %+v", plan.Backup)
	}
	if !digestForm(plan.PlanDigest) {
		t.Fatalf("plan digest %q", plan.PlanDigest)
	}

	// Published strictly: exact path, mode 0600, regular file.
	planPath := filepath.Join(storeDir, PlanFile)
	info, err := os.Lstat(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("plan mode %v", info.Mode())
	}
	// No authority was created or mutated, and the source is untouched.
	if _, err := os.Lstat(filepath.Join(storeDir, "tasks.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("tasks.sqlite must not be created: %v", err)
	}
	after, err := os.ReadFile(legacyPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("legacy live.json mutated: %v", err)
	}
	// No staging leftovers.
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".migration-plan-") {
			t.Fatalf("staging leftover %s", e.Name())
		}
	}
	// The plan carries no bundle path and no task content.
	body, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), bundle) || strings.Contains(string(body), "One") || strings.Contains(string(body), "first") {
		t.Fatalf("plan leaks bundle path or task content: %s", body)
	}
	// The strict reader round-trips it.
	reread, err := ReadMigrationPlan(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if *reread != *plan {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", reread, plan)
	}
}

func TestMigrationPlanDeterministicDigest(t *testing.T) {
	storeDir, legacyPath, commonDir, owner := planFamily(t)
	bundle := planBundle(t, storeDir, legacyPath, commonDir, owner)
	p1, err := CreateMigrationPlan(bundle, legacyPath, commonDir, owner, storeDir, "noise-tasks test")
	if err != nil {
		t.Fatal(err)
	}
	// A second plan of the same semantic inputs (same created time) has the
	// same digest; the timestamp is the only free variable.
	p2, err := CreateMigrationPlan(bundle, legacyPath, commonDir, owner, storeDir, "noise-tasks test")
	if err != nil {
		t.Fatal(err)
	}
	if p1.CreatedAt == p2.CreatedAt && p1.PlanDigest != p2.PlanDigest {
		t.Fatalf("same-time plans must share a digest: %s vs %s", p1.PlanDigest, p2.PlanDigest)
	}
	p2.CreatedAt = p1.CreatedAt
	if d, err := p2.computePlanDigest(); err != nil || d != p1.PlanDigest {
		t.Fatalf("digest not deterministic: %s vs %s (%v)", d, p1.PlanDigest, err)
	}
	// Any covered fact changes the digest.
	p2.CreatedAt = p1.CreatedAt
	p2.FamilyID = LegacyFamilyUUID("/other/.git", "/other")
	if d, err := p2.computePlanDigest(); err != nil || d == p1.PlanDigest {
		t.Fatalf("family change must change the digest: %s (%v)", d, err)
	}
}

func TestMigrationPlanSourceMutationRefused(t *testing.T) {
	storeDir, legacyPath, commonDir, owner := planFamily(t)
	bundle := planBundle(t, storeDir, legacyPath, commonDir, owner)
	mutated := strings.ReplaceAll(legacyFixture, `"title": "Two"`, `"title": "Changed"`)
	if err := os.WriteFile(legacyPath, []byte(mutated), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := CreateMigrationPlan(bundle, legacyPath, commonDir, owner, storeDir, "noise-tasks test")
	if err == nil || !strings.Contains(err.Error(), "comparison digests differ") {
		t.Fatalf("mutated source must refuse on comparison digest: %v", err)
	}
	if _, serr := os.Lstat(filepath.Join(storeDir, PlanFile)); !os.IsNotExist(serr) {
		t.Fatalf("refused plan must publish nothing: %v", serr)
	}
}

func TestMigrationPlanWrongFamilyRefused(t *testing.T) {
	storeDir, legacyPath, commonDir, owner := planFamily(t)
	bundle := planBundle(t, storeDir, legacyPath, commonDir, owner)
	_, err := CreateMigrationPlan(bundle, legacyPath, "/other/.git", "/other", storeDir, "noise-tasks test")
	if err == nil || !strings.Contains(err.Error(), "family") {
		t.Fatalf("wrong family must refuse: %v", err)
	}
}

func TestMigrationPlanWrongSourceKindRefused(t *testing.T) {
	storeDir, legacyPath, commonDir, owner := planFamily(t)
	// A current-SQLite bundle is a valid bundle but not a migration source.
	dbDir := t.TempDir()
	db, err := Create(filepath.Join(dbDir, "tasks.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureIdentity(dbDir); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureTag("main"); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "sqlite.ntbackup")
	if _, err := db.CreateBackup(bundle, "noise-tasks test"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = CreateMigrationPlan(bundle, legacyPath, commonDir, owner, storeDir, "noise-tasks test")
	if err == nil || !strings.Contains(err.Error(), "source kind") {
		t.Fatalf("sqlite bundle must refuse: %v", err)
	}
}

func TestMigrationPlanExclusiveLockRefused(t *testing.T) {
	storeDir, legacyPath, commonDir, owner := planFamily(t)
	bundle := planBundle(t, storeDir, legacyPath, commonDir, owner)
	release, err := LockExclusive(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = CreateMigrationPlan(bundle, legacyPath, commonDir, owner, storeDir, "noise-tasks test")
	if err == nil || !strings.Contains(err.Error(), "maintenance lock") {
		t.Fatalf("planning under an exclusive holder must refuse: %v", err)
	}
	release()
	if _, err := CreateMigrationPlan(bundle, legacyPath, commonDir, owner, storeDir, "noise-tasks test"); err != nil {
		t.Fatalf("planning after release: %v", err)
	}
}

func TestMigrationPlanAtomicReplacement(t *testing.T) {
	storeDir, legacyPath, commonDir, owner := planFamily(t)
	bundle := planBundle(t, storeDir, legacyPath, commonDir, owner)
	if _, err := CreateMigrationPlan(bundle, legacyPath, commonDir, owner, storeDir, "noise-tasks test"); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(storeDir, PlanFile)
	// A damaged plan is replaced whole by the next planning run; no partial
	// JSON is ever observable at the plan path.
	if err := os.WriteFile(planPath, []byte("{\"schemaVersion\":1,"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateMigrationPlan(bundle, legacyPath, commonDir, owner, storeDir, "noise-tasks test"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMigrationPlan(planPath); err != nil {
		t.Fatalf("replacement plan must be readable: %v", err)
	}
}

func TestMigrationPlanCannotReplaceRecoveryPlan(t *testing.T) {
	storeDir, legacyPath, commonDir, owner := planFamily(t)
	bundle := planBundle(t, storeDir, legacyPath, commonDir, owner)
	plan, err := CreateMigrationPlan(bundle, legacyPath, commonDir, owner, storeDir, "noise-tasks test")
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(storeDir, PlanFile)
	before, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, MigrationJournalFile), mustJournalBytes(t, plan), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateMigrationPlan(bundle, legacyPath, commonDir, owner, storeDir, "replacement"); err == nil || !strings.Contains(err.Error(), "only the matching migrate apply") {
		t.Fatalf("plan over recovery journal: %v", err)
	}
	after, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("recovery plan was replaced")
	}
}

func TestReadMigrationPlanStrictRefusals(t *testing.T) {
	storeDir, legacyPath, commonDir, owner := planFamily(t)
	bundle := planBundle(t, storeDir, legacyPath, commonDir, owner)
	plan, err := CreateMigrationPlan(bundle, legacyPath, commonDir, owner, storeDir, "noise-tasks test")
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(storeDir, PlanFile)
	good, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), PlanFile)
	write := func(data []byte, mode os.FileMode) {
		os.Remove(bad)
		if err := os.WriteFile(bad, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	refuse := func(what string) {
		t.Helper()
		if _, err := ReadMigrationPlan(bad); err == nil {
			t.Fatalf("%s must be refused", what)
		}
	}

	// Trailing whitespace is a noncanonical encoding.
	write(append(append([]byte{}, good...), ' ', '\n'), 0o600)
	refuse("noncanonical encoding")
	// Oversize document.
	write(append(append([]byte{}, good...), bytesOf(' ', 1<<16)...), 0o600)
	refuse("oversize")
	// Wrong mode.
	write(good, 0o644)
	refuse("mode 0644")
	// Symlink at the plan path.
	os.Remove(bad)
	if err := os.Symlink(planPath, bad); err != nil {
		t.Fatal(err)
	}
	refuse("symlink")
	os.Remove(bad)

	// An unknown top-level key, canonically encoded with an otherwise valid
	// digest, is still refused.
	obj, err := decodeJSONObject(good)
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(fn func(map[string]any)) []byte {
		t.Helper()
		clone := make(map[string]any, len(obj))
		for k, v := range obj {
			clone[k] = v
		}
		fn(clone)
		data, err := canonicalJSON(clone)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	write(mutate(func(m map[string]any) { m["extra"] = int64(1) }), 0o600)
	refuse("unknown key")
	write(mutate(func(m map[string]any) { delete(m, "createdBy") }), 0o600)
	refuse("missing key")
	write(mutate(func(m map[string]any) { m["planDigest"] = "sha256:" + strings.Repeat("0", 64) }), 0o600)
	refuse("digest mismatch")
	write(mutate(func(m map[string]any) {
		src := m["source"].(map[string]any)
		src["path"] = "/elsewhere/live.json"
	}), 0o600)
	refuse("noncanonical derived source identity")

	// The good plan still reads.
	if _, err := ReadMigrationPlan(planPath); err != nil {
		t.Fatalf("good plan refused: %v", err)
	}
	_ = plan
}

// TestReadMigrationPlanCrossFieldRefusals pins the hardened strict
// validation: each variant is a digest-valid canonical plan (the digest is
// recomputed over the mutated content) that still must be refused.
func TestReadMigrationPlanCrossFieldRefusals(t *testing.T) {
	storeDir, legacyPath, commonDir, owner := planFamily(t)
	bundle := planBundle(t, storeDir, legacyPath, commonDir, owner)
	good, err := CreateMigrationPlan(bundle, legacyPath, commonDir, owner, storeDir, "noise-tasks test")
	if err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), PlanFile)
	refuse := func(name string, plan *MigrationPlan) {
		t.Helper()
		body, err := plan.renderPlan()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bad, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadMigrationPlan(bad); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}

	// Duplicate keys are rejected by the strict parser at any level.
	goodBody, err := good.renderPlan()
	if err != nil {
		t.Fatal(err)
	}
	dup := strings.Replace(string(goodBody), `"schemaVersion":1`, `"schemaVersion":1,"schemaVersion":1`, 1)
	if dup == string(goodBody) {
		t.Fatal("duplicate-key mutation did not apply")
	}
	if err := os.WriteFile(bad, []byte(dup), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMigrationPlan(bad); err == nil {
		t.Fatal("duplicate key must be refused")
	}

	// Source and backup comparison digests must agree.
	p := *good
	p.Source.ComparisonDigest = "sha256:" + strings.Repeat("3", 64)
	refuse("source/backup comparison digest disagreement", &p)

	// A legacy plan admits no SQLite-only inventory class.
	p = *good
	p.Inventory.Assignments = 1
	refuse("nonzero legacy-only inventory class", &p)
	p = *good
	p.Inventory.Receipts = 2
	refuse("nonzero receipts inventory class", &p)

	// Non-clean canonical paths.
	p = *good
	p.Source.Path = commonDir + "/noise-tasks/../noise-tasks/" + LegacyFile
	refuse("non-clean source path", &p)
	p = *good
	p.Target.OwnerBoundary = owner + string(filepath.Separator)
	refuse("non-clean owner boundary", &p)

	// A store directory that is not <common>/noise-tasks.
	p = *good
	p.Target.Path = filepath.Join(commonDir, "elsewhere", StoreFile)
	refuse("foreign store directory", &p)

	// A family that is not the deterministic derivation of the plan's own
	// identity.
	p = *good
	p.FamilyID = "01234567-89ab-cdef-0123-456789abcdef"
	refuse("nondeterministic family", &p)

	// The unmutated good plan still reads.
	if _, err := ReadMigrationPlan(filepath.Join(storeDir, PlanFile)); err != nil {
		t.Fatalf("good plan refused: %v", err)
	}
}

func bytesOf(c byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return b
}
