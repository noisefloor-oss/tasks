package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// §8.6 apply recovery journal: `migration-apply.json` in the store directory.
// The journal is coordination metadata, never task authority: it carries the
// plan, request, family, source-comparison, backup-payload, and target-schema
// digests and no phase or target-receipt field — recovery derives progress
// from source/target presence plus the target's embedded receipt.
const (
	// JournalSchemaVersion is the journal format this binary writes.
	JournalSchemaVersion = 1
	// JournalMaxBytes bounds the journal document.
	JournalMaxBytes = 16 << 10
)

// MigrationJournal is the strict §8.6 apply journal document. Every field is
// required and the encoding must be the canonical rendering.
type MigrationJournal struct {
	SchemaVersion          int    `json:"schemaVersion"`
	RequestID              string `json:"requestId"`
	FamilyID               string `json:"familyId"`
	PlanDigest             string `json:"planDigest"`
	SourceComparisonDigest string `json:"sourceComparisonDigest"`
	BackupPayloadDigest    string `json:"backupPayloadDigest"`
	TargetSchemaVersion    int    `json:"targetSchemaVersion"`
}

// journalValue renders the journal in the canonical value domain.
func (j *MigrationJournal) journalValue() map[string]any {
	return map[string]any{
		"schemaVersion":          int64(j.SchemaVersion),
		"requestId":              j.RequestID,
		"familyId":               j.FamilyID,
		"planDigest":             j.PlanDigest,
		"sourceComparisonDigest": j.SourceComparisonDigest,
		"backupPayloadDigest":    j.BackupPayloadDigest,
		"targetSchemaVersion":    int64(j.TargetSchemaVersion),
	}
}

// renderJournal produces the exact canonical journal bytes.
func (j *MigrationJournal) renderJournal() ([]byte, error) {
	body, err := canonicalJSON(j.journalValue())
	if err != nil {
		return nil, fmt.Errorf("render migration journal canonically: %w", err)
	}
	return body, nil
}

// ReadMigrationJournal loads and strictly validates the journal at path:
// a bounded, mode-0600, regular non-symlink file holding exactly the
// canonical encoding of the required field set with valid forms. A missing
// file returns (nil, os.ErrNotExist); any violation is a typed PlanError.
func ReadMigrationJournal(path string) (*MigrationJournal, error) {
	f, info, err := openRegularNoFollow(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, os.ErrNotExist
		}
		if err == errNotRegular {
			return nil, planErrorf("%s is not a regular file", path)
		}
		return nil, err
	}
	defer f.Close()
	if info.Mode().Perm() != 0o600 {
		return nil, planErrorf("%s mode %o is not 0600", path, info.Mode().Perm())
	}
	if info.Size() > JournalMaxBytes {
		return nil, planErrorf("%s exceeds %d bytes", path, JournalMaxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, JournalMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read migration journal %s: %w", path, err)
	}
	if len(data) > JournalMaxBytes {
		return nil, planErrorf("%s exceeds %d bytes", path, JournalMaxBytes)
	}
	obj, err := decodeJSONObject(data)
	if err != nil {
		return nil, planErrorf("%s is not strict JSON: %v", path, err)
	}
	j, err := validateJournal(obj)
	if err != nil {
		return nil, err
	}
	canon, err := j.renderJournal()
	if err != nil {
		return nil, err
	}
	if string(canon) != string(data) {
		return nil, planErrorf("%s is not the canonical journal encoding", path)
	}
	return j, nil
}

