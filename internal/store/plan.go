package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// §8.6 `migrate plan` (batch 1: planning only). The plan is a strict,
// bounded, content-free record published at
// <storeDir>/migration-plan.json; it changes no authority and carries no
// bundle path or task content.
const (
	// PlanFile names the published migration plan inside the store dir.
	PlanFile = "migration-plan.json"
	// PlanMaxBytes bounds the plan document.
	PlanMaxBytes = 64 << 10
	// PlanSchemaVersion is the plan format this binary writes.
	PlanSchemaVersion = 1
	// planDigestDomain prefixes the canonical plan digest input.
	planDigestDomain = "noise-tasks-migration-plan-v1\x00"
)

// planCodec names one strict plan surface. The §8.6 migration plan and the
// §8.6A adoption plan share shape, encoding, and size bounds; they differ in
// digest domain, source-kind literals, and the source-path rule.
type planCodec struct {
	what         string // human name in diagnostics
	sourceKind   string
	sourceSchema string
	domain       string // canonical digest domain
	adoption     bool
}

// file names the codec's published plan inside the store directory.
func (c planCodec) file() string {
	if c.adoption {
		return AdoptionPlanFile
	}
	return PlanFile
}

var (
	migrationPlanCodec = planCodec{
		what:         "migration plan",
		sourceKind:   SourceKindLegacyJSON,
		sourceSchema: LegacySourceSchema,
		domain:       planDigestDomain,
	}
	adoptionPlanCodec = planCodec{
		what:         "adoption plan",
		sourceKind:   SourceKindAdoptedJSON,
		sourceSchema: AdoptionSourceSchema,
		domain:       adoptionPlanDigestDomain,
		adoption:     true,
	}
)

// PlanError reports a fail-closed migration plan refusal (exit 1).
type PlanError struct{ Reason string }

func (e *PlanError) Error() string { return "invalid migration plan: " + e.Reason }

func planErrorf(format string, args ...any) *PlanError {
	return &PlanError{Reason: fmt.Sprintf(format, args...)}
}

// PlanSource records the verified legacy source facts: its kind and schema,
// the source-side comparison digest the plan was proven against, and the
// derived canonical source identity (never a caller-supplied path).
type PlanSource struct {
	Kind             string `json:"kind"`
	Schema           string `json:"schema"`
	ComparisonDigest string `json:"comparisonDigest"`
	Path             string `json:"path"`
}

// PlanBackup records the verified §8.5 bundle facts the plan was proven
// against: the backup format, the source and current schemas it names, and
// its three digests. It never records the bundle's pathname.
type PlanBackup struct {
	Format           int    `json:"format"`
	SourceSchema     string `json:"sourceSchema"`
	CurrentSchema    int    `json:"currentSchema"`
	PayloadDigest    string `json:"payloadDigest"`
	SemanticDigest   string `json:"semanticDigest"`
	ComparisonDigest string `json:"comparisonDigest"`
}

// PlanTarget records the target the plan will build: the current SQLite
// schema this binary owns, the derived canonical target path, and the
// canonical owner boundary.
type PlanTarget struct {
	CurrentSchema int    `json:"currentSchema"`
	Path          string `json:"path"`
	OwnerBoundary string `json:"ownerBoundary"`
}

// MigrationPlan is the strict §8.6 plan document. Every field is required.
type MigrationPlan struct {
	SchemaVersion int            `json:"schemaVersion"`
	CreatedBy     string         `json:"createdBy"`
	CreatedAt     string         `json:"createdAt"`
	FamilyID      string         `json:"familyId"`
	Source        PlanSource     `json:"source"`
	Backup        PlanBackup     `json:"backup"`
	Inventory     ClassInventory `json:"inventory"`
	Target        PlanTarget     `json:"target"`
	PlanDigest    string         `json:"planDigest"`
}

// comparisonClassesWithCounts names the shared §8.5 comparison classes that
// carry a variable-cardinality inventory counter (fence is a singleton).
var comparisonClassCounts = []string{
	"tags", "milestones", "tasks", "dependencies", "subtasks", "bindings",
	"claims", "events",
}

func inventoryCount(inv *ClassInventory, class string) int {
	switch class {
	case "tags":
		return inv.Tags
	case "milestones":
		return inv.Milestones
	case "tasks":
		return inv.Tasks
	case "dependencies":
		return inv.Dependencies
	case "subtasks":
		return inv.Subtasks
	case "bindings":
		return inv.Bindings
	case "claims":
		return inv.Claims
	case "events":
		return inv.Events
	default:
		return -1
	}
}

