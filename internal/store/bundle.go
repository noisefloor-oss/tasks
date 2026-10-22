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
	"strings"
	"sync"
	"time"
)

// §8.5 backup bundle contract: one authority-neutral directory ending in
// .ntbackup containing exactly the regular, non-symlink files manifest.json
// and store.sqlite. The manifest is strict, bounded, and content-free.
const (
	// BundleSuffix names the backup bundle directory form.
	BundleSuffix = ".ntbackup"
	// ManifestFile is the bundle's manifest member.
	ManifestFile = "manifest.json"
	// PayloadFile is the bundle's SQLite payload member: one consistent
	// current authority, never a projection or WAL sidecar copy.
	PayloadFile = "store.sqlite"
	// ManifestMaxBytes bounds the manifest (SPEC §8.5).
	ManifestMaxBytes = 64 << 10
	// ManifestSchemaVersion is the backup format this binary writes.
	ManifestSchemaVersion = 1
	// SourceKindSQLite is a backup of the current SQLite authority.
	SourceKindSQLite = "sqlite"
	// SourceKindLegacyJSON is a backup of the exact legacy live.json (the
	// §8.6 migration compatibility surface).
	SourceKindLegacyJSON = "legacy-json"
)

// Manifest is the strict §8.5 backup manifest. Every field is required.
type Manifest struct {
	SchemaVersion           int            `json:"schemaVersion"`
	CreatedBy               string         `json:"createdBy"`
	CreatedAt               string         `json:"createdAt"`
	FamilyID                string         `json:"familyId"`
	SourceKind              string         `json:"sourceKind"`
	SourceSchema            string         `json:"sourceSchema"`
	CurrentSchemaVersion    int            `json:"currentSchemaVersion"`
	Inventory               ClassInventory `json:"inventory"`
	PayloadDigest           string         `json:"payloadDigest"`
	SemanticDigestVersion   int            `json:"semanticDigestVersion"`
	SemanticDigest          string         `json:"semanticDigest"`
	ComparisonDigestVersion int            `json:"comparisonDigestVersion"`
	ComparisonDigest        string         `json:"comparisonDigest"`
}

// fsyncFile durably flushes one file.
func fsyncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("fsync %s: %w", path, err)
	}
	return f.Close()
}

// fsyncDir durably flushes a directory's entries.
func fsyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return fmt.Errorf("fsync directory %s: %w", path, err)
	}
	return d.Close()
}

// ForeignKeyCheck runs PRAGMA foreign_key_check; any finding is an error.
func (d *DB) ForeignKeyCheck() error {
	rows, err := d.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign key check on %s: %w", d.path, err)
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("foreign key check on %s found violations", d.path)
	}
	return rows.Err()
}

// userVersion reports the database's recorded schema version.
func (d *DB) userVersion() (int, error) {
	var version int
	if err := d.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}

