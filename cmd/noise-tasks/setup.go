package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/preferences"
	"github.com/noisefloor-oss/tasks/internal/store"
)

const setupTag = "main"

var (
	setupBeforeLockHook = func() {}
	setupFaultHook      = func(string) error { return nil }
)

type setupState struct {
	boundary    string
	storePath   string
	initialized bool
	preferences preferences.Snapshot
}

type setupResult struct {
	SchemaVersion          int                    `json:"schemaVersion"`
	ProjectBoundary        string                 `json:"projectBoundary"`
	StorePath              string                 `json:"storePath"`
	PreferencesPath        string                 `json:"preferencesPath"`
	StoreAction            string                 `json:"storeAction"`
	PreferencesAction      string                 `json:"preferencesAction"`
	Generation             preferences.Generation `json:"generation"`
	AdapterAvailable       bool                   `json:"adapterAvailable"`
	AuthenticationVerified bool                   `json:"authenticationVerified"`
}

func cmdSetup(args []string, stdout, stderr io.Writer) int {
	return setupMain(args, os.Stdin, stdout, stderr, fileIsTerminal(os.Stdin))
}

func setupMain(args []string, stdin io.Reader, stdout, stderr io.Writer, terminal bool) int {
	fs := newCommandFlagSet("setup", stderr)
	adapterFlag := fs.String("adapter", "", "generation adapter id")
	modelFlag := fs.String("model", "", "generation model id")
	yesFlag := fs.Bool("yes", false, "confirm creating or replacing preferences")
	jsonFlag := fs.Bool("json", false, "machine-readable JSON on stdout")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "noise-tasks setup: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	seen := make(map[string]bool)
	fs.Visit(func(flag *flag.Flag) { seen[flag.Name] = true })
	adapterSet, modelSet := seen["adapter"], seen["model"]
	if adapterSet != modelSet || adapterSet && (*adapterFlag == "" || *modelFlag == "") {
		fmt.Fprintln(stderr, "noise-tasks setup: --adapter and --model must be supplied together with nonempty values")
		return 2
	}

	observed, err := observeSetupState()
	if err != nil {
		return fail(stderr, err)
	}
	reader := bufio.NewReader(stdin)
	desired, explicit, code := desiredSetupPreferences(observed, *adapterFlag, *modelFlag, adapterSet, terminal, *jsonFlag, reader, stderr)
	if code != 0 {
		return code
	}

	changing := !observed.preferences.Exists || observed.preferences.Preferences != desired
	available, err := preferences.AdapterAvailable(observed.boundary, desired.Generation.Adapter)
	if err != nil {
		return fail(stderr, err)
	}
	if changing && !available {
		return fail(stderr, fmt.Errorf("generation adapter %q is not available", desired.Generation.Adapter))
	}
	if changing && terminal && !*jsonFlag {
		previewSetup(stderr, desired, explicit)
	}
	if changing && !*yesFlag {
		if *jsonFlag || !terminal {
			fmt.Fprintln(stderr, "noise-tasks setup: creating or replacing preferences noninteractively requires --yes")
			return 2
		}
		fmt.Fprint(stderr, "write these generation preferences? [y/N] ")
		if !readSetupConfirm(reader) {
			fmt.Fprintln(stderr, "setup cancelled; no files written")
			return 0
		}
	}

	setupBeforeLockHook()
	result, err := commitSetup(observed, desired, available)
	if err != nil {
		return fail(stderr, err)
	}
	if !available {
		fmt.Fprintf(stderr, "warning: generation adapter %q is not currently available\n", desired.Generation.Adapter)
	}
	if *jsonFlag {
		if err := writeJSON(stdout, result); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	writeSetupHuman(stdout, result)
	return 0
}

func desiredSetupPreferences(state setupState, adapter, model string, explicit, terminal, jsonOutput bool, reader *bufio.Reader, stderr io.Writer) (preferences.Preferences, bool, int) {
	if explicit {
		prefs := preferences.Preferences{
			SchemaVersion: preferences.SchemaVersion,
			Generation:    preferences.Generation{Adapter: adapter, Model: model},
		}
		if err := preferences.Validate(prefs); err != nil {
			fmt.Fprintf(stderr, "noise-tasks setup: %v\n", err)
			return preferences.Preferences{}, true, 2
		}
		return prefs, true, 0
	}
	if state.preferences.Exists {
		return state.preferences.Preferences, false, 0
	}
	if jsonOutput || !terminal {
		fmt.Fprintln(stderr, "noise-tasks setup: --adapter and --model are required without existing preferences in noninteractive or --json mode")
		return preferences.Preferences{}, false, 2
	}
	prefs, err := promptSetupPreferences(state.boundary, reader, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "noise-tasks setup: %v\n", err)
		return preferences.Preferences{}, false, 2
	}
	return prefs, false, 0
}

