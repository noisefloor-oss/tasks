package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBackupCreateVerifyRestoreCLI drives the §8.5 surface end to end:
// create, context-free verify, offline overwrite restore, and replay.
func TestBackupCreateVerifyRestoreCLI(t *testing.T) {
	dir := chdirGitDir(t)
	if code, _, stderr := runCmd(cmdInit); code != 0 {
		t.Fatalf("init exit %d: %s", code, stderr)
	}
	if code, _, stderr := runCmd(cmdAdd, "--title", "one"); code != 0 {
		t.Fatalf("add exit %d: %s", code, stderr)
	}
	if code, _, stderr := runCmd(cmdAdd, "--title", "two", "--priority", "high"); code != 0 {
		t.Fatalf("add exit %d: %s", code, stderr)
	}

	bundle := filepath.Join(dir, "snap.ntbackup")
	code, stdout, stderr := runCmd(cmdBackup, "create", "--output", bundle, "--json")
	if code != 0 {
		t.Fatalf("backup create exit %d: %s", code, stderr)
	}
	var manifest map[string]any
	if err := json.Unmarshal([]byte(stdout), &manifest); err != nil {
		t.Fatalf("create --json output: %v (%q)", err, stdout)
	}
	for _, key := range []string{"schemaVersion", "createdBy", "createdAt", "familyId", "sourceKind",
		"sourceSchema", "currentSchemaVersion", "inventory", "payloadDigest",
		"semanticDigestVersion", "semanticDigest", "comparisonDigestVersion", "comparisonDigest"} {
		if _, ok := manifest[key]; !ok {
			t.Fatalf("manifest missing %q: %v", key, manifest)
		}
	}
	if manifest["sourceKind"] != "sqlite" || manifest["sourceSchema"] != "sqlite:6" {
		t.Fatalf("unexpected source fields: %v", manifest)
	}

	// Verify is context-free: it works from an unrelated directory.
	orig, _ := os.Getwd()
	os.Chdir(t.TempDir())
	code, stdout, stderr = runCmd(cmdBackup, "verify", "--json", bundle)
	os.Chdir(orig)
	if code != 0 {
		t.Fatalf("backup verify exit %d: %s", code, stderr)
	}
	var verifyOut struct {
		Valid    bool           `json:"valid"`
		Manifest map[string]any `json:"manifest"`
	}
	if err := json.Unmarshal([]byte(stdout), &verifyOut); err != nil {
		t.Fatalf("verify --json output: %v", err)
	}
	if !verifyOut.Valid || verifyOut.Manifest["familyId"] != manifest["familyId"] {
		t.Fatalf("unexpected verify output: %v", verifyOut)
	}

	// Restore requires the offline acknowledgment and request id.
	if code, _, _ := runCmd(cmdBackup, "restore", "--request-id", "r1", bundle); code != 2 {
		t.Fatalf("restore without --offline exit %d, want 2", code)
	}
	if code, _, _ := runCmd(cmdBackup, "restore", "--offline", bundle); code != 2 {
		t.Fatalf("restore without --request-id exit %d, want 2", code)
	}
	// An existing target refuses without --overwrite.
	code, stdout, stderr = runCmd(cmdBackup, "restore", "--offline", "--request-id", "r1", bundle)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "--overwrite") {
		t.Fatalf("restore refusal: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// Same family and boundary: --overwrite authorizes.
	code, stdout, stderr = runCmd(cmdBackup, "restore", "--overwrite", "--offline", "--request-id", "r1", "--json", bundle)
	if code != 0 {
		t.Fatalf("restore exit %d: %s", code, stderr)
	}
	var restoreOut struct {
		Receipt  map[string]any `json:"receipt"`
		Replayed bool           `json:"replayed"`
		Adopted  bool           `json:"adopted"`
	}
	if err := json.Unmarshal([]byte(stdout), &restoreOut); err != nil {
		t.Fatalf("restore --json output: %v (%q)", err, stdout)
	}
	if restoreOut.Replayed || restoreOut.Adopted {
		t.Fatalf("unexpected restore flags: %+v", restoreOut)
	}
	if restoreOut.Receipt["kind"] != "restore" || restoreOut.Receipt["requestId"] != "r1" ||
		restoreOut.Receipt["familyId"] != manifest["familyId"] {
		t.Fatalf("unexpected receipt: %v", restoreOut.Receipt)
	}

	// Re-running the same request and bundle returns the original receipt.
	code, stdout, stderr = runCmd(cmdBackup, "restore", "--overwrite", "--offline", "--request-id", "r1", "--json", bundle)
	if code != 0 {
		t.Fatalf("replay restore exit %d: %s", code, stderr)
	}
	var replayOut struct {
		Receipt  map[string]any `json:"receipt"`
		Replayed bool           `json:"replayed"`
	}
	if err := json.Unmarshal([]byte(stdout), &replayOut); err != nil {
		t.Fatalf("replay --json output: %v", err)
	}
	if !replayOut.Replayed || replayOut.Receipt["receiptId"] != restoreOut.Receipt["receiptId"] {
		t.Fatalf("expected replay of %v, got %+v", restoreOut.Receipt["receiptId"], replayOut)
	}

	// The restored store still serves reads.
	code, stdout, stderr = runCmd(cmdList, "--json")
	if code != 0 {
		t.Fatalf("list after restore exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, `"one"`) || !strings.Contains(stdout, `"two"`) {
		t.Fatalf("restored store lost tasks: %q", stdout)
	}
}

func TestBackupCreateRefusalsCLI(t *testing.T) {
	chdirGitDir(t)
	// Uninitialized project: exact §2 diagnostic, exit 1, empty stdout.
	code, stdout, stderr := runCmd(cmdBackup, "create", "--output", "x.ntbackup")
	if code != 1 || stdout != "" ||
		!strings.Contains(stderr, "project is not initialized; run noise tasks setup") {
		t.Fatalf("create on uninitialized project: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if code, _, _ := runCmd(cmdBackup, "create"); code != 2 {
		t.Fatalf("create without --output exit %d, want 2", code)
	}
	// --legacy without a legacy live store at the exact derived path is a
	// fail-closed refusal (exit 1), not a usage error.
	code, stdout, stderr = runCmd(cmdBackup, "create", "--output", "x.ntbackup", "--legacy")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "live.json") {
		t.Fatalf("missing legacy store: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if code, _, _ := runCmd(cmdBackup, "explode"); code != 2 {
		t.Fatalf("unknown subcommand exit %d, want 2", code)
	}
}

func TestBackupVerifyCorruptionCLI(t *testing.T) {
	dir := chdirGitDir(t)
	if code, _, stderr := runCmd(cmdInit); code != 0 {
		t.Fatalf("init exit %d: %s", code, stderr)
	}
	if code, _, stderr := runCmd(cmdAdd, "--title", "one"); code != 0 {
		t.Fatalf("add exit %d: %s", code, stderr)
	}
	bundle := filepath.Join(dir, "snap.ntbackup")
	if code, _, stderr := runCmd(cmdBackup, "create", "--output", bundle); code != 0 {
		t.Fatalf("create exit %d: %s", code, stderr)
	}
	payload := filepath.Join(bundle, "store.sqlite")
	data, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(payload, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// Corruption: exit 1 with empty stdout even under --json.
	code, stdout, _ := runCmd(cmdBackup, "verify", "--json", bundle)
	if code != 1 || stdout != "" {
		t.Fatalf("corrupt verify: exit %d stdout %q", code, stdout)
	}
	// Restore of a corrupt bundle refuses before touching the target.
	code, stdout, _ = runCmd(cmdBackup, "restore", "--overwrite", "--offline", "--request-id", "r9", bundle)
	if code != 1 || stdout != "" {
		t.Fatalf("corrupt restore: exit %d stdout %q", code, stdout)
	}
}

// TestBackupRestoreAdoptionCLI covers the recovery case: no target exists and
// the bundle's family is adopted onto the matching derived owner boundary,
// with the recovery fact named in output.
func TestBackupRestoreAdoptionCLI(t *testing.T) {
	dir := chdirGitDir(t)
	if code, _, stderr := runCmd(cmdInit); code != 0 {
		t.Fatalf("init exit %d: %s", code, stderr)
	}
	if code, _, stderr := runCmd(cmdAdd, "--title", "one"); code != 0 {
		t.Fatalf("add exit %d: %s", code, stderr)
	}
	bundle := filepath.Join(dir, "snap.ntbackup")
	if code, _, stderr := runCmd(cmdBackup, "create", "--output", bundle, "--json"); code != 0 {
		t.Fatalf("create exit %d: %s", code, stderr)
	}
	target := projectStorePath(dir)
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCmd(cmdBackup, "restore", "--offline", "--request-id", "adopt-1", bundle)
	if code != 0 {
		t.Fatalf("adopt restore exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "adopted:") || !strings.Contains(stdout, "restored:") {
		t.Fatalf("adoption output should name the recovery fact: %q", stdout)
	}
	code, stdout, stderr = runCmd(cmdList, "--json")
	if code != 0 || !strings.Contains(stdout, `"one"`) {
		t.Fatalf("list after adoption: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// TestBackupLegacyCreateCLI drives `backup create --legacy` end to end from
// the exact derived live.json: bundle creation without any SQLite authority,
// context-free verify of the legacy manifest, and no publication or mutation
// of the legacy source.
func TestBackupLegacyCreateCLI(t *testing.T) {
	dir := chdirGitDir(t)
	storeDir := filepath.Join(dir, ".git", "noise-tasks")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(storeDir, "live.json")
	document := `{"schemaVersion":1,"fencing":0,"nextSeq":1,"base":{"hash":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","tags":{}},"tags":{"main":{"nextId":2,"taskCount":1,"completedCount":0,"tasks":[{"task":{"id":1,"title":"Legacy","description":"d","details":"","status":"pending","dependencies":[],"updatedAt":"2026-01-01T00:00:00Z"},"revision":1}]}},"events":[]}`
	if err := os.WriteFile(legacyPath, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "legacy.ntbackup")
	code, stdout, stderr := runCmd(cmdBackup, "create", "--legacy", "--output", bundle, "--json")
	if code != 0 {
		t.Fatalf("legacy create exit %d: %s", code, stderr)
	}
	var manifest struct {
		FamilyID      string `json:"familyId"`
		SourceKind    string `json:"sourceKind"`
		SourceSchema  string `json:"sourceSchema"`
		CurrentSchema int    `json:"currentSchemaVersion"`
	}
	if err := json.Unmarshal([]byte(stdout), &manifest); err != nil {
		t.Fatalf("decode legacy manifest %q: %v", stdout, err)
	}
	if manifest.SourceKind != "legacy-json" || manifest.SourceSchema != "legacy-json:1" || manifest.CurrentSchema != 3 {
		t.Fatalf("legacy manifest: %+v", manifest)
	}
	// No authority was published and the legacy source is untouched.
	if _, err := os.Lstat(filepath.Join(storeDir, "tasks.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("tasks.sqlite must not be published: %v", err)
	}
	after, err := os.ReadFile(legacyPath)
	if err != nil || string(after) != document {
		t.Fatalf("legacy live.json mutated: %v", err)
	}
	// Context-free verify accepts the legacy bundle.
	code, stdout, stderr = runCmd(cmdBackup, "verify", "--json", bundle)
	if code != 0 {
		t.Fatalf("legacy verify exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, `"legacy-json"`) || !strings.Contains(stdout, manifest.FamilyID) {
		t.Fatalf("legacy verify output: %q", stdout)
	}
	// A second backup of the same family is stable.
	bundle2 := filepath.Join(dir, "legacy2.ntbackup")
	code, _, stderr = runCmd(cmdBackup, "create", "--legacy", "--output", bundle2)
	if code != 0 {
		t.Fatalf("second legacy create exit %d: %s", code, stderr)
	}
	code, stdout, _ = runCmd(cmdBackup, "verify", "--json", bundle2)
	if code != 0 || !strings.Contains(stdout, manifest.FamilyID) {
		t.Fatalf("second verify: exit %d stdout %q", code, stdout)
	}
}
