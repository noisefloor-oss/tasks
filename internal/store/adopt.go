package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/noisefloor-oss/tasks/internal/backlog"
)

// §8.6A one-way adoption of a retired layout into a fresh store. Adoption
// shares the §8.5 bundle contract, the §8.6 plan/journal/receipt machinery,
// and the legacy snapshot/import/validation model; it differs in provenance
// (an operator-named retired-layout document), its own plan and journal
// artifacts, and its refusal to touch the layout document itself.

const (
	// AdoptionPlanFile names the published §8.6A plan inside the store dir.
	AdoptionPlanFile = "adoption-plan.json"
	// AdoptionJournalFile names the §8.6A apply recovery journal.
	AdoptionJournalFile = "adoption-apply.json"
	// SourceKindAdoptedJSON is a bundle of one adopted retired-layout
	// document (the §8.6A adoption compatibility surface).
	SourceKindAdoptedJSON = "adopted-json"
	// AdoptionSourceSchema is the literal sourceSchema of an adoption bundle.
	AdoptionSourceSchema = "adopted-json:1"
	// ReceiptKindAdoption is the §8.4 receipt kind migrate adopt apply embeds.
	ReceiptKindAdoption = "adoption"
	// adoptionPlanDigestDomain prefixes the canonical adoption plan digest.
	adoptionPlanDigestDomain = "noise-tasks-adoption-plan-v1\x00"
	// adoptionReceiptDomain prefixes the deterministic adoption receipt id.
	adoptionReceiptDomain = "noise-tasks-adoption-v1\x00"
)

// AdoptionError reports a fail-closed adoption refusal (exit 1).
type AdoptionError struct{ Reason string }

func (e *AdoptionError) Error() string { return "adoption refused: " + e.Reason }

func adoptionErrorf(format string, args ...any) *AdoptionError {
	return &AdoptionError{Reason: fmt.Sprintf(format, args...)}
}

// --- source document parsing ---

// adoptionDocument is the raw parsed retired-layout §1 document: tag name →
// tag block, with the tag names in byte order.
type adoptionDocument struct {
	blocks map[string]map[string]json.RawMessage
	names  []string
}