// inventoryMap renders one ClassInventory as the canonical counter map.
func inventoryMap(inv *ClassInventory) map[string]any {
	out := make(map[string]any, 15)
	for _, field := range []struct {
		name  string
		value int
	}{
		{"tags", inv.Tags}, {"milestones", inv.Milestones},
		{"tasks", inv.Tasks}, {"dependencies", inv.Dependencies},
		{"subtasks", inv.Subtasks}, {"bindings", inv.Bindings},
		{"assignments", inv.Assignments}, {"claims", inv.Claims},
		{"events", inv.Events}, {"receipts", inv.Receipts},
		{"plans", inv.Plans}, {"planMappings", inv.PlanMappings},
		{"runs", inv.Runs}, {"attempts", inv.Attempts},
		{"worktreeSagas", inv.WorktreeSagas},
	} {
		out[field.name] = int64(field.value)
	}
	return out
}

// planValue renders the plan's digest-covered content (everything except
// planDigest) in the canonical value domain.
func (p *MigrationPlan) planValue() map[string]any {
	return map[string]any{
		"schemaVersion": int64(p.SchemaVersion),
		"createdBy":     p.CreatedBy,
		"createdAt":     p.CreatedAt,
		"familyId":      p.FamilyID,
		"source": map[string]any{
			"kind":             p.Source.Kind,
			"schema":           p.Source.Schema,
			"comparisonDigest": p.Source.ComparisonDigest,
			"path":             p.Source.Path,
		},
		"backup": map[string]any{
			"format":           int64(p.Backup.Format),
			"sourceSchema":     p.Backup.SourceSchema,
			"currentSchema":    int64(p.Backup.CurrentSchema),
			"payloadDigest":    p.Backup.PayloadDigest,
			"semanticDigest":   p.Backup.SemanticDigest,
			"comparisonDigest": p.Backup.ComparisonDigest,
		},
		"inventory": inventoryMap(&p.Inventory),
		"target": map[string]any{
			"currentSchema": int64(p.Target.CurrentSchema),
			"path":          p.Target.Path,
			"ownerBoundary": p.Target.OwnerBoundary,
		},
	}
}