// publishBundle stages one §8.5 bundle directory next to outputPath and
// publishes it atomically. build renders the payload member at payloadPath
// (inside the staging directory) and returns the finished manifest; any
// error removes the staging directory and publishes nothing. Creation never
// overwrites an artifact.
func publishBundle(outputPath string, build func(payloadPath string) (*Manifest, error)) (*Manifest, error) {
	if !strings.HasSuffix(outputPath, BundleSuffix) {
		return nil, fmt.Errorf("backup output %s does not end in %s", outputPath, BundleSuffix)
	}
	parent := filepath.Dir(outputPath)
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return nil, fmt.Errorf("backup output parent %s: %w", parent, err)
	}
	if !parentInfo.IsDir() {
		return nil, fmt.Errorf("backup output parent %s is not a directory", parent)
	}
	if _, err := os.Lstat(outputPath); err == nil {
		return nil, fmt.Errorf("backup output %s already exists; creation never overwrites an artifact", outputPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// The temporary bundle lives in the output path's parent so publication is
	// a same-filesystem atomic directory rename; no cross-filesystem rename or
	// copy fallback is attempted (SPEC §8.5).
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(outputPath)+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary bundle in %s: %w", parent, err)
	}
	if err := os.Chmod(tmp, 0o700); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(tmp)
		}
	}()
	manifest, err := build(filepath.Join(tmp, PayloadFile))
	if err != nil {
		return nil, err
	}
	rawManifest, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if len(rawManifest) > ManifestMaxBytes {
		return nil, fmt.Errorf("manifest exceeds %d bytes", ManifestMaxBytes)
	}
	manifestPath := filepath.Join(tmp, ManifestFile)
	if err := os.WriteFile(manifestPath, rawManifest, 0o600); err != nil {
		return nil, fmt.Errorf("write manifest: %w", err)
	}
	// Enforce the contracted modes exactly (§8.5): bundle directory 0700,
	// manifest.json and store.sqlite 0600. Creation-time umask bits cannot
	// narrow them, so verification never refuses the binary's own bundle.
	if err := os.Chmod(manifestPath, 0o600); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Join(tmp, PayloadFile), 0o600); err != nil {
		return nil, err
	}
	if err := fsyncFile(filepath.Join(tmp, PayloadFile)); err != nil {
		return nil, err
	}
	if err := fsyncFile(manifestPath); err != nil {
		return nil, err
	}
	if err := fsyncDir(tmp); err != nil {
		return nil, err
	}
	// Publication is one atomic no-replace rename: if outputPath appears
	// between the pre-check above and this rename, the rename itself fails
	// and the pre-existing artifact (even an attacker-created empty
	// directory) is preserved, never replaced.
	if err := renameNoReplace(tmp, outputPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("backup output %s already exists; creation never overwrites an artifact", outputPath)
		}
		return nil, fmt.Errorf("publish backup bundle %s: %w", outputPath, err)
	}
	cleanup = false
	if err := fsyncDir(parent); err != nil {
		return nil, err
	}
	return manifest, nil
}

// CreateBackup snapshots the current authority into one §8.5 bundle at
// outputPath. The snapshot goes through VACUUM INTO — SQLite's backup
// interface — never a byte copy of the live main file, so the bundle is
// consistent under concurrent writers. createdBy is the creating binary's
// identity string ("noise-tasks <version>").
func (d *DB) CreateBackup(outputPath, createdBy string) (*Manifest, error) {
	return publishBundle(outputPath, func(payloadPath string) (*Manifest, error) {
		// VACUUM INTO snapshots as of its own read transaction, and the source
		// digest pass reads one deferred snapshot; a writer committing between
		// the two would make the comparison spuriously disagree, so retry the
		// pair until one quiet window agrees (SPEC §8.5: both paths run
		// inventory comparison and semantic digest comparison).
		var manifest *Manifest
		for attempt := 0; attempt < 20; attempt++ {
			os.Remove(payloadPath)
			if err := d.BackupTo(payloadPath); err != nil {
				return nil, err
			}
			payload, err := openUnlocked(payloadPath, false, "")
			if err != nil {
				return nil, err
			}
			manifest, err = d.checkBackupPayload(payload, createdBy)
			payload.Close()
			if err != nil {
				return nil, err
			}
			sourceSemantic, sourceComparison, sourceInv, err := d.ClassDigests()
			if err != nil {
				return nil, err
			}
			if sourceSemantic == manifest.SemanticDigest &&
				sourceComparison == manifest.ComparisonDigest &&
				*sourceInv == manifest.Inventory {
				return manifest, nil
			}
			if attempt == 19 {
				return nil, errors.New("task store kept changing during backup; retry when writers are quiet")
			}
			time.Sleep(25 * time.Millisecond)
		}
		return manifest, nil
	})
}

