package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/preferences"
	"github.com/noisefloor-oss/tasks/internal/store"
)

func setupProject(t *testing.T) string {
	t.Helper()
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOISE_PROJECT_DIR", project)
	return project
}

// projectStorePath is the §8.1 database path for a plain .git-dir project.
func projectStorePath(project string) string {
	return filepath.Join(project, ".git", "noise-tasks", "tasks.sqlite")
}

func runSetup(stdin string, terminal bool, args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = setupMain(args, strings.NewReader(stdin), &out, &errOut, terminal)
	return code, out.String(), errOut.String()
}

func decodeSetupResult(t *testing.T, stdout string) setupResult {
	t.Helper()
	var result setupResult
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil {
		t.Fatalf("decode setup JSON: %v\n%s", err, stdout)
	}
	return result
}

// storeDigest renders the database's §8.6 semantic digest for comparisons.
func storeDigest(t *testing.T, path string) string {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	snap, err := db.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	digest, err := snap.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return digest
}

func TestSetupFirstCreateAndIdempotentJSON(t *testing.T) {
	project := setupProject(t)
	args := []string{"--adapter", "anthropic-api", "--model", "claude-fable-5", "--yes", "--json"}
	code, stdout, stderr := runSetup("", false, args...)
	if code != 0 || stderr != "" {
		t.Fatalf("first setup = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	result := decodeSetupResult(t, stdout)
	if result.SchemaVersion != 2 || result.ProjectBoundary != project ||
		result.StorePath != projectStorePath(project) || result.PreferencesPath != ".tasks/preferences.json" ||
		result.StoreAction != "created" || result.PreferencesAction != "created" ||
		result.Generation.Adapter != preferences.AdapterAnthropicAPI || result.Generation.Model != "claude-fable-5" ||
		!result.AdapterAvailable || result.AuthenticationVerified {
		t.Fatalf("first result = %+v", result)
	}
	if _, err := store.Open(result.StorePath); err != nil {
		t.Fatalf("created store: %v", err)
	}
	snapshot, err := preferences.Load(project)
	if err != nil || !snapshot.Exists || snapshot.Preferences.Generation != result.Generation {
		t.Fatalf("preferences snapshot = %+v, %v", snapshot, err)
	}
	digestBefore := storeDigest(t, result.StorePath)
	prefsBefore := bytes.Clone(snapshot.Raw)

	code, stdout, stderr = runSetup("", false, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("rerun = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	result = decodeSetupResult(t, stdout)
	if result.StoreAction != "unchanged" || result.PreferencesAction != "unchanged" {
		t.Fatalf("rerun actions = %+v", result)
	}
	snapshot, _ = preferences.Load(project)
	if storeDigest(t, result.StorePath) != digestBefore || !bytes.Equal(prefsBefore, snapshot.Raw) {
		t.Fatal("idempotent setup rewrote project state")
	}
}

func TestSetupNoninteractiveRequiresSelectorsAndConfirmation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing selectors", args: []string{"--json"}, want: "--adapter and --model are required"},
		{name: "yes has no defaults", args: []string{"--yes", "--json"}, want: "--adapter and --model are required"},
		{name: "adapter only", args: []string{"--adapter", "anthropic-api", "--json"}, want: "must be supplied together"},
		{name: "empty pair", args: []string{"--adapter", "", "--model", "", "--yes"}, want: "nonempty values"},
		{name: "needs yes", args: []string{"--adapter", "anthropic-api", "--model", "m", "--json"}, want: "requires --yes"},
		{name: "invalid model", args: []string{"--adapter", "anthropic-api", "--model", "--flag", "--yes"}, want: "must not begin"},
		{name: "positional", args: []string{"extra"}, want: "unexpected argument"},
		{name: "file flag", args: []string{"--file", "tasks.json"}, want: "flag provided but not defined"},
		{name: "tag flag", args: []string{"--tag", "main"}, want: "flag provided but not defined"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			project := setupProject(t)
			code, stdout, stderr := runSetup("", false, test.args...)
			if code != 2 || stdout != "" || !strings.Contains(stderr, test.want) {
				t.Fatalf("setup = code %d stdout %q stderr %q", code, stdout, stderr)
			}
			if _, err := os.Lstat(projectStorePath(project)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refusal created state: %v", err)
			}
		})
	}
}

func TestSetupInteractivePromptCancelAndConfirm(t *testing.T) {
	project := setupProject(t)
	code, stdout, stderr := runSetup("1\nmodel-a\nn\n", true)
	if code != 0 || stdout != "" || !strings.Contains(stderr, "available generation adapters") ||
		!strings.Contains(stderr, "adapter: anthropic-api") || !strings.Contains(stderr, "cancelled") {
		t.Fatalf("cancel = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := os.Lstat(projectStorePath(project)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancel created state: %v", err)
	}

	code, stdout, stderr = runSetup("1\nmodel-a\nyes\n", true)
	if code != 0 || !strings.Contains(stdout, "authentication is unverified") ||
		!strings.Contains(stdout, "store .git/noise-tasks/tasks.sqlite: created") ||
		!strings.Contains(stderr, "write these generation preferences") {
		t.Fatalf("confirm = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestSetupInteractiveYesSkipsOnlyConfirmation(t *testing.T) {
	setupProject(t)
	code, stdout, stderr := runSetup("", true,
		"--adapter", "anthropic-api", "--model", "model-a", "--yes")
	if code != 0 || !strings.Contains(stdout, "generation: adapter=anthropic-api model=model-a") ||
		!strings.Contains(stderr, "requested generation preferences") ||
		strings.Contains(stderr, "write these generation preferences?") {
		t.Fatalf("setup --yes = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestSetupCodexAndKimi(t *testing.T) {
	for _, adapter := range []string{preferences.AdapterCodex, preferences.AdapterKimi} {
		for _, interactive := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/interactive=%t", adapter, interactive), func(t *testing.T) {
				project := setupProject(t)
				bin := t.TempDir()
				if err := os.WriteFile(filepath.Join(bin, adapter), []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin)
				args := []string{"--adapter", adapter, "--model", "provider/model", "--yes", "--json"}
				stdin := ""
				if interactive {
					args = nil
					stdin = "2\nprovider/model\nyes\n"
				}
				code, stdout, stderr := runSetup(stdin, interactive, args...)
				if code != 0 {
					t.Fatalf("setup = code %d stdout %q stderr %q", code, stdout, stderr)
				}
				snapshot, err := preferences.Load(project)
				if err != nil || !snapshot.Exists ||
					snapshot.Preferences.Generation != (preferences.Generation{Adapter: adapter, Model: "provider/model"}) {
					t.Fatalf("preferences = %+v, %v", snapshot, err)
				}
				if interactive && !strings.Contains(stderr, "2) "+adapter) {
					t.Fatalf("adapter missing from prompt: %q", stderr)
				}
			})
		}
	}
}

func TestSetupInteractiveRejectsPaddedModel(t *testing.T) {
	project := setupProject(t)
	code, stdout, stderr := runSetup("1\n model-a \n", true)
	if code != 2 || stdout != "" || !strings.Contains(stderr, "must be nonempty and trimmed") {
		t.Fatalf("padded model = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := os.Lstat(projectStorePath(project)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid prompt created state: %v", err)
	}
}

func TestSetupReplacementRequiresConfirmationAndPreservesStore(t *testing.T) {
	project := setupProject(t)
	if code, _, stderr := runSetup("", false, "--adapter", "anthropic-api", "--model", "a", "--yes", "--json"); code != 0 {
		t.Fatal(stderr)
	}
	storePath := projectStorePath(project)
	before := storeDigest(t, storePath)

	code, stdout, stderr := runSetup("", false, "--adapter", "anthropic-api", "--model", "b", "--json")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "requires --yes") {
		t.Fatalf("unconfirmed replacement = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	snapshot, _ := preferences.Load(project)
	if snapshot.Preferences.Generation.Model != "a" {
		t.Fatal("unconfirmed replacement changed preferences")
	}

	code, stdout, stderr = runSetup("", false, "--adapter", "anthropic-api", "--model", "b", "--yes", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("replacement = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	result := decodeSetupResult(t, stdout)
	if result.StoreAction != "unchanged" || result.PreferencesAction != "updated" {
		t.Fatalf("replacement result = %+v", result)
	}
	if storeDigest(t, storePath) != before {
		t.Fatal("replacement rewrote the task store")
	}
}

func TestSetupRepairsStoreWhileUnavailablePreferenceIsKept(t *testing.T) {
	project := setupProject(t)
	prefs := preferences.Preferences{
		SchemaVersion: preferences.SchemaVersion,
		Generation:    preferences.Generation{Adapter: preferences.AdapterOMP, Model: "zai/glm-5.2"},
	}
	if err := preferences.Publish(project, prefs); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := preferences.Load(project)
	t.Setenv("PATH", t.TempDir())

	code, stdout, stderr := runSetup("", false, "--json")
	if code != 0 || !strings.Contains(stderr, "not currently available") {
		t.Fatalf("repair = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	result := decodeSetupResult(t, stdout)
	if result.StoreAction != "created" || result.PreferencesAction != "unchanged" || result.AdapterAvailable {
		t.Fatalf("repair result = %+v", result)
	}
	after, _ := preferences.Load(project)
	if !bytes.Equal(snapshot.Raw, after.Raw) {
		t.Fatal("repair rewrote kept preferences")
	}
}

func TestSetupIgnoresNestedRetiredLayout(t *testing.T) {
	project := setupProject(t)
	start := filepath.Join(project, "sub", "pkg")
	if err := os.MkdirAll(start, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOISE_PROJECT_DIR", start)
	// A retired-layout file below the boundary is inert leftover state:
	// discovery never selects it and setup's stat-only refusal covers only
	// the fixed boundary paths (§2).
	nested := filepath.Join(project, "sub", ".tasks", "tasks", "tasks.json")
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte(`{"main":{"tasks":[]}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runSetup("", false, "--adapter", "anthropic-api", "--model", "m", "--yes", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("nested setup = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	result := decodeSetupResult(t, stdout)
	if result.StorePath != projectStorePath(project) || result.StoreAction != "created" {
		t.Fatalf("nested result = %+v", result)
	}
	got, _ := os.ReadFile(nested)
	if string(got) != `{"main":{"tasks":[]}}` {
		t.Fatal("setup touched nested retired-layout file")
	}
	if snapshot, err := preferences.Load(project); err != nil || !snapshot.Exists {
		t.Fatalf("boundary preferences = %+v, %v", snapshot, err)
	}
}

func TestSetupRefusesRetiredLayoutsWithoutCreatingState(t *testing.T) {
	tests := []struct {
		name string
		path []string
		want string
	}{
		{name: "canonical", path: []string{".tasks", "tasks", "tasks.json"}, want: "pre-cutover release"},
		{name: "taskmaster", path: []string{".taskmaster", "tasks", "tasks.json"}, want: "pre-cutover release"},
		{name: "noisetask", path: []string{".noisetask", "tasks.json"}, want: "migration contract is not available"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			project := setupProject(t)
			path := filepath.Join(append([]string{project}, test.path...)...)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(`{"main":{"tasks":[]}}`), 0o644); err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := runSetup("", false, "--adapter", "anthropic-api", "--model", "m", "--yes", "--json")
			if code != 1 || stdout != "" || !strings.Contains(stderr, test.want) {
				t.Fatalf("refusal = code %d stdout %q stderr %q", code, stdout, stderr)
			}
			if _, err := os.Lstat(projectStorePath(project)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("retired-layout refusal created the store: %v", err)
			}
		})
	}
}

func TestSetupRefusesLegacyJSONLiveStore(t *testing.T) {
	project := setupProject(t)
	legacy := filepath.Join(project, ".git", "noise-tasks", "live.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runSetup("", false, "--adapter", "anthropic-api", "--model", "m", "--yes", "--json")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "legacy live store found; create a verified backup and run noise tasks migrate plan") {
		t.Fatalf("legacy refusal = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := os.Lstat(projectStorePath(project)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy refusal created the store: %v", err)
	}
}

// TestSetupHistoricMigrationArtifactsAreInert pins §8.7: the historic
// sqlite-migration.json receipts (including quarantine/finalize forms and
// the retired boundary-level .noise-tasks-migration.json) are inert — setup
// proceeds over them; they never select or replace a database.
func TestSetupHistoricMigrationArtifactsAreInert(t *testing.T) {
	project := setupProject(t)
	storeDir := filepath.Join(project, ".git", "noise-tasks")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	receipt := `{"version":1,"binary":"test","schemaVersion":1,"phase":"migrating","sourceDigest":"` + strings.Repeat("0", 64) + `","inventory":{"tags":0,"tasks":0,"milestones":0,"claims":0,"bindings":0,"events":0},"startedAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(storeDir, "sqlite-migration.json"), []byte(receipt), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".noise-tasks-migration.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runSetup("", false, "--adapter", "anthropic-api", "--model", "m", "--yes", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("inert historic receipts: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := os.Lstat(projectStorePath(project)); err != nil {
		t.Fatalf("store must be created over inert historic receipts: %v", err)
	}
}

// TestSetupRefusesOverRecoveryJournal pins §2/§8.7: a valid §8.6 apply
// journal with no published database never gets a new store suggested over
// it, and a malformed journal fails closed without writing state.
func TestSetupRefusesOverRecoveryJournal(t *testing.T) {
	project := setupProject(t)
	storeDir := filepath.Join(project, ".git", "noise-tasks")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(storeDir, "migration-apply.json")
	if err := os.WriteFile(journalPath, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runSetup("", false, "--adapter", "anthropic-api", "--model", "m", "--yes", "--json")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "invalid migration recovery journal") {
		t.Fatalf("malformed journal refusal = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := os.Lstat(projectStorePath(project)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("malformed journal refusal created the store: %v", err)
	}
	if got, err := os.ReadFile(journalPath); err != nil || string(got) != "not json" {
		t.Fatalf("malformed journal changed on disk: %q %v", got, err)
	}

	body, err := store.JournalBytes(&store.MigrationJournal{
		SchemaVersion:          1,
		RequestID:              "recover1",
		FamilyID:               store.LegacyFamilyUUID(filepath.Join(project, ".git"), project),
		PlanDigest:             "sha256:" + strings.Repeat("0", 64),
		SourceComparisonDigest: "sha256:" + strings.Repeat("1", 64),
		BackupPayloadDigest:    "sha256:" + strings.Repeat("2", 64),
		TargetSchemaVersion:    store.SchemaVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journalPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runSetup("", false, "--adapter", "anthropic-api", "--model", "m", "--yes", "--json")
	want := "migration recovery required; rerun noise tasks migrate apply with the original plan and backup"
	if code != 1 || stdout != "" || !strings.Contains(stderr, want) {
		t.Fatalf("recovery refusal = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := os.Lstat(projectStorePath(project)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery refusal created the store: %v", err)
	}
}

func TestSetupRefusesUnsafePreferencesWithoutWriting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated Windows privileges")
	}
	project := setupProject(t)
	state := filepath.Join(project, ".tasks")
	if err := os.Mkdir(state, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "preferences.json")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, preferences.Path(project)); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runSetup("", false,
		"--adapter", "anthropic-api", "--model", "m", "--yes", "--json")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "preferences") {
		t.Fatalf("symlink refusal = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "outside" {
		t.Fatalf("symlink target changed: %q, %v", got, err)
	}
	if _, err := os.Lstat(projectStorePath(project)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink refusal created the store: %v", err)
	}
}

func TestSetupCrashAfterStoreIsRepairable(t *testing.T) {
	project := setupProject(t)
	originalHook := setupFaultHook
	setupFaultHook = func(point string) error {
		if point != "store-published" {
			t.Fatalf("fault point = %q", point)
		}
		return errors.New("simulated crash")
	}
	t.Cleanup(func() { setupFaultHook = originalHook })

	args := []string{"--adapter", "anthropic-api", "--model", "m", "--yes", "--json"}
	code, stdout, stderr := runSetup("", false, args...)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "simulated crash") {
		t.Fatalf("crash = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := store.Open(projectStorePath(project)); err != nil {
		t.Fatalf("store half-state = %v", err)
	}
	if snapshot, err := preferences.Load(project); err != nil || snapshot.Exists {
		t.Fatalf("preferences half-state = %+v, %v", snapshot, err)
	}

	setupFaultHook = func(string) error { return nil }
	code, stdout, stderr = runSetup("", false, args...)
	if code != 0 || stderr != "" {
		t.Fatalf("repair = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	result := decodeSetupResult(t, stdout)
	if result.StoreAction != "unchanged" || result.PreferencesAction != "created" {
		t.Fatalf("repair result = %+v", result)
	}
}

type concurrentSetupResult struct {
	code   int
	stdout string
	stderr string
}

func runConcurrentSetups(t *testing.T, models ...string) []concurrentSetupResult {
	t.Helper()
	ready := make(chan struct{}, len(models))
	release := make(chan struct{})
	originalHook := setupBeforeLockHook
	setupBeforeLockHook = func() {
		ready <- struct{}{}
		<-release
	}
	t.Cleanup(func() { setupBeforeLockHook = originalHook })

	results := make([]concurrentSetupResult, len(models))
	var wg sync.WaitGroup
	for i, model := range models {
		wg.Add(1)
		go func(i int, model string) {
			defer wg.Done()
			results[i].code, results[i].stdout, results[i].stderr = runSetup("", false,
				"--adapter", "anthropic-api", "--model", model, "--yes", "--json")
		}(i, model)
	}
	for range models {
		<-ready
	}
	close(release)
	wg.Wait()
	return results
}

func TestConcurrentIdenticalSetupIsIdempotent(t *testing.T) {
	setupProject(t)
	results := runConcurrentSetups(t, "same", "same")
	actions := make(map[string]int)
	for _, result := range results {
		if result.code != 0 || result.stderr != "" {
			t.Fatalf("result = %+v", result)
		}
		decoded := decodeSetupResult(t, result.stdout)
		actions[decoded.PreferencesAction]++
	}
	if actions["created"] != 1 || actions["unchanged"] != 1 {
		t.Fatalf("preference actions = %v", actions)
	}
}

func TestConcurrentConflictingSetupRefusesLostUpdate(t *testing.T) {
	project := setupProject(t)
	results := runConcurrentSetups(t, "left", "right")
	successes, refusals := 0, 0
	for _, result := range results {
		switch result.code {
		case 0:
			successes++
		case 1:
			refusals++
			if result.stdout != "" || !strings.Contains(result.stderr, "changed concurrently") {
				t.Fatalf("conflict refusal = %+v", result)
			}
		default:
			t.Fatalf("unexpected result = %+v", result)
		}
	}
	if successes != 1 || refusals != 1 {
		t.Fatalf("successes=%d refusals=%d results=%+v", successes, refusals, results)
	}
	snapshot, err := preferences.Load(project)
	if err != nil || (snapshot.Preferences.Generation.Model != "left" && snapshot.Preferences.Generation.Model != "right") {
		t.Fatalf("winning preferences = %+v, %v", snapshot, err)
	}
}

func TestMalformedPreferencesBlockSetupButNotTaskCommands(t *testing.T) {
	project := setupProject(t)
	db, err := store.CreateWithOwner(projectStorePath(project), project)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureTag("main"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := os.MkdirAll(filepath.Dir(preferences.Path(project)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preferences.Path(project), []byte(`{"schemaVersion":99}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runSetup("", false, "--json")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "invalid preferences") {
		t.Fatalf("setup = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if code, _, stderr := runCmd(cmdList, "--json"); code != 0 {
		t.Fatalf("list read preferences: %s", stderr)
	}
	if code, stdout, stderr := runCmd(cmdContext, "--json"); code != 0 || !strings.Contains(stdout, `"initialized": true`) {
		t.Fatalf("context = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestImplicitMissingProjectDiagnostic(t *testing.T) {
	setupProject(t)
	want := "noise-tasks: project is not initialized; run noise tasks setup\n"
	commands := []struct {
		name string
		run  func([]string, io.Writer, io.Writer) int
		args []string
	}{
		{name: "list", run: cmdList},
		{name: "next", run: cmdNext},
		{name: "show", run: cmdShow, args: []string{"1"}},
		{name: "add", run: cmdAdd, args: []string{"--title", "x"}},
		{name: "set-status", run: cmdSetStatus, args: []string{"1", "done"}},
		{name: "append", run: cmdAppend, args: []string{"1", "x"}},
		{name: "remove", run: cmdRemove, args: []string{"1"}},
		{name: "verify", run: verifyMain},
	}
	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			code, stdout, stderr := runCmd(command.run, command.args...)
			if code != 1 || stdout != "" || stderr != want {
				t.Fatalf("command = code %d stdout %q stderr %q", code, stdout, stderr)
			}
		})
	}
	prd := prdFile(t, "# PRD")
	code, stdout, stderr := runGenerate(fakeFactory(&fakeProvider{drafts: threeDrafts()}), "", "--yes", prd)
	if code != 1 || stdout != "" || stderr != want {
		t.Fatalf("generate = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestExplicitMissingFileKeepsFileDiagnostic(t *testing.T) {
	setupProject(t)
	missing := filepath.Join(t.TempDir(), "missing.json")
	code, stdout, stderr := runCmd(cmdList, "--file", missing)
	if code != 1 || stdout != "" || strings.Contains(stderr, "run noise tasks setup") || !strings.Contains(stderr, missing) {
		t.Fatalf("explicit file = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestSetupRejectsProjectContainedAdapter(t *testing.T) {
	project := setupProject(t)
	bin := filepath.Join(project, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(bin, "omp")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	code, stdout, stderr := runSetup("", false, "--adapter", "omp", "--model", "m", "--yes", "--json")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "not available") {
		t.Fatalf("containment = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := os.Lstat(projectStorePath(project)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("containment refusal created state: %v", err)
	}
}

func TestSetupStoreActionNeverUpdated(t *testing.T) {
	project := setupProject(t)
	db, err := store.CreateWithOwner(projectStorePath(project), project)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	code, stdout, stderr := runSetup("", false, "--adapter", "anthropic-api", "--model", "m", "--yes", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("setup = code %d stderr %q", code, stderr)
	}
	if action := decodeSetupResult(t, stdout).StoreAction; action != "unchanged" {
		t.Fatalf("store action = %q", action)
	}
}

// TestSetupCrashAfterPublicationLeavesCompleteIdentity proves the PR79
// repair: a simulated crash right after the store is published can never
// leave a current-schema authority without its family identity, because creation
// stamps both in one transaction and publishes atomically.
func TestSetupCrashAfterPublicationLeavesCompleteIdentity(t *testing.T) {
	project := setupProject(t)
	originalHook := setupFaultHook
	setupFaultHook = func(point string) error {
		if point != "store-published" {
			t.Fatalf("fault point = %q", point)
		}
		return errors.New("simulated crash")
	}
	t.Cleanup(func() { setupFaultHook = originalHook })

	code, stdout, stderr := runSetup("", false, "--adapter", "anthropic-api", "--model", "m", "--yes", "--json")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "simulated crash") {
		t.Fatalf("crash = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	path := projectStorePath(project)
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("published store unreadable: %v", err)
	}
	familyID, owner, err := db.Identity()
	db.Close()
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	canonical, err := canonicalizePath(project)
	if err != nil {
		t.Fatal(err)
	}
	if familyID == "" || owner != canonical {
		t.Fatalf("identity after crash = (%q, %q), want minted family for %q", familyID, owner, canonical)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("published store has %s sidecar: %v", suffix, err)
		}
	}

	// Rerun adopts the published winner without rewriting it.
	setupFaultHook = func(string) error { return nil }
	code, stdout, stderr = runSetup("", false, "--adapter", "anthropic-api", "--model", "m", "--yes", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("repair = code %d stderr %q", code, stderr)
	}
	result := decodeSetupResult(t, stdout)
	if result.StoreAction != "unchanged" {
		t.Fatalf("repair store action = %q", result.StoreAction)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id2, owner2, err := db.Identity()
	db.Close()
	if err != nil || id2 != familyID || owner2 != owner {
		t.Fatalf("repair changed identity: (%q, %q) -> (%q, %q), %v", familyID, owner, id2, owner2, err)
	}
}

// TestConcurrentSetupsCreateExactlyOneCompleteStore is the create/adopt
// regression: two setups racing from before the lock both succeed, exactly
// one creates, and the published store carries its family identity.
func TestConcurrentSetupsCreateExactlyOneCompleteStore(t *testing.T) {
	project := setupProject(t)
	results := runConcurrentSetups(t, "same-model", "same-model")
	if len(results) != 2 {
		t.Fatalf("results = %+v", results)
	}
	created := 0
	for _, result := range results {
		if result.code != 0 || result.stderr != "" {
			t.Fatalf("concurrent setup = %+v", result)
		}
		if decodeSetupResult(t, result.stdout).StoreAction == "created" {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created actions = %d, want 1", created)
	}
	db, err := store.Open(projectStorePath(project))
	if err != nil {
		t.Fatal(err)
	}
	familyID, owner, err := db.Identity()
	db.Close()
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	canonical, err := canonicalizePath(project)
	if err != nil {
		t.Fatal(err)
	}
	if familyID == "" || owner != canonical {
		t.Fatalf("identity = (%q, %q), want minted family for %q", familyID, owner, canonical)
	}
}