// validateJournal enforces the exact journal field set and forms.
func validateJournal(obj map[string]any) (*MigrationJournal, error) {
	for _, key := range []string{
		"schemaVersion", "requestId", "familyId", "planDigest",
		"sourceComparisonDigest", "backupPayloadDigest", "targetSchemaVersion",
	} {
		if _, ok := obj[key]; !ok {
			return nil, planErrorf("migration journal is missing required key %q", key)
		}
	}
	for key := range obj {
		switch key {
		case "schemaVersion", "requestId", "familyId", "planDigest",
			"sourceComparisonDigest", "backupPayloadDigest", "targetSchemaVersion":
		default:
			return nil, planErrorf("migration journal has unknown key %q", key)
		}
	}
	j := &MigrationJournal{}
	sv, ok := obj["schemaVersion"].(json.Number)
	if !ok {
		return nil, planErrorf("migration journal schemaVersion is not an integer")
	}
	n, err := sv.Int64()
	if err != nil || n != JournalSchemaVersion {
		return nil, planErrorf("unsupported migration journal schema version %v", obj["schemaVersion"])
	}
	j.SchemaVersion = JournalSchemaVersion
	str := func(key string) string { s, _ := obj[key].(string); return s }
	if j.RequestID = str("requestId"); !ValidLocalKey(j.RequestID) {
		return nil, planErrorf("migration journal requestId %q is not a valid local key", j.RequestID)
	}
	if j.FamilyID = str("familyId"); !ValidFamilyID(j.FamilyID) {
		return nil, planErrorf("migration journal familyId %q is not a valid UUID", j.FamilyID)
	}
	for key, dst := range map[string]*string{
		"planDigest":             &j.PlanDigest,
		"sourceComparisonDigest": &j.SourceComparisonDigest,
		"backupPayloadDigest":    &j.BackupPayloadDigest,
	} {
		if *dst = str(key); !digestForm(*dst) {
			return nil, planErrorf("migration journal key %q is not a lowercase SHA-256 digest", key)
		}
	}
	tsv, ok := obj["targetSchemaVersion"].(json.Number)
	if !ok {
		return nil, planErrorf("migration journal targetSchemaVersion is not an integer")
	}
	n, err = tsv.Int64()
	if err != nil || (n != int64(orchestratorSchemaVersion) && n != int64(SchemaVersion)) {
		return nil, planErrorf("migration journal targetSchemaVersion %v is not a supported recovery schema (%d or %d)", obj["targetSchemaVersion"], orchestratorSchemaVersion, SchemaVersion)
	}
	j.TargetSchemaVersion = int(n)
	return j, nil
}

// writeJournalTo atomically publishes one §8.6/§8.6A recovery journal at
// storeDir/name: a mode-0600 temporary with the given prefix in the store
// directory, file fsync, no-replace rename (a journal mismatch is never
// overwritten), and directory fsync.
func writeJournalTo(storeDir, name, tmpPrefix string, j *MigrationJournal) error {
	body, err := j.renderJournal()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(storeDir, tmpPrefix+"*")
	if err != nil {
		return fmt.Errorf("stage recovery journal in %s: %w", storeDir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod staged recovery journal: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write staged recovery journal: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync staged recovery journal: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close staged recovery journal: %w", err)
	}
	if err := renameNoReplace(tmpPath, filepath.Join(storeDir, name)); err != nil {
		return fmt.Errorf("publish recovery journal: %w", err)
	}
	return fsyncDir(storeDir)
}

// writeMigrationJournal atomically publishes the §8.6 journal.
func writeMigrationJournal(storeDir string, j *MigrationJournal) error {
	return writeJournalTo(storeDir, MigrationJournalFile, ".migration-apply-", j)
}

// MigrationRecovery classifies the §2/§8.7 fencing state of one store
// directory from the journal and the published database's presence.
type MigrationRecovery int

const (
	// RecoveryNone means no journal exists: no migration recovery state.
	RecoveryNone MigrationRecovery = iota
	// RecoveryRequired means either no database is published (§8.6 states
	// 2/3), or the journal names a pre-current target schema whose owning
	// binary must finish recovery. Normal commands report the recovery
	// diagnostic and only matching migrate apply may advance the state.
	RecoveryRequired
	// RecoveryPublished means a strict-valid journal exists alongside a
	// published database (§8.6 state 4): only matching migrate apply may
	// remove the journal; normal commands proceed against the database.
	RecoveryPublished
)

// ReadMigrationRecovery is the stable recovery diagnostic helper shared by
// the normal-command fencing (open, setup/init gates, context). A malformed
// journal is an error — it is never classified or bypassed.
func ReadMigrationRecovery(storeDir string) (MigrationRecovery, error) {
	journalPath := filepath.Join(storeDir, MigrationJournalFile)
	journal, err := ReadMigrationJournal(journalPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RecoveryNone, nil
		}
		return RecoveryNone, err
	}
	_ = journal
	presence := func(name, what string) (bool, error) {
		info, err := os.Lstat(filepath.Join(storeDir, name))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.Mode().IsRegular() {
			return false, planErrorf("migration journal is state-inconsistent: %s is not a regular file", what)
		}
		return true, nil
	}
	targetPresent, err := presence(StoreFile, "SQLite target")
	if err != nil {
		return RecoveryNone, err
	}
	sourcePresent, err := presence(LegacyFile, "legacy source")
	if err != nil {
		return RecoveryNone, err
	}
	if targetPresent {
		if sourcePresent {
			return RecoveryNone, planErrorf("migration journal is state-inconsistent: both legacy source and SQLite target exist")
		}
		if journal.TargetSchemaVersion < SchemaVersion {
			return RecoveryRequired, nil
		}
		return RecoveryPublished, nil
	}
	return RecoveryRequired, nil
}

// JournalBytes renders the canonical journal bytes of one record. It is the
// exact strict encoding writeMigrationJournal publishes and
// ReadMigrationJournal accepts.
func JournalBytes(j *MigrationJournal) ([]byte, error) { return j.renderJournal() }