// readAdoptionDocument loads and structurally validates one explicitly named
// retired-layout document. A missing, irregular, symlinked, oversized, or
// malformed document fails closed.
func readAdoptionDocument(path string) (*adoptionDocument, error) {
	// The document is opened once with O_NOFOLLOW and read through that
	// handle: regularity and size are validated from the open file, never a
	// re-stat of a pathname that could have been swapped for a symlink.
	f, info, err := openRegularNoFollow(path)
	if err != nil {
		if errors.Is(err, errNotRegular) {
			return nil, legacyRefusalf("adoption source %s is not a regular file", path)
		}
		return nil, fmt.Errorf("read adoption source %s: %w", path, err)
	}
	defer f.Close()
	if info.Size() > legacyMaxBytes {
		return nil, legacyRefusalf("adoption source %s exceeds %d bytes", path, legacyMaxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, legacyMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read adoption source %s: %w", path, err)
	}
	if len(data) > legacyMaxBytes {
		return nil, legacyRefusalf("adoption source %s exceeds %d bytes", path, legacyMaxBytes)
	}
	return parseAdoptionDocument(data)
}

// parseAdoptionDocument parses the §1 exchange format: one top-level object
// mapping nonempty tag names to tag blocks. Strict JSON with no duplicate
// keys is required, and a document with no tag block refuses.
func parseAdoptionDocument(data []byte) (*adoptionDocument, error) {
	if err := rejectDuplicateKeys(data); err != nil {
		return nil, legacyRefusalf("adoption source is not strict JSON: %v", err)
	}
	top, err := rawObject(json.RawMessage(data), "adoption document")
	if err != nil {
		return nil, err
	}
	if len(top) == 0 {
		return nil, legacyRefusalf("adoption document has no tag block")
	}
	doc := &adoptionDocument{blocks: map[string]map[string]json.RawMessage{}}
	for name, block := range top {
		if name == "" {
			return nil, legacyRefusalf("adoption tag name is empty")
		}
		obj, err := rawObject(block, fmt.Sprintf("adoption tag %q", name))
		if err != nil {
			return nil, err
		}
		doc.blocks[name] = obj
		doc.names = append(doc.names, name)
	}
	sort.Strings(doc.names)
	return doc, nil
}

// adoptionTagKnownKeys are the §1 tag-block keys with defined meaning; every
// other block key is an unknown field preserved verbatim in the tag row.
var adoptionTagKnownKeys = map[string]bool{"tasks": true, "metadata": true, "milestones": true}

// buildAdoptionSnapshot converts the parsed §1 document into payload record
// form with the §8.6A semantics: every tag keeps its name verbatim, nextId
// seeds once to max(id)+1 (1 for an empty tag), taskCount and completedCount
// are recounted from the imported tasks, every task takes revision 1 and
// keeps its updatedAt, and the feed starts empty.
func buildAdoptionSnapshot(doc *adoptionDocument) (*legacySnapshot, error) {
	snap := &legacySnapshot{fencing: 0, nextSeq: 1, tags: map[string]*legacyTagSnapshot{}}
	snap.tagName = append([]string(nil), doc.names...)
	for _, name := range snap.tagName {
		entry, err := buildAdoptionTag(name, doc.blocks[name])
		if err != nil {
			return nil, err
		}
		snap.tags[name] = entry
	}
	if err := validateLegacyGraph(snap); err != nil {
		return nil, err
	}
	return snap, nil
}

func buildAdoptionTag(name string, obj map[string]json.RawMessage) (*legacyTagSnapshot, error) {
	entry := &legacyTagSnapshot{name: name}
	for key, raw := range obj {
		if !adoptionTagKnownKeys[key] {
			if entry.extra == nil {
				entry.extra = map[string]json.RawMessage{}
			}
			entry.extra[key] = raw
		}
	}
	// metadata is file-maintenance state: its object shape is validated and
	// it is never migrated (§8.6A).
	if raw, ok := obj["metadata"]; ok && !isJSONNullRaw(raw) {
		if _, err := rawObject(raw, fmt.Sprintf("adoption tag %q metadata", name)); err != nil {
			return nil, err
		}
	}
	if raw, ok := obj["milestones"]; ok && !isJSONNullRaw(raw) {
		if _, err := backlog.ParseMilestones(name, raw); err != nil {
			return nil, legacyRefusalf("adoption tag %q milestones registry: %v", name, err)
		}
		if len(raw) > 0 {
			entry.milestones = raw
		}
	}
	tasksRaw, ok := obj["tasks"]
	if !ok || isJSONNullRaw(tasksRaw) {
		return nil, legacyRefusalf("adoption tag %q is missing \"tasks\"", name)
	}
	var records []json.RawMessage
	if err := json.Unmarshal(tasksRaw, &records); err != nil {
		return nil, fmt.Errorf("adoption tag %q tasks: %w", name, err)
	}
	entry.tasks = make([]*Task, len(records))
	maxID := 0
	done := 0
	for i, raw := range records {
		task, err := taskFromFields(name, raw)
		if err != nil {
			return nil, fmt.Errorf("adoption tag %q task %d: %w", name, i, err)
		}
		if !validStatuses[task.Status] {
			return nil, legacyRefusalf("adoption tag %q task %s has invalid status %q", name, task.ID, task.Status)
		}
		if task.Priority != "" && task.Priority != "high" && task.Priority != "medium" && task.Priority != "low" {
			return nil, legacyRefusalf("adoption tag %q task %s has invalid priority %q", name, task.ID, task.Priority)
		}
		task.Revision = 1
		entry.tasks[i] = task
		if n, err := strconv.Atoi(task.ID); err == nil && n > maxID {
			maxID = n
		}
		if task.Status == "done" {
			done++
		}
	}
	entry.nextID = int64(maxID + 1)
	entry.taskCount = int64(len(records))
	entry.completedCount = int64(done)
	return entry, nil
}

// --- bundle ---

// CreateAdoptionBackup builds one §8.5 adoption bundle (§8.6A) from the
// explicitly named retired-layout document at layoutPath. It never opens,
// creates, or publishes the SQLite authority and never records the layout's
// pathname in the bundle.
func CreateAdoptionBackup(outputPath, layoutPath, commonDir, ownerBoundary, createdBy string) (*Manifest, error) {
	doc, err := readAdoptionDocument(layoutPath)
	if err != nil {
		return nil, err
	}
	snap, err := buildAdoptionSnapshot(doc)
	if err != nil {
		return nil, err
	}
	familyID := LegacyFamilyUUID(commonDir, ownerBoundary)
	return buildLegacyKindBundle(outputPath, snap, familyID, ownerBoundary, createdBy, SourceKindAdoptedJSON, AdoptionSourceSchema)
}

// --- source identity ---

// ResolveAdoptionSource canonicalizes the operator-named layout path (the
// file must exist) and enforces the §8.6A location rules: inside the owner
// boundary, never inside the store directory (which also excludes the
// derived live.json).
func ResolveAdoptionSource(path, ownerBoundary, storeDir string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve adoption source %s: %w", path, err)
	}
	canonical := filepath.Clean(resolved)
	if canonical != ownerBoundary && !strings.HasPrefix(canonical, ownerBoundary+string(filepath.Separator)) {
		return "", adoptionErrorf("adoption source %s is not inside the owner boundary %s", path, ownerBoundary)
	}
	if canonical == storeDir || strings.HasPrefix(canonical, storeDir+string(filepath.Separator)) {
		return "", adoptionErrorf("adoption source %s is inside the store directory %s; that directory's files are protocol artifacts, and its live.json belongs to migrate plan (§8.6)", path, storeDir)
	}
	return canonical, nil
}