// checkBackupPayload validates a freshly snapshotted payload and derives its
// manifest fields: integrity and foreign-key checks, identity presence, and
// the versioned digests and inventory (SPEC §8.5).
func (d *DB) checkBackupPayload(payload *DB, createdBy string) (*Manifest, error) {
	if err := payload.IntegrityCheck(); err != nil {
		return nil, err
	}
	if err := payload.ForeignKeyCheck(); err != nil {
		return nil, err
	}
	version, err := payload.userVersion()
	if err != nil {
		return nil, err
	}
	if version < identitySchemaVersion || version > SchemaVersion {
		return nil, bundleErrorf("payload schema %d is not supported for backup", version)
	}
	if err := payload.validateSchemaVersion(version); err != nil {
		return nil, err
	}
	if version >= runSchemaVersion {
		if err := payload.ValidateRunState(); err != nil {
			return nil, err
		}
	}
	if version >= worktreeSagaSchemaVersion {
		if err := payload.ValidateWorktreeSagaState(); err != nil {
			return nil, err
		}
	}
	familyID, _, err := payload.Identity()
	if err != nil {
		return nil, err
	}
	if familyID == "" {
		return nil, errors.New("task store has no family identity; run any mutating noise-tasks command to upgrade it first")
	}
	semantic, comparison, inv, err := payload.ClassDigests()
	if err != nil {
		return nil, err
	}
	payloadDigest, err := hashFile(payload.Path())
	if err != nil {
		return nil, err
	}
	return &Manifest{
		SchemaVersion:           ManifestSchemaVersion,
		CreatedBy:               createdBy,
		CreatedAt:               time.Now().UTC().Format(time.RFC3339),
		FamilyID:                familyID,
		SourceKind:              SourceKindSQLite,
		SourceSchema:            "sqlite:" + strconv.Itoa(version),
		CurrentSchemaVersion:    version,
		Inventory:               *inv,
		PayloadDigest:           payloadDigest,
		SemanticDigestVersion:   SemanticDigestVersion,
		SemanticDigest:          semantic,
		ComparisonDigestVersion: ComparisonDigestVersion,
		ComparisonDigest:        comparison,
	}, nil
}

// hashFile computes the §8.5 payload digest of a path this process controls.
// Bundle members are hashed through their open handles via hashOpenFile.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return hashOpenFile(f)
}

// hashOpenFile computes the §8.5 payload digest of an already-open file:
// lowercase SHA-256 of the bytes read from the handle.
func hashOpenFile(f *os.File) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// schemaObject is one catalog entry of a SQLite database: its type and the
// exact SQL text that created it.
type schemaObject struct {
	typ   string
	table string
	sql   string
}

// sqliteSequenceSQL is the engine-generated AUTOINCREMENT bookkeeping table.
// Its rows are versioned content, not schema, so only its presence and shape
// are validated — never its contents.
const sqliteSequenceSQL = "CREATE TABLE sqlite_sequence(name,seq)"

// schemaObjectsByVersion returns the exact binary-owned catalogs admitted for
// released payload schemas. v2 through v4 are retained so old bundles can be
// verified context-free; v5 is the current schema. Each admits both fresh and
// v1-upgraded tasks-table SQL, whose record_extra column order differs.
var schemaObjectsByVersion = sync.OnceValues(func() (map[int]map[string][]schemaObject, error) {
	build := func(version int) (map[string][]schemaObject, error) {
		var freshDDL string
		switch version {
		case identitySchemaVersion:
			freshDDL = schemaV2DDL
		case orchestratorSchemaVersion:
			freshDDL = schemaV2DDL + orchestratorSchemaDDL
		case runSchemaVersion:
			freshDDL = schemaV2DDL + orchestratorSchemaDDL + runSchemaDDL
		case worktreeSagaSchemaVersion:
			freshDDL = schemaDDL
		default:
			return nil, fmt.Errorf("build schema-v%d reference: unsupported version", version)
		}
		fresh, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			return nil, err
		}
		defer fresh.Close()
		if _, err := fresh.Exec(freshDDL); err != nil {
			return nil, fmt.Errorf("build fresh schema-v%d reference: %w", version, err)
		}
		upgraded, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			return nil, err
		}
		defer upgraded.Close()
		steps := []string{schemaV2DDL, `ALTER TABLE tasks DROP COLUMN record_extra`, upgradeV2DDL}
		if version >= orchestratorSchemaVersion {
			steps = append(steps, upgradeV3DDL)
		}
		if version >= runSchemaVersion {
			steps = append(steps, upgradeV4DDL)
		}
		if version >= worktreeSagaSchemaVersion {
			steps = append(steps, upgradeV5DDL)
		}
		for _, q := range steps {
			if _, err := upgraded.Exec(q); err != nil {
				return nil, fmt.Errorf("build upgraded schema-v%d reference: %w", version, err)
			}
		}
		freshObjects, err := readSchemaObjects(fresh)
		if err != nil {
			return nil, err
		}
		upgradedObjects, err := readSchemaObjects(upgraded)
		if err != nil {
			return nil, err
		}
		expected := make(map[string][]schemaObject, len(freshObjects))
		for name, obj := range freshObjects {
			expected[name] = []schemaObject{obj}
			if variant, ok := upgradedObjects[name]; ok && variant != obj {
				expected[name] = append(expected[name], variant)
			}
		}
		return expected, nil
	}
	v2, err := build(identitySchemaVersion)
	if err != nil {
		return nil, err
	}
	v3, err := build(orchestratorSchemaVersion)
	if err != nil {
		return nil, err
	}
	v4, err := build(runSchemaVersion)
	if err != nil {
		return nil, err
	}
	v5, err := build(worktreeSagaSchemaVersion)
	if err != nil {
		return nil, err
	}
	return map[int]map[string][]schemaObject{
		identitySchemaVersion:     v2,
		orchestratorSchemaVersion: v3,
		runSchemaVersion:          v4,
		worktreeSagaSchemaVersion: v5,
	}, nil
})