func promptSetupPreferences(boundary string, reader *bufio.Reader, stderr io.Writer) (preferences.Preferences, error) {
	all := []string{preferences.AdapterAnthropicAPI, preferences.AdapterClaudeCLI, preferences.AdapterOMP, preferences.AdapterCodex, preferences.AdapterKimi}
	available := make([]string, 0, len(all))
	for _, adapter := range all {
		ok, err := preferences.AdapterAvailable(boundary, adapter)
		if err != nil {
			return preferences.Preferences{}, err
		}
		if ok {
			available = append(available, adapter)
		}
	}
	fmt.Fprintln(stderr, "available generation adapters:")
	for i, adapter := range available {
		fmt.Fprintf(stderr, "  %d) %s\n", i+1, adapter)
	}
	fmt.Fprintf(stderr, "select adapter [1-%d]: ", len(available))
	selection, err := readSetupTrimmedLine(reader)
	if err != nil {
		return preferences.Preferences{}, errors.New("adapter selection is required")
	}
	index, err := strconv.Atoi(selection)
	if err != nil || index < 1 || index > len(available) {
		return preferences.Preferences{}, fmt.Errorf("invalid adapter selection %q", selection)
	}
	fmt.Fprint(stderr, "model: ")
	model, err := readSetupRawLine(reader)
	if err != nil {
		return preferences.Preferences{}, errors.New("model is required")
	}
	prefs := preferences.Preferences{
		SchemaVersion: preferences.SchemaVersion,
		Generation:    preferences.Generation{Adapter: available[index-1], Model: model},
	}
	if err := preferences.Validate(prefs); err != nil {
		return preferences.Preferences{}, err
	}
	return prefs, nil
}

func previewSetup(stderr io.Writer, desired preferences.Preferences, explicit bool) {
	source := "selected"
	if explicit {
		source = "requested"
	}
	fmt.Fprintf(stderr, "%s generation preferences:\n  adapter: %s\n  model: %s\n", source, desired.Generation.Adapter, desired.Generation.Model)
}

func readSetupRawLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}

func readSetupTrimmedLine(reader *bufio.Reader) (string, error) {
	line, err := readSetupRawLine(reader)
	return strings.TrimSpace(line), err
}

func readSetupConfirm(reader *bufio.Reader) bool {
	line, err := readSetupTrimmedLine(reader)
	if err != nil {
		return false
	}
	return strings.EqualFold(line, "y") || strings.EqualFold(line, "yes")
}

// observeSetupState resolves the boundary and §8.1 database path, enforces
// the §2.3 fail-closed refusals (retired layouts, legacy JSON live store,
// unsettled or invalid migration state), and loads the preferences snapshot.
func observeSetupState() (setupState, error) {
	_, boundary, err := backlog.ProjectContext()
	if err != nil {
		return setupState{}, err
	}
	storePath, err := contextStorePath(boundary)
	if err != nil {
		return setupState{}, err
	}
	if err := setupRefusals(boundary, storePath); err != nil {
		return setupState{}, err
	}
	snapshot, err := preferences.Load(boundary)
	if err != nil {
		return setupState{}, err
	}
	return setupState{
		boundary:    boundary,
		storePath:   storePath,
		initialized: contextInitialized(storePath),
		preferences: snapshot,
	}, nil
}