// AdoptionFreshGate refuses the states §8.6A never operates on: an existing
// store, a §8.6 recovery journal, an adoption recovery journal, or the
// derived legacy live.json. It is evaluated under a held lock by the plan
// path; bundle creation checks it before reading the source.
func AdoptionFreshGate(storeDir string) error {
	for _, fence := range []struct {
		name string
		why  string
	}{
		{StoreFile, "task store %s already exists; adoption targets a fresh store"},
		{LegacyFile, "the derived legacy live store %s exists; that family belongs to migrate plan and migrate apply (§8.6)"},
	} {
		path := filepath.Join(storeDir, fence.name)
		if _, err := os.Lstat(path); err == nil {
			return adoptionErrorf(fence.why, path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	recovery, err := ReadMigrationRecovery(storeDir)
	if err != nil {
		return err
	}
	if recovery != RecoveryNone {
		return adoptionErrorf("a §8.6 migration recovery journal exists in %s; complete it with migrate apply first", storeDir)
	}
	journal, err := ReadMigrationJournal(filepath.Join(storeDir, AdoptionJournalFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if journal != nil {
		return adoptionErrorf("adoption recovery is already in progress; only the matching migrate adopt apply may advance it")
	}
	return nil
}

// --- plan ---

// CreateAdoptionPlan verifies a §8.5 adoption bundle against the exact named
// layout document and atomically publishes the strict §8.6A plan at
// <storeDir>/adoption-plan.json. It holds the shared maintenance lock across
// source verification and plan publication, changes no authority, and never
// opens the SQLite authority.
func CreateAdoptionPlan(bundlePath, layoutPath, commonDir, ownerBoundary, storeDir, createdBy string) (*MigrationPlan, error) {
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create task store directory: %w", err)
	}
	release, err := LockShared(storeDir)
	if err != nil {
		return nil, err
	}
	defer release()
	// Once apply has a durable journal, the original verified bundle and
	// accepted plan are recovery inputs; plan never replaces them.
	if err := AdoptionFreshGate(storeDir); err != nil {
		return nil, err
	}
	manifest, err := VerifyBundle(bundlePath)
	if err != nil {
		return nil, err
	}
	if manifest.SourceKind != SourceKindAdoptedJSON {
		return nil, adoptionErrorf("bundle source kind %q is not %q; migrate adopt plan requires an adoption backup", manifest.SourceKind, SourceKindAdoptedJSON)
	}
	layoutCanonical, err := ResolveAdoptionSource(layoutPath, ownerBoundary, storeDir)
	if err != nil {
		return nil, err
	}
	doc, err := readAdoptionDocument(layoutCanonical)
	if err != nil {
		return nil, err
	}
	snap, err := buildAdoptionSnapshot(doc)
	if err != nil {
		return nil, err
	}
	sourceComparison, sourceInv, err := legacyComparison(snap)
	if err != nil {
		return nil, err
	}
	// The proofs: source and bundle must be the same logical adopted state.
	if manifest.ComparisonDigest != sourceComparison {
		return nil, adoptionErrorf("source and bundle comparison digests differ: source %s, bundle %s; the adoption source changed since the backup", sourceComparison, manifest.ComparisonDigest)
	}
	for _, class := range comparisonClassCounts {
		want := inventoryCount(sourceInv, class)
		got := inventoryCount(&manifest.Inventory, class)
		if want != got {
			return nil, adoptionErrorf("source and bundle inventory differ on shared class %s: source %d, bundle %d", class, want, got)
		}
	}
	familyID := LegacyFamilyUUID(commonDir, ownerBoundary)
	if manifest.FamilyID != familyID {
		return nil, adoptionErrorf("bundle family %s does not match this repository family %s", manifest.FamilyID, familyID)
	}
	plan := &MigrationPlan{
		SchemaVersion: PlanSchemaVersion,
		CreatedBy:     createdBy,
		CreatedAt:     nowUTC(),
		FamilyID:      familyID,
		Source: PlanSource{
			Kind:             SourceKindAdoptedJSON,
			Schema:           AdoptionSourceSchema,
			ComparisonDigest: sourceComparison,
			Path:             layoutCanonical,
		},
		Backup: PlanBackup{
			Format:           manifest.SchemaVersion,
			SourceSchema:     manifest.SourceSchema,
			CurrentSchema:    manifest.CurrentSchemaVersion,
			PayloadDigest:    manifest.PayloadDigest,
			SemanticDigest:   manifest.SemanticDigest,
			ComparisonDigest: manifest.ComparisonDigest,
		},
		Inventory: manifest.Inventory,
		Target: PlanTarget{
			CurrentSchema: SchemaVersion,
			Path:          filepath.Join(storeDir, StoreFile),
			OwnerBoundary: ownerBoundary,
		},
	}
	if err := publishPlanCodec(storeDir, plan, adoptionPlanCodec); err != nil {
		return nil, err
	}
	return plan, nil
}

// adoptionPlanMatchesDerivedIdentity proves the plan names exactly the
// currently derived target, owner, and deterministic family (§8.6A). The
// strict plan reader proved the source path is absolute, canonical, inside
// the plan's own owner boundary, and outside the store directory.
func adoptionPlanMatchesDerivedIdentity(plan *MigrationPlan, targetPath, ownerBoundary, familyID string) error {
	switch {
	case plan.Target.Path != targetPath:
		return adoptionErrorf("plan target path %s is not the currently derived %s", plan.Target.Path, targetPath)
	case plan.Target.OwnerBoundary != ownerBoundary:
		return adoptionErrorf("plan owner boundary %s is not the currently derived %s", plan.Target.OwnerBoundary, ownerBoundary)
	case plan.FamilyID != familyID:
		return adoptionErrorf("plan family %s is not the deterministic family %s of this repository", plan.FamilyID, familyID)
	}
	return nil
}

// --- apply ---

// ApplyAdoption performs the §8.6A cutover: it publishes a fresh current
// SQLite authority from the verified adoption bundle under the exclusive
// maintenance lock, with the same crash-state recovery discipline as §8.6
// minus source retirement — the layout document is never removed. Every
// invalid presence combination fails closed without deleting or choosing
// data.
func ApplyAdoption(bundlePath, commonDir, ownerBoundary string, opts ApplyOptions) (*ApplyResult, error) {
	// All authorizations validate before any effect (§8.6A follows §8.6).
	if !ValidLocalKey(opts.RequestID) {
		return nil, fmt.Errorf("migrate adopt apply request id is invalid: must match %s", localKeyGrammar)
	}
	if !digestForm(opts.PlanDigest) {
		return nil, adoptionErrorf("accepted --digest %q is not a lowercase SHA-256 digest", opts.PlanDigest)
	}
	storeDir := filepath.Join(commonDir, "noise-tasks")
	targetPath := filepath.Join(storeDir, StoreFile)
	planPath := filepath.Join(storeDir, AdoptionPlanFile)
	journalPath := filepath.Join(storeDir, AdoptionJournalFile)

	// Stage the verified payload from one securely opened bundle member
	// before anything else: the staged bytes are bound to the manifest's
	// payload digest, so a later pathname swap of the bundle cannot
	// influence what apply publishes.
	scratch, err := os.MkdirTemp("", "noise-tasks-adopt-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	payloadCopy := filepath.Join(scratch, PayloadFile)
	manifest, err := verifyBundle(bundlePath, payloadCopy)
	if err != nil {
		return nil, err
	}
	if manifest.SourceKind != SourceKindAdoptedJSON {
		return nil, adoptionErrorf("bundle source kind %q is not %q; migrate adopt apply requires an adoption backup", manifest.SourceKind, SourceKindAdoptedJSON)
	}
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
	// identity (§8.6A).
	plan, err := ReadPlanFile(planPath, adoptionPlanCodec)
	if err != nil {
		return nil, err
	}
	if plan.PlanDigest != opts.PlanDigest {
		return nil, adoptionErrorf("plan digest %s does not match the accepted --digest %s", plan.PlanDigest, opts.PlanDigest)
	}
	familyID := LegacyFamilyUUID(commonDir, ownerBoundary)
	if err := adoptionPlanMatchesDerivedIdentity(plan, targetPath, ownerBoundary, familyID); err != nil {
		return nil, err
	}
	// Every plan↔bundle schema, digest, and inventory fact must agree.
	if err := planMatchesBundle(plan, manifest); err != nil {
		return nil, err
	}
	// §8.6 artifacts never advance alongside adoption (§8.6A).
	recovery, err := ReadMigrationRecovery(storeDir)
	if err != nil {
		return nil, err
	}
	if recovery != RecoveryNone {
		return nil, adoptionErrorf("a §8.6 migration recovery journal exists in %s; complete it with migrate apply first", storeDir)
	}
	if _, err := os.Lstat(filepath.Join(storeDir, LegacyFile)); err == nil {
		return nil, adoptionErrorf("the derived legacy live store %s exists; that family belongs to migrate plan and migrate apply (§8.6)", filepath.Join(storeDir, LegacyFile))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
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
			return nil, adoptionErrorf("adoption journal %s is not a regular file", journalPath)
		}
		if journal, err = ReadMigrationJournal(journalPath); err != nil {
			return nil, err
		}
		if err := journalMatchesPlan(journal, plan, opts.RequestID, familyID); err != nil {
			return nil, err
		}
	}

	// Target state, determined only under the lock. An existing target is
	// only ever the proven adoption target (states 3/4), and those states
	// never consult the source: the operator may remove or edit the layout
	// after publication (§8.6A), so replay must depend on the published
	// receipt alone.
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
	if targetPresent {
		return applyVerifyPublished(storeDir, targetPath, plan, familyID, ownerBoundary, journalExists, journalPath, opts.RequestID, inputDigest, adoptionCutover)
	}

	// Source state, determined only under the lock and required only while
	// the target is absent (states 1/2): the exact plan-named layout
	// document is securely re-read, fully validated, and proven unchanged
	// against the plan's source-comparison digest and shared inventory. The
	// document is never retired.
	layoutPath := plan.Source.Path
	sourceInfo, err := os.Lstat(layoutPath)
	if err != nil {
		return nil, adoptionErrorf("adoption source %s is missing or changed since planning: %v", layoutPath, err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return nil, adoptionErrorf("adoption source %s is not a regular file", layoutPath)
	}
	doc, err := readAdoptionDocument(layoutPath)
	if err != nil {
		return nil, err
	}
	snap, err := buildAdoptionSnapshot(doc)
	if err != nil {
		return nil, err
	}
	comparison, inv, err := legacyComparison(snap)
	if err != nil {
		return nil, err
	}
	if comparison != plan.Source.ComparisonDigest {
		return nil, adoptionErrorf("adoption source comparison digest %s does not match the plan's %s; the adoption source changed since planning", comparison, plan.Source.ComparisonDigest)
	}
	for _, class := range comparisonClassCounts {
		if want, got := inventoryCount(inv, class), inventoryCount(&plan.Inventory, class); want != got {
			return nil, adoptionErrorf("adoption source and plan inventory differ on shared class %s: source %d, plan %d", class, want, got)
		}
	}
	migrateSourceHook(layoutPath)

	// Target absent: a matching journal is the resume state (§8.6A state 2);
	// no journal is fresh (state 1).
	res := &ApplyResult{}
	if journalExists {
		res.Resumed = true
	}
	receipt, tmpPath, cleanup, err := buildMigrationTarget(storeDir, payloadCopy, manifest, plan, familyID, ownerBoundary, opts.RequestID, inputDigest, adoptionCutover)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// The recovery journal is durable before publication. It is written only
	// when absent — a mismatched journal was already refused, and a matching
	// one is never rewritten.
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
		if err := writeJournalTo(storeDir, AdoptionJournalFile, ".adoption-apply-", j); err != nil {
			return nil, err
		}
	}
	if err := migrateFaultHook("journal-durable"); err != nil {
		return nil, err
	}

	// Publish by atomic no-replace rename, then fsync the directory. The
	// layout document stays exactly where it was (§8.6A).
	if err := removeSQLiteSidecars(targetPath); err != nil {
		return nil, err
	}
	if err := renameNoReplace(tmpPath, targetPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, adoptionErrorf("target %s appeared during apply", targetPath)
		}
		return nil, fmt.Errorf("publish adopted task store %s: %w", targetPath, err)
	}
	if err := fsyncDir(storeDir); err != nil {
		return nil, err
	}
	if err := migrateFaultHook("target-published"); err != nil {
		return nil, err
	}

	// After durable publication the journal is removed and the directory
	// fsynced (§8.6A).
	if err := migrateFaultHook("journal-removal"); err != nil {
		return nil, err
	}
	if err := os.Remove(journalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove adoption journal: %w", err)
	}
	if err := fsyncDir(storeDir); err != nil {
		return nil, err
	}
	res.Receipt = receipt
	return res, nil
}

// ValidatePublishedAdoptionRecovery validates a state-3 adoption journal and
// its already-open target without removing coordination metadata. Normal
// commands may serve the published authority only after this proof succeeds;
// cleanup remains exclusively the matching migrate adopt apply's job.
func ValidatePublishedAdoptionRecovery(target *DB, commonDir, ownerBoundary string) error {
	storeDir := filepath.Join(commonDir, "noise-tasks")
	journal, err := ReadMigrationJournal(filepath.Join(storeDir, AdoptionJournalFile))
	if err != nil {
		return err
	}
	plan, err := ReadPlanFile(filepath.Join(storeDir, AdoptionPlanFile), adoptionPlanCodec)
	if err != nil {
		return err
	}
	familyID := LegacyFamilyUUID(commonDir, ownerBoundary)
	if err := adoptionPlanMatchesDerivedIdentity(plan, filepath.Join(storeDir, StoreFile), ownerBoundary, familyID); err != nil {
		return err
	}
	if err := journalMatchesPlan(journal, plan, journal.RequestID, familyID); err != nil {
		return err
	}
	inputDigest, err := migrationInputDigest(journal.RequestID, plan)
	if err != nil {
		return err
	}
	_, err = validatePublishedTarget(target, filepath.Join(storeDir, StoreFile), plan, familyID, ownerBoundary, journal.RequestID, inputDigest, adoptionCutover)
	return err
}

// ReadAdoptionRecovery classifies the §8.7 fencing state of one store
// directory from the adoption journal and the published database's presence.
// A malformed journal is an error — it is never classified or bypassed.
func ReadAdoptionRecovery(storeDir string) (MigrationRecovery, error) {
	journal, err := ReadMigrationJournal(filepath.Join(storeDir, AdoptionJournalFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RecoveryNone, nil
		}
		return RecoveryNone, err
	}
	targetInfo, err := os.Lstat(filepath.Join(storeDir, StoreFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return RecoveryNone, err
	}
	if err == nil {
		if !targetInfo.Mode().IsRegular() {
			return RecoveryNone, planErrorf("adoption journal is state-inconsistent: %s is not a regular file", StoreFile)
		}
		if journal.TargetSchemaVersion < SchemaVersion {
			return RecoveryRequired, nil
		}
		return RecoveryPublished, nil
	}
	return RecoveryRequired, nil
}