// readSchemaObjects reads the sqlite_master catalog. sqlite_sequence is
// validated for shape and excluded: its rows are content. All other
// engine-internal entries (the sqlite_autoindex objects backing explicit
// keys) are ordinary catalog members and are returned.
func readSchemaObjects(db *sql.DB) (map[string]schemaObject, error) {
	rows, err := db.Query(`SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master`)
	if err != nil {
		return nil, fmt.Errorf("read schema catalog: %w", err)
	}
	defer rows.Close()
	objects := map[string]schemaObject{}
	for rows.Next() {
		var name string
		var obj schemaObject
		if err := rows.Scan(&obj.typ, &name, &obj.table, &obj.sql); err != nil {
			return nil, fmt.Errorf("read schema catalog: %w", err)
		}
		if name == "sqlite_sequence" {
			if obj.typ != "table" || obj.table != "sqlite_sequence" || obj.sql != sqliteSequenceSQL {
				return nil, bundleErrorf("payload sqlite_sequence schema is unexpected")
			}
			continue
		}
		objects[name] = obj
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema catalog: %w", err)
	}
	return objects, nil
}

// validateCurrentSchema rejects any payload catalog object the current
// released schema does not own. Data digests cannot see schema: a trigger
// hidden in the payload would fire when Restore appends its restore event
// after semantic validation, mutating task state under a matching digest.
// Unexpected or altered tables, indexes, triggers, and views are therefore
// refused before restore publishes anything (§8.5).
func (d *DB) validateSchemaVersion(version int) error {
	versions, err := schemaObjectsByVersion()
	if err != nil {
		return err
	}
	expected, ok := versions[version]
	if !ok {
		return bundleErrorf("payload schema %d is not supported", version)
	}
	got, err := readSchemaObjects(d.db)
	if err != nil {
		return err
	}
	for name, variants := range expected {
		obj, ok := got[name]
		if !ok {
			return bundleErrorf("payload schema is missing %s %q", variants[0].typ, name)
		}
		allowed := false
		for _, v := range variants {
			if obj == v {
				allowed = true
				break
			}
		}
		if !allowed {
			return bundleErrorf("payload %s %q does not match the current schema", obj.typ, name)
		}
	}
	for name, obj := range got {
		if _, ok := expected[name]; !ok {
			return bundleErrorf("payload carries unexpected %s %q", obj.typ, name)
		}
	}
	return nil
}

func (d *DB) validateCurrentSchema() error { return d.validateSchemaVersion(SchemaVersion) }

// BundleError reports a §8.5 verification refusal: corruption, unsupported
// format or schema, tampering, or a malformed bundle. It is exit 1 with empty
// stdout under --json.
type BundleError struct{ Reason string }

func (e *BundleError) Error() string { return "invalid backup bundle: " + e.Reason }

func bundleErrorf(format string, args ...any) *BundleError {
	return &BundleError{Reason: fmt.Sprintf(format, args...)}
}