// computePlanDigestWith computes the canonical plan digest of one codec's
// surface: SHA-256 over the codec's domain string and the canonical JSON of
// the plan excluding planDigest.
func (p *MigrationPlan) computePlanDigestWith(c planCodec) (string, error) {
	body, err := canonicalJSON(p.planValue())
	if err != nil {
		return "", fmt.Errorf("render plan canonically: %w", err)
	}
	h := sha256.New()
	h.Write([]byte(c.domain))
	h.Write(body)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func (p *MigrationPlan) computePlanDigest() (string, error) {
	return p.computePlanDigestWith(migrationPlanCodec)
}

// renderPlanWith produces one codec's exact published bytes — the canonical
// digest-covered content plus the planDigest member — and records that digest
// on the plan.
func (p *MigrationPlan) renderPlanWith(c planCodec) ([]byte, error) {
	digest, err := p.computePlanDigestWith(c)
	if err != nil {
		return nil, err
	}
	value := p.planValue()
	value["planDigest"] = digest
	body, err := canonicalJSON(value)
	if err != nil {
		return nil, fmt.Errorf("render plan canonically: %w", err)
	}
	if len(body) > PlanMaxBytes {
		return nil, planErrorf("plan exceeds %d bytes", PlanMaxBytes)
	}
	p.PlanDigest = digest
	return body, nil
}

func (p *MigrationPlan) renderPlan() ([]byte, error) { return p.renderPlanWith(migrationPlanCodec) }

// CreateMigrationPlan verifies a §8.5 legacy bundle against the exact derived
// legacy live.json and atomically publishes the strict §8.6 plan at
// <storeDir>/migration-plan.json. It holds the shared maintenance lock across
// source verification and plan publication so migration apply cannot retire
// the source concurrently, and it never opens, creates, or mutates the SQLite
// authority. commonDir and ownerBoundary are the canonical paths; legacyPath
// and storeDir are derived from commonDir by the caller.
func CreateMigrationPlan(bundlePath, legacyPath, commonDir, ownerBoundary, storeDir, createdBy string) (*MigrationPlan, error) {
	// The lock spans source verification and plan publication. Plan itself
	// opens no database, so this shared hold cannot double-lock.
	info, err := os.Stat(storeDir)
	if err != nil || !info.IsDir() {
		return nil, planErrorf("store directory %s does not exist; the legacy live store lives at <git-common-dir>/noise-tasks/live.json", storeDir)
	}
	release, err := LockShared(storeDir)
	if err != nil {
		return nil, err
	}
	defer release()
	// Defense in depth for non-CLI callers: once apply has published its
	// journal, only that matching apply may advance recovery. In particular,
	// never atomically replace the plan whose digest the journal binds.
	recovery, err := ReadMigrationRecovery(storeDir)
	if err != nil {
		return nil, err
	}
	if recovery != RecoveryNone {
		return nil, planErrorf("migration recovery is already in progress; only the matching migrate apply may advance it")
	}
	// §8.6 and §8.6A never advance each other's state (§8.7): an adoption
	// recovery journal fences the §8.6 plan too.
	if _, err := ReadMigrationJournal(filepath.Join(storeDir, AdoptionJournalFile)); err == nil {
		return nil, planErrorf("an adoption recovery journal exists in %s; complete it with migrate adopt apply first", storeDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	manifest, err := VerifyBundle(bundlePath)
	if err != nil {
		return nil, err
	}
	if manifest.SourceKind != SourceKindLegacyJSON {
		return nil, planErrorf("bundle source kind %q is not %q; migrate plan requires a legacy backup", manifest.SourceKind, SourceKindLegacyJSON)
	}

	// Re-read and fully validate the exact derived source through the same
	// parser/import model as legacy backup (§8.5).
	doc, err := readLegacyDocument(legacyPath)
	if err != nil {
		return nil, err
	}
	snap, err := buildLegacySnapshot(doc)
	if err != nil {
		return nil, err
	}
	sourceComparison, sourceInv, err := legacyComparison(snap)
	if err != nil {
		return nil, err
	}

	// The proofs: source and bundle must be the same logical legacy state.
	if manifest.ComparisonDigest != sourceComparison {
		return nil, planErrorf("source and bundle comparison digests differ: source %s, bundle %s; the legacy live store changed since the backup", sourceComparison, manifest.ComparisonDigest)
	}
	for _, class := range comparisonClassCounts {
		want := inventoryCount(sourceInv, class)
		got := inventoryCount(&manifest.Inventory, class)
		if want != got {
			return nil, planErrorf("source and bundle inventory differ on shared class %s: source %d, bundle %d", class, want, got)
		}
	}
	familyID := LegacyFamilyUUID(commonDir, ownerBoundary)
	if manifest.FamilyID != familyID {
		return nil, planErrorf("bundle family %s does not match this repository family %s", manifest.FamilyID, familyID)
	}

	plan := &MigrationPlan{
		SchemaVersion: PlanSchemaVersion,
		CreatedBy:     createdBy,
		CreatedAt:     nowUTC(),
		FamilyID:      familyID,
		Source: PlanSource{
			Kind:             SourceKindLegacyJSON,
			Schema:           LegacySourceSchema,
			ComparisonDigest: sourceComparison,
			Path:             legacyPath,
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
			Path:          filepath.Join(storeDir, "tasks.sqlite"),
			OwnerBoundary: ownerBoundary,
		},
	}
	if err := publishPlan(storeDir, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// publishPlanCodec atomically writes one codec's plan: a mode-0600 temporary
// in the same store directory, fsync of the file, atomic rename over any
// prior plan, and fsync of the directory. A repeat with the same semantic
// inputs replaces the prior plan atomically; no partial JSON is ever visible
// at the plan path.
func publishPlanCodec(storeDir string, plan *MigrationPlan, c planCodec) error {
	body, err := plan.renderPlanWith(c)
	if err != nil {
		return err
	}
	name := c.file()
	tmp, err := os.CreateTemp(storeDir, "."+name+"-*")
	if err != nil {
		return fmt.Errorf("stage %s in %s: %w", c.what, storeDir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod staged %s: %w", c.what, err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write staged %s: %w", c.what, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync staged %s: %w", c.what, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close staged %s: %w", c.what, err)
	}
	if err := os.Rename(tmpPath, filepath.Join(storeDir, name)); err != nil {
		return fmt.Errorf("publish %s: %w", c.what, err)
	}
	return fsyncDir(storeDir)
}

func publishPlan(storeDir string, plan *MigrationPlan) error {
	return publishPlanCodec(storeDir, plan, migrationPlanCodec)
}

// ReadPlanFile loads and strictly validates one codec's plan at path:
// bounded size, regular non-symlink mode-0600 file, strict JSON with no
// duplicate or unknown keys, canonical encoding, exact field set and forms,
// derived source/target identity agreement, and a matching recomputed plan
// digest over the codec's domain.
func ReadPlanFile(path string, c planCodec) (*MigrationPlan, error) {
	f, info, err := openRegularNoFollow(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, err
		}
		if reason, ok := err.(*PlanError); ok {
			return nil, reason
		}
		return nil, planErrorf("%s is not a regular file: %v", path, err)
	}
	defer f.Close()
	if info.Mode().Perm() != 0o600 {
		return nil, planErrorf("%s mode %o is not 0600", path, info.Mode().Perm())
	}
	if info.Size() > PlanMaxBytes {
		return nil, planErrorf("%s exceeds %d bytes", path, PlanMaxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, PlanMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s %s: %w", c.what, path, err)
	}
	if len(data) > PlanMaxBytes {
		return nil, planErrorf("%s exceeds %d bytes", path, PlanMaxBytes)
	}
	obj, err := decodeJSONObject(data)
	if err != nil {
		return nil, planErrorf("%s is not strict JSON: %v", path, err)
	}
	plan, err := validatePlanCodec(obj, c)
	if err != nil {
		return nil, err
	}
	// The published bytes must be the canonical rendering: any other valid
	// encoding of the same plan is noncanonical and refused.
	canon, err := plan.renderPlanWith(c)
	if err != nil {
		return nil, err
	}
	if string(canon) != string(data) {
		return nil, planErrorf("%s is not the canonical plan encoding", path)
	}
	return plan, nil
}

// ReadMigrationPlan loads and strictly validates the §8.6 migration plan at
// path.
func ReadMigrationPlan(path string) (*MigrationPlan, error) {
	return ReadPlanFile(path, migrationPlanCodec)
}

// validatePlanCodec enforces the exact §8.6/§8.6A plan field set, forms, and
// derived identity agreement for one codec's surface.
func validatePlanCodec(obj map[string]any, c planCodec) (*MigrationPlan, error) {
	required := []string{
		"schemaVersion", "createdBy", "createdAt", "familyId", "source",
		"backup", "inventory", "target", "planDigest",
	}
	strictKeys := func(obj map[string]any, what string, keys ...string) error {
		for _, key := range keys {
			if _, ok := obj[key]; !ok {
				return planErrorf("%s is missing required key %q", what, key)
			}
		}
		for key := range obj {
			known := false
			for _, k := range keys {
				if key == k {
					known = true
					break
				}
			}
			if !known {
				return planErrorf("%s has unknown key %q", what, key)
			}
		}
		return nil
	}
	if err := strictKeys(obj, "plan", required...); err != nil {
		return nil, err
	}
	intField := func(obj map[string]any, what, key string) (int64, error) {
		v, ok := obj[key].(json.Number)
		if !ok {
			return 0, planErrorf("%s key %q is not an integer", what, key)
		}
		n, err := v.Int64()
		if err != nil {
			return 0, planErrorf("%s key %q is not an integer", what, key)
		}
		return n, nil
	}
	strField := func(obj map[string]any, what, key string) (string, error) {
		v, ok := obj[key].(string)
		if !ok || v == "" {
			return "", planErrorf("%s key %q is not a nonempty string", what, key)
		}
		return v, nil
	}
	digestField := func(obj map[string]any, what, key string) (string, error) {
		v, err := strField(obj, what, key)
		if err != nil {
			return "", err
		}
		if !digestForm(v) {
			return "", planErrorf("%s key %q is not a lowercase SHA-256 digest", what, key)
		}
		return v, nil
	}

	p := &MigrationPlan{}
	schemaVersion, err := intField(obj, "plan", "schemaVersion")
	if err != nil {
		return nil, err
	}
	if schemaVersion != PlanSchemaVersion {
		return nil, planErrorf("unsupported plan format version %d", schemaVersion)
	}
	p.SchemaVersion = PlanSchemaVersion
	if p.CreatedBy, err = strField(obj, "plan", "createdBy"); err != nil {
		return nil, err
	}
	if p.CreatedAt, err = strField(obj, "plan", "createdAt"); err != nil {
		return nil, err
	}
	if _, err := time.Parse(time.RFC3339, p.CreatedAt); err != nil {
		return nil, planErrorf("plan createdAt is not RFC3339")
	}
	if p.FamilyID, err = strField(obj, "plan", "familyId"); err != nil {
		return nil, err
	}
	if !ValidFamilyID(p.FamilyID) {
		return nil, planErrorf("plan familyId %q is not a valid UUID", p.FamilyID)
	}

	srcObj, ok := obj["source"].(map[string]any)
	if !ok {
		return nil, planErrorf("plan source is not an object")
	}
	if err := strictKeys(srcObj, "plan source", "kind", "schema", "comparisonDigest", "path"); err != nil {
		return nil, err
	}
	if p.Source.Kind, err = strField(srcObj, "plan source", "kind"); err != nil {
		return nil, err
	}
	if p.Source.Kind != c.sourceKind {
		return nil, planErrorf("plan source kind %q is not %q", p.Source.Kind, c.sourceKind)
	}
	if p.Source.Schema, err = strField(srcObj, "plan source", "schema"); err != nil {
		return nil, err
	}
	if p.Source.Schema != c.sourceSchema {
		return nil, planErrorf("plan source schema %q is not %s", p.Source.Schema, c.sourceSchema)
	}
	if p.Source.ComparisonDigest, err = digestField(srcObj, "plan source", "comparisonDigest"); err != nil {
		return nil, err
	}
	if p.Source.Path, err = strField(srcObj, "plan source", "path"); err != nil {
		return nil, err
	}
	if !c.adoption {
		// §8.6: the source is the exact derived live.json of the store
		// directory.
		if !filepath.IsAbs(p.Source.Path) || filepath.Base(p.Source.Path) != LegacyFile {
			return nil, planErrorf("plan source path %q is not the derived canonical live.json path", p.Source.Path)
		}
	}
	if !filepath.IsAbs(p.Source.Path) {
		return nil, planErrorf("plan source path %q is not an absolute canonical path", p.Source.Path)
	}
	if filepath.Clean(p.Source.Path) != p.Source.Path {
		return nil, planErrorf("plan source path %q is not filepath.Clean-canonical", p.Source.Path)
	}
	backupObj, ok := obj["backup"].(map[string]any)
	if !ok {
		return nil, planErrorf("plan backup is not an object")
	}
	if err := strictKeys(backupObj, "plan backup", "format", "sourceSchema", "currentSchema", "payloadDigest", "semanticDigest", "comparisonDigest"); err != nil {
		return nil, err
	}
	format, err := intField(backupObj, "plan backup", "format")
	if err != nil {
		return nil, err
	}
	if format != ManifestSchemaVersion {
		return nil, planErrorf("plan backup format %d is not the backup format %d", format, ManifestSchemaVersion)
	}
	p.Backup.Format = int(format)
	if p.Backup.SourceSchema, err = strField(backupObj, "plan backup", "sourceSchema"); err != nil {
		return nil, err
	}
	if p.Backup.SourceSchema != c.sourceSchema {
		return nil, planErrorf("plan backup sourceSchema %q is not %s", p.Backup.SourceSchema, c.sourceSchema)
	}
	currentSchema, err := intField(backupObj, "plan backup", "currentSchema")
	if err != nil {
		return nil, err
	}
	if currentSchema != int64(orchestratorSchemaVersion) {
		return nil, planErrorf("plan backup currentSchema %d is not the legacy compatibility schema %d", currentSchema, orchestratorSchemaVersion)
	}
	p.Backup.CurrentSchema = int(currentSchema)
	if p.Backup.PayloadDigest, err = digestField(backupObj, "plan backup", "payloadDigest"); err != nil {
		return nil, err
	}
	if p.Backup.SemanticDigest, err = digestField(backupObj, "plan backup", "semanticDigest"); err != nil {
		return nil, err
	}
	if p.Backup.ComparisonDigest, err = digestField(backupObj, "plan backup", "comparisonDigest"); err != nil {
		return nil, err
	}

	invObj, ok := obj["inventory"].(map[string]any)
	if !ok {
		return nil, planErrorf("plan inventory is not an object")
	}
	inv, err := parseInventory(invObj)
	if err != nil {
		var be *BundleError
		if errors.As(err, &be) {
			return nil, planErrorf("plan inventory: %s", be.Reason)
		}
		return nil, planErrorf("plan inventory: %v", err)
	}
	p.Inventory = *inv
	// A legacy or adoption plan is content-neutral on the SQLite-only
	// classes: any nonzero count there contradicts the source the plan
	// proves.
	for class, count := range map[string]int{
		"assignments":   inv.Assignments,
		"receipts":      inv.Receipts,
		"plans":         inv.Plans,
		"planMappings":  inv.PlanMappings,
		"runs":          inv.Runs,
		"attempts":      inv.Attempts,
		"worktreeSagas": inv.WorktreeSagas,
	} {
		if count != 0 {
			return nil, planErrorf("plan inventory class %s is %d; a %s requires 0", class, count, c.what)
		}
	}
	// Cross-field proof: the source the plan verified and the bundle it was
	// verified against are the same logical legacy state.
	if p.Source.ComparisonDigest != p.Backup.ComparisonDigest {
		return nil, planErrorf("plan source comparison digest %s does not equal the backup comparison digest %s", p.Source.ComparisonDigest, p.Backup.ComparisonDigest)
	}

	targetObj, ok := obj["target"].(map[string]any)
	if !ok {
		return nil, planErrorf("plan target is not an object")
	}
	if err := strictKeys(targetObj, "plan target", "currentSchema", "path", "ownerBoundary"); err != nil {
		return nil, err
	}
	targetSchema, err := intField(targetObj, "plan target", "currentSchema")
	if err != nil {
		return nil, err
	}
	if targetSchema != int64(SchemaVersion) {
		return nil, planErrorf("plan target currentSchema %d is not the current schema %d", targetSchema, SchemaVersion)
	}
	p.Target.CurrentSchema = int(targetSchema)
	if p.Target.Path, err = strField(targetObj, "plan target", "path"); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(p.Target.Path) || filepath.Base(p.Target.Path) != StoreFile {
		return nil, planErrorf("plan target path %q is not the derived canonical tasks.sqlite path", p.Target.Path)
	}
	if filepath.Clean(p.Target.Path) != p.Target.Path {
		return nil, planErrorf("plan target path %q is not filepath.Clean-canonical", p.Target.Path)
	}
	if p.Target.OwnerBoundary, err = strField(targetObj, "plan target", "ownerBoundary"); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(p.Target.OwnerBoundary) {
		return nil, planErrorf("plan target ownerBoundary %q is not an absolute canonical path", p.Target.OwnerBoundary)
	}
	if filepath.Clean(p.Target.OwnerBoundary) != p.Target.OwnerBoundary {
		return nil, planErrorf("plan target ownerBoundary %q is not filepath.Clean-canonical", p.Target.OwnerBoundary)
	}
	// Derived identity agreement: the target is the exact SQLite authority of
	// one canonical <git-common-dir>/noise-tasks store directory, and the
	// family is its deterministic §8.5 derivation. A §8.6 plan additionally
	// proves its source is that directory's derived live.json; an §8.6A plan
	// proves its adopted source lives outside the store directory.
	storeDir := filepath.Dir(p.Target.Path)
	if !c.adoption && filepath.Dir(p.Source.Path) != storeDir {
		return nil, planErrorf("plan source and target paths are not derived from one store directory")
	}
	if c.adoption && (strings.HasPrefix(p.Source.Path, storeDir+string(filepath.Separator)) || p.Source.Path == storeDir) {
		return nil, planErrorf("plan source path %q is inside the store directory %s", p.Source.Path, storeDir)
	}
	// An adoption plan's source is operator-named, and the plan digest is
	// not authenticated content: containment inside the plan's own owner
	// boundary is proven here, not trusted from planning time.
	if c.adoption && p.Source.Path != p.Target.OwnerBoundary &&
		!strings.HasPrefix(p.Source.Path, p.Target.OwnerBoundary+string(filepath.Separator)) {
		return nil, planErrorf("plan source path %q is not inside the owner boundary %s", p.Source.Path, p.Target.OwnerBoundary)
	}
	if filepath.Base(storeDir) != "noise-tasks" || filepath.Clean(storeDir) != storeDir {
		return nil, planErrorf("plan store directory %q is not the canonical <git-common-dir>/noise-tasks directory", storeDir)
	}
	if want := LegacyFamilyUUID(filepath.Dir(storeDir), p.Target.OwnerBoundary); p.FamilyID != want {
		return nil, planErrorf("plan familyId %s is not the deterministic family %s of its own derived identity", p.FamilyID, want)
	}

	if p.PlanDigest, err = digestField(obj, "plan", "planDigest"); err != nil {
		return nil, err
	}
	digest, err := p.computePlanDigestWith(c)
	if err != nil {
		return nil, err
	}
	if digest != p.PlanDigest {
		return nil, planErrorf("plan digest mismatch: document says %s, recomputed %s", p.PlanDigest, digest)
	}
	return p, nil
}