// setupRefusals fails closed on the §2.3 refusal states. The retired-layout
// checks are bounded stat-only existence checks on fixed paths; they select
// nothing and never parse (§2).
func setupRefusals(boundary, storePath string) error {
	for _, layout := range []struct{ rel, msg string }{
		{filepath.Join(".tasks", "tasks", "tasks.json"),
			"retired .tasks/tasks/tasks.json layout found; complete the retired-layout migration with the final pre-cutover release before running setup"},
		{filepath.Join(".noisetask", "tasks.json"),
			"retired .noisetask layout found; its migration contract is not available in this release"},
		{filepath.Join(".taskmaster", "tasks", "tasks.json"),
			"retired .taskmaster layout found; complete the retired-layout migration with the final pre-cutover release before running setup"},
	} {
		info, err := os.Lstat(filepath.Join(boundary, layout.rel))
		if err == nil && info.Mode().IsRegular() {
			return errors.New(layout.msg)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	// §2/§8.7 fencing: a valid recovery journal with no database never gets
	// a new store suggested over it, and a malformed journal fails closed.
	// The historic sqlite-migration.json receipts are inert (§8.7): they
	// are never read as authority here.
	if err := migrationProjectGate(boundary, storePath); err != nil {
		return err
	}
	if legacyStorePresent(storePath) {
		return errors.New("legacy live store found; create a verified backup and run noise tasks migrate plan before running setup")
	}
	return nil
}

// openAdoptedStore opens the store a concurrent setup just created. The
// winner's bootstrap can still hold the write lock, and the WAL-mode
// transition answers SQLITE_BUSY without honoring busy_timeout, so the open
// retries briefly before giving up.
func openAdoptedStore(path string) (*store.DB, error) {
	var lastErr error
	for range 100 {
		db, err := store.Open(path)
		if err == nil {
			return db, nil
		}
		if !strings.Contains(err.Error(), "SQLITE_BUSY") && !strings.Contains(err.Error(), "database is locked") {
			return nil, err
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	return nil, lastErr
}

// commitSetup performs §2.3: create the database first when missing, then
// publish preferences, re-reading state under the store's immediate lock so
// a concurrent setup publishing anything but the identical desired state is
// refused. The database is never rewritten once it exists.
func commitSetup(observed setupState, desired preferences.Preferences, available bool) (*setupResult, error) {
	// Canonical §8.1 owner boundary: required to create, and configured on
	// every handle — created, existing, or adopted — before any mutation.
	owner, err := derivedOwnerBoundary(observed.boundary)
	if err != nil {
		return nil, err
	}
	// Create or open the database first (§2.3 ordering). Creation publishes
	// a complete current-schema store — schema, fencing counter, and family
	// identity in one transaction — atomically, so the authority path is
	// never visible partially created. A concurrent setup winning the create
	// race is indistinguishable from a rerun: unchanged.
	storeAction := "unchanged"
	db, err := store.Open(observed.storePath)
	if errors.Is(err, os.ErrNotExist) {
		db, err = store.CreateWithOwner(observed.storePath, owner)
		if err == nil {
			storeAction = "created"
		} else if errors.Is(err, os.ErrExist) {
			// A concurrent setup won the create race after our Lstat
			// pre-check; adopt its store.
			db, err = openAdoptedStore(observed.storePath)
		}
	}
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if current, err := db.PrepareIdentity(owner); err != nil {
		return nil, err
	} else if !current {
		// An older released schema: setup is a minting command and may
		// upgrade it deliberately. A current-schema store that lost its
		// identity already failed PrepareIdentity above and is never
		// silently repaired here.
		if err := db.EnsureIdentity(owner); err != nil {
			return nil, err
		}
	}
	if storeAction == "created" {
		if err := db.EnsureTag(setupTag); err != nil {
			return nil, err
		}
		if err := setupFaultHook("store-published"); err != nil {
			return nil, err
		}
	}

	preferencesAction := "unchanged"
	err = db.WithImmediateLock(func() error {
		_, boundary, berr := backlog.ProjectContext()
		if berr != nil {
			return berr
		}
		if boundary != observed.boundary {
			return errors.New("project boundary changed during setup")
		}
		current, cerr := preferences.Load(observed.boundary)
		if cerr != nil {
			return cerr
		}
		currentIsDesired := current.Exists && current.Preferences == desired
		if !currentIsDesired && !samePreferenceSnapshot(observed.preferences, current) {
			return errors.New("generation preferences changed concurrently; rerun noise tasks setup")
		}
		if !currentIsDesired {
			preferencesAction = "created"
			if current.Exists {
				preferencesAction = "updated"
			}
			if err := preferences.Publish(observed.boundary, desired); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	preferencesRelative, err := setupRelativePath(observed.boundary, preferences.Path(observed.boundary))
	if err != nil {
		return nil, err
	}
	return &setupResult{
		SchemaVersion:          2,
		ProjectBoundary:        observed.boundary,
		StorePath:              observed.storePath,
		PreferencesPath:        preferencesRelative,
		StoreAction:            storeAction,
		PreferencesAction:      preferencesAction,
		Generation:             desired.Generation,
		AdapterAvailable:       available,
		AuthenticationVerified: false,
	}, nil
}

func samePreferenceSnapshot(left, right preferences.Snapshot) bool {
	if left.Exists != right.Exists {
		return false
	}
	return !left.Exists || bytes.Equal(left.Raw, right.Raw)
}

func setupRelativePath(boundary, path string) (string, error) {
	relative, err := filepath.Rel(boundary, path)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("setup path %s escapes project boundary", path)
	}
	return filepath.ToSlash(relative), nil
}

func writeSetupHuman(stdout io.Writer, result *setupResult) {
	storeDisplay := result.StorePath
	if relative, err := filepath.Rel(result.ProjectBoundary, result.StorePath); err == nil &&
		relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		storeDisplay = filepath.ToSlash(relative)
	}
	fmt.Fprintf(stdout, "store %s: %s\n", storeDisplay, result.StoreAction)
	fmt.Fprintf(stdout, "preferences %s: %s\n", result.PreferencesPath, result.PreferencesAction)
	fmt.Fprintf(stdout, "generation: adapter=%s model=%s\n", result.Generation.Adapter, result.Generation.Model)
	fmt.Fprintf(stdout, "adapter available: %t\n", result.AdapterAvailable)
	fmt.Fprintln(stdout, "authentication is unverified")
}