// VerifyBundle is the context-free §8.5 verification pass: it checks and
// hashes the bundle through securely opened handles, never reopens a member
// by path, and cleans up its own scratch staging.
func VerifyBundle(bundlePath string) (*Manifest, error) {
	return verifyBundle(bundlePath, "")
}

// verifyBundle performs the §8.5 verification pass over securely opened
// handles. The staged copy is the single source of truth: the payload bytes
// are copied from the securely opened member to a private stage path while
// the §8.5 digest is computed over the exact bytes being copied in that
// same pass, then compared to the manifest — so no in-place writer can make
// a consumer read bytes not bound to manifest.PayloadDigest, and the
// semantic inspection below runs against that exact staged file. When
// stageTo is empty, staging uses an internal scratch directory this
// function creates and cleans up; when non-empty, the caller-owned stageTo
// path is used directly and the caller owns its cleanup. This is how
// Restore consumes the verified payload without ever reopening a bundle
// member by pathname.
func verifyBundle(bundlePath, stageTo string) (*Manifest, error) {
	// The bundle directory is opened once with O_NOFOLLOW|O_DIRECTORY; every
	// member is then opened relative to that handle with O_NOFOLLOW and
	// inspected, hashed, and copied through the same open file, so a pathname
	// swapped between checks cannot substitute different content (§8.5).
	dir, err := openDirNoFollow(bundlePath)
	if err != nil {
		return nil, bundleErrorf("cannot read %s: %v", bundlePath, err)
	}
	defer dir.Close()
	dirInfo, err := dir.Stat()
	if err != nil {
		return nil, bundleErrorf("cannot stat bundle: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		return nil, bundleErrorf("bundle directory mode is %04o, want 0700", dirInfo.Mode().Perm())
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, bundleErrorf("cannot list %s: %v", bundlePath, err)
	}
	if len(entries) != 2 {
		return nil, bundleErrorf("bundle must contain exactly %s and %s", ManifestFile, PayloadFile)
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if name != ManifestFile && name != PayloadFile {
			return nil, bundleErrorf("unexpected entry %q", name)
		}
		seen[name] = true
	}
	if !seen[ManifestFile] || !seen[PayloadFile] {
		return nil, bundleErrorf("bundle must contain exactly %s and %s", ManifestFile, PayloadFile)
	}

	manifestFile, manifestInfo, err := openMemberNoFollow(dir, ManifestFile)
	if err != nil {
		if errors.Is(err, errNotRegular) {
			return nil, bundleErrorf("entry %q is not a regular file", ManifestFile)
		}
		return nil, bundleErrorf("cannot open %q: %v", ManifestFile, err)
	}
	defer manifestFile.Close()
	if manifestInfo.Mode().Perm() != 0o600 {
		return nil, bundleErrorf("entry %q mode is %04o, want 0600", ManifestFile, manifestInfo.Mode().Perm())
	}
	manifestObj, err := readManifest(manifestFile)
	if err != nil {
		return nil, err
	}
	manifest, err := validateManifest(manifestObj)
	if err != nil {
		return nil, err
	}

	payloadFile, payloadInfo, err := openMemberNoFollow(dir, PayloadFile)
	if err != nil {
		if errors.Is(err, errNotRegular) {
			return nil, bundleErrorf("entry %q is not a regular file", PayloadFile)
		}
		return nil, bundleErrorf("cannot open %q: %v", PayloadFile, err)
	}
	defer payloadFile.Close()
	if payloadInfo.Mode().Perm() != 0o600 {
		return nil, bundleErrorf("entry %q mode is %04o, want 0600", PayloadFile, payloadInfo.Mode().Perm())
	}

	// Stage the payload in one pass from the securely opened member,
	// hashing the exact bytes as they are copied, then bind that digest to
	// the manifest. An in-place writer racing this copy can only produce a
	// digest mismatch, never unbound bytes in the staged file.
	stage := stageTo
	if stage == "" {
		scratch, err := os.MkdirTemp("", "noise-tasks-verify-*")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(scratch)
		stage = filepath.Join(scratch, PayloadFile)
	}
	if _, err := payloadFile.Seek(0, io.SeekStart); err != nil {
		return nil, bundleErrorf("cannot stage payload: %v", err)
	}
	stagedDigest, err := copyHashOpenFile(payloadFile, stage)
	if err != nil {
		return nil, bundleErrorf("cannot stage payload: %v", err)
	}
	if stagedDigest != manifest.PayloadDigest {
		return nil, bundleErrorf("payload digest mismatch")
	}

	// Semantically verify the exact staged file: verification never writes
	// sidecar files into the bundle and never upgrades it in place.
	payload, err := openUnlocked(stage, false, "")
	if err != nil {
		var newer *SchemaNewerError
		var notDB *NotSQLiteError
		switch {
		case errors.As(err, &newer):
			return nil, bundleErrorf("payload schema is newer than this binary")
		case errors.As(err, &notDB):
			return nil, bundleErrorf("payload is not a valid SQLite database")
		}
		return nil, err
	}
	defer payload.Close()
	version, err := payload.userVersion()
	if err != nil {
		return nil, bundleErrorf("cannot read payload schema version: %v", err)
	}
	if int64(version) != int64(manifest.CurrentSchemaVersion) {
		return nil, bundleErrorf("payload schema %d does not match manifest currentSchemaVersion %d", version, manifest.CurrentSchemaVersion)
	}
	if manifest.SourceKind == SourceKindLegacyJSON {
		// A legacy bundle's payload is a supported SQLite image; the
		// legacy sourceSchema names the JSON document, not the payload.
	} else if "sqlite:"+strconv.Itoa(version) != manifest.SourceSchema {
		return nil, bundleErrorf("payload schema %d does not match manifest source schema %s", version, manifest.SourceSchema)
	}
	if err := payload.IntegrityCheck(); err != nil {
		return nil, bundleErrorf("payload integrity check failed: %v", err)
	}
	if err := payload.ForeignKeyCheck(); err != nil {
		return nil, bundleErrorf("payload foreign key check failed: %v", err)
	}
	if err := payload.validateSchemaVersion(version); err != nil {
		return nil, err
	}
	if version >= runSchemaVersion {
		if err := payload.ValidateRunState(); err != nil {
			return nil, bundleErrorf("payload run state is invalid: %v", err)
		}
	}
	if version >= worktreeSagaSchemaVersion {
		if err := payload.ValidateWorktreeSagaState(); err != nil {
			return nil, bundleErrorf("payload worktree saga state is invalid: %v", err)
		}
	}
	familyID, ownerBoundary, err := payload.Identity()
	if err != nil {
		return nil, err
	}
	if familyID == "" || ownerBoundary == "" {
		return nil, bundleErrorf("payload carries no family identity")
	}
	if !ValidFamilyID(familyID) {
		return nil, bundleErrorf("payload family %q is not a valid UUID", familyID)
	}
	if familyID != manifest.FamilyID {
		return nil, bundleErrorf("payload family does not match manifest family")
	}
	semantic, comparison, inv, err := payload.ClassDigests()
	if err != nil {
		return nil, err
	}
	if semantic != manifest.SemanticDigest {
		return nil, bundleErrorf("semantic digest mismatch")
	}
	if comparison != manifest.ComparisonDigest {
		return nil, bundleErrorf("comparison digest mismatch")
	}
	if *inv != manifest.Inventory {
		return nil, bundleErrorf("inventory mismatch")
	}
	return manifest, nil
}

// copyFile copies src (by path) to dst; used for staging paths this process
// created and controls. Bundle members are copied through their already-open,
// verified handles via copyOpenFile instead.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return copyOpenFile(in, dst)
}

// copyOpenFile copies the already-open src to a fresh exclusive dst.
func copyOpenFile(src *os.File, dst string) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// copyHashOpenFile copies the already-open src to a fresh exclusive dst
// while hashing the exact bytes copied in the same pass, returning the
// §8.5 digest of the bytes that now live in dst.
func copyHashOpenFile(src *os.File, dst string) (string, error) {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), src); err != nil {
		out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// readManifest reads and strictly parses the bounded manifest from the
// already-open, verified manifest handle: at most 64 KiB, no duplicate keys
// at any level, top level an object.
func readManifest(f *os.File) (map[string]any, error) {
	data, err := io.ReadAll(io.LimitReader(f, ManifestMaxBytes+1))
	if err != nil {
		return nil, bundleErrorf("cannot read manifest: %v", err)
	}
	if len(data) > ManifestMaxBytes {
		return nil, bundleErrorf("manifest exceeds %d bytes", ManifestMaxBytes)
	}
	obj, err := decodeJSONObject(data)
	if err != nil {
		return nil, bundleErrorf("manifest is not strict JSON: %v", err)
	}
	return obj, nil
}

// validateManifest enforces the exact §8.5 field set and supported versions.
func validateManifest(obj map[string]any) (*Manifest, error) {
	required := []string{
		"schemaVersion", "createdBy", "createdAt", "familyId", "sourceKind",
		"sourceSchema", "currentSchemaVersion", "inventory", "payloadDigest",
		"semanticDigestVersion", "semanticDigest", "comparisonDigestVersion",
		"comparisonDigest",
	}
	for _, key := range required {
		if _, ok := obj[key]; !ok {
			return nil, bundleErrorf("manifest is missing required key %q", key)
		}
	}
	for key := range obj {
		known := false
		for _, k := range required {
			if key == k {
				known = true
				break
			}
		}
		if !known {
			return nil, bundleErrorf("manifest has unknown key %q", key)
		}
	}

	intField := func(key string) (int64, error) {
		v, ok := obj[key].(json.Number)
		if !ok {
			return 0, bundleErrorf("manifest key %q is not an integer", key)
		}
		n, err := v.Int64()
		if err != nil {
			return 0, bundleErrorf("manifest key %q is not an integer", key)
		}
		return n, nil
	}
	strField := func(key string) (string, error) {
		v, ok := obj[key].(string)
		if !ok || v == "" {
			return "", bundleErrorf("manifest key %q is not a nonempty string", key)
		}
		return v, nil
	}
	digestField := func(key string) (string, error) {
		v, err := strField(key)
		if err != nil {
			return "", err
		}
		if !digestForm(v) {
			return "", bundleErrorf("manifest key %q is not a lowercase SHA-256 digest", key)
		}
		return v, nil
	}

	m := &Manifest{}
	schemaVersion, err := intField("schemaVersion")
	if err != nil {
		return nil, err
	}
	if schemaVersion != ManifestSchemaVersion {
		return nil, bundleErrorf("unsupported backup format version %d", schemaVersion)
	}
	m.SchemaVersion = int(schemaVersion)
	if m.CreatedBy, err = strField("createdBy"); err != nil {
		return nil, err
	}
	if m.CreatedAt, err = strField("createdAt"); err != nil {
		return nil, err
	}
	if _, err := time.Parse(time.RFC3339, m.CreatedAt); err != nil {
		return nil, bundleErrorf("manifest createdAt is not RFC3339")
	}
	if m.FamilyID, err = strField("familyId"); err != nil {
		return nil, err
	}
	if !ValidFamilyID(m.FamilyID) {
		return nil, bundleErrorf("manifest familyId %q is not a valid UUID", m.FamilyID)
	}
	if m.SourceKind, err = strField("sourceKind"); err != nil {
		return nil, err
	}
	var sourceVersion int64
	switch m.SourceKind {
	case SourceKindSQLite:
		if m.SourceSchema, err = strField("sourceSchema"); err != nil {
			return nil, err
		}
		if sourceVersion, err = parseSourceSchema(m.SourceSchema); err != nil {
			return nil, err
		}
	case SourceKindLegacyJSON:
		if m.SourceSchema, err = strField("sourceSchema"); err != nil {
			return nil, err
		}
		if m.SourceSchema != LegacySourceSchema {
			return nil, bundleErrorf("legacy source schema %q is not %s", m.SourceSchema, LegacySourceSchema)
		}
		sourceVersion = 1
	default:
		return nil, bundleErrorf("unsupported source kind %q", m.SourceKind)
	}
	currentSchema, err := intField("currentSchemaVersion")
	if err != nil {
		return nil, err
	}
	// currentSchemaVersion names the payload schema this binary actually
	// writes (SPEC §8.5). Verify accepts the identity-bearing released
	// schemas whose exact catalogs and semantic forms remain supported.
	if currentSchema < identitySchemaVersion || currentSchema > int64(SchemaVersion) {
		return nil, bundleErrorf("manifest currentSchemaVersion %d is not a supported payload schema (%d through %d)", currentSchema, identitySchemaVersion, SchemaVersion)
	}
	if sourceVersion > int64(SchemaVersion) {
		return nil, bundleErrorf("unsupported schema %s (binary supports sqlite:%d)", m.SourceSchema, SchemaVersion)
	}
	m.CurrentSchemaVersion = int(currentSchema)
	if m.PayloadDigest, err = digestField("payloadDigest"); err != nil {
		return nil, err
	}
	semanticVersion, err := intField("semanticDigestVersion")
	if err != nil {
		return nil, err
	}
	if semanticVersion != SemanticDigestVersion {
		return nil, bundleErrorf("unsupported semantic digest version %d", semanticVersion)
	}
	m.SemanticDigestVersion = int(semanticVersion)
	if m.SemanticDigest, err = digestField("semanticDigest"); err != nil {
		return nil, err
	}
	comparisonVersion, err := intField("comparisonDigestVersion")
	if err != nil {
		return nil, err
	}
	if comparisonVersion != ComparisonDigestVersion {
		return nil, bundleErrorf("unsupported comparison digest version %d", comparisonVersion)
	}
	m.ComparisonDigestVersion = int(comparisonVersion)
	if m.ComparisonDigest, err = digestField("comparisonDigest"); err != nil {
		return nil, err
	}
	invObj, ok := obj["inventory"].(map[string]any)
	if !ok {
		return nil, bundleErrorf("manifest inventory is not an object")
	}
	inv, err := parseInventory(invObj)
	if err != nil {
		return nil, err
	}
	m.Inventory = *inv
	return m, nil
}

func digestForm(v string) bool {
	if !strings.HasPrefix(v, "sha256:") || len(v) != len("sha256:")+64 {
		return false
	}
	for _, c := range v[len("sha256:"):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func parseSourceSchema(v string) (int64, error) {
	rest, ok := strings.CutPrefix(v, "sqlite:")
	if !ok {
		return 0, bundleErrorf("unsupported source schema %q", v)
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || n < 0 {
		return 0, bundleErrorf("unsupported source schema %q", v)
	}
	return n, nil
}

// parseInventory enforces the exact §8.5 inventory counter set, each a
// nonnegative integer.
func parseInventory(obj map[string]any) (*ClassInventory, error) {
	counter := func(key string) (int, error) {
		raw, ok := obj[key]
		if !ok {
			return 0, bundleErrorf("inventory is missing counter %q", key)
		}
		n, ok := raw.(json.Number)
		if !ok {
			return 0, bundleErrorf("inventory counter %q is not an integer", key)
		}
		v, err := n.Int64()
		if err != nil || v < 0 {
			return 0, bundleErrorf("inventory counter %q is not a nonnegative integer", key)
		}
		return int(v), nil
	}
	inv := &ClassInventory{}
	keys := map[string]*int{
		"tags": &inv.Tags, "milestones": &inv.Milestones, "tasks": &inv.Tasks,
		"dependencies": &inv.Dependencies, "subtasks": &inv.Subtasks,
		"bindings": &inv.Bindings, "assignments": &inv.Assignments,
		"claims": &inv.Claims, "events": &inv.Events, "receipts": &inv.Receipts,
		"plans": &inv.Plans, "planMappings": &inv.PlanMappings, "runs": &inv.Runs,
		"attempts": &inv.Attempts, "worktreeSagas": &inv.WorktreeSagas,
	}
	for key := range obj {
		if _, ok := keys[key]; !ok {
			return nil, bundleErrorf("inventory has unknown counter %q", key)
		}
	}
	for key, dst := range keys {
		v, err := counter(key)
		if err != nil {
			return nil, err
		}
		*dst = v
	}
	return inv, nil
}
