package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// contextResult is the §2.2 schema 5 project-context contract.
type contextResult struct {
	SchemaVersion       int    `json:"schemaVersion"`
	Start               string `json:"start"`
	ProjectBoundary     string `json:"projectBoundary"`
	StorePath           string `json:"storePath"`
	StoreOwnerBoundary  string `json:"storeOwnerBoundary"`
	StoreSchemaVersion  int    `json:"storeSchemaVersion"`
	EngineSchemaVersion int    `json:"engineSchemaVersion"`
	DocsPath            string `json:"docsPath"`
	PreferencesPath     string `json:"preferencesPath"`
	Initialized         bool   `json:"initialized"`
	Writable            bool   `json:"writable"`
	Blocked             bool   `json:"blocked"`
	BlockedReason       string `json:"blockedReason"`
}

func cmdContext(args []string, stdout, stderr io.Writer) int {
	fs := newCommandFlagSet("context", stderr)
	jsonOutput := fs.Bool("json", false, "machine-readable JSON on stdout")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 || !*jsonOutput {
		fmt.Fprintln(stderr, "usage: noise-tasks context --json")
		return 2
	}

	start, boundary, err := backlog.ProjectContext()
	if err != nil {
		fmt.Fprintln(stderr, "noise-tasks: project context unavailable")
		return 1
	}
	storePath, err := contextStorePath(boundary)
	if err != nil {
		return fail(stderr, err)
	}

	result := &contextResult{
		SchemaVersion:   5,
		Start:           start,
		ProjectBoundary: boundary,
		StorePath:       storePath,
		DocsPath:        ".tasks/docs/PRD.md",
		PreferencesPath: ".tasks/preferences.json",
		Initialized:     contextInitialized(storePath),
	}
	if result.Initialized {
		owner, version, err := contextStoreFacts(storePath)
		if err != nil {
			// An unreadable store is an operational failure (§2.2): exit 1
			// with empty stdout rather than degrading the report.
			fmt.Fprintln(stderr, "noise-tasks: "+err.Error())
			return 1
		}
		result.StoreOwnerBoundary, result.StoreSchemaVersion = owner, version
	}
	result.EngineSchemaVersion = store.SchemaVersion
	blocked, reason, err := contextBlocked(storePath)
	if err != nil {
		// Any malformed or state-inconsistent journal is an operational
		// failure: exit 1 with empty stdout rather than classifying or
		// choosing authority (§2.2).
		fmt.Fprintln(stderr, "noise-tasks: "+err.Error())
		return 1
	}
	result.Blocked, result.BlockedReason = blocked, reason
	result.Writable = !result.Blocked && contextWritable(filepath.Dir(storePath))
	if err := writeJSON(stdout, result); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// contextStoreFacts reads the two §2.2 schema-5 store facts through the
// engine's read-only open path under the shared maintenance lock: the
// recorded owner boundary and the store schema version. Nothing else is
// read and nothing is mutated; the caller treats any failure as an
// operational failure.
func contextStoreFacts(storePath string) (ownerBoundary string, schemaVersion int, err error) {
	db, err := store.Open(storePath)
	if err != nil {
		return "", 0, err
	}
	defer db.Close()
	_, ownerBoundary, err = db.Identity()
	if err != nil {
		return "", 0, err
	}
	schemaVersion, err = db.UserVersion()
	if err != nil {
		return "", 0, err
	}
	return ownerBoundary, schemaVersion, nil
}

// contextStorePath renders the absolute canonical §8.1 database path. The
// database may not exist yet, so symlink resolution is best-effort.
func contextStorePath(boundary string) (string, error) {
	path, _ := store.Path(boundary)
	return canonicalizePath(path)
}

// contextInitialized reports whether the §8.1 database exists as a regular,
// non-symlink file. This field alone never opens the database (§2.2); the
// schema-5 store facts do, read-only, when it exists.
func contextInitialized(storePath string) bool {
	lstat, err := os.Lstat(storePath)
	if err != nil {
		return false
	}
	return lstat.Mode().IsRegular()
}

// contextBlocked applies the §2.2 blocked rule to the §8.6 apply
// journal: a strict-valid journal with no published database proves cutover
// began but SQLite publication did not complete, which is exactly
// `migration-recovery-required`. A journal beside a published database has
// completed publication and is not blocked. A malformed journal is an
// operational failure returned as an error.
func contextBlocked(storePath string) (blocked bool, reason string, err error) {
	recovery, err := store.ReadMigrationRecovery(filepath.Dir(storePath))
	if err != nil {
		return false, "", fmt.Errorf("invalid migration recovery journal: %w", err)
	}
	if recovery == store.RecoveryRequired {
		return true, "migration-recovery-required", nil
	}
	return false, "", nil
}

// contextWritable reports whether the current process can publish in dir, or
// at its nearest existing ancestor before initialization (§2.2). Read-only:
// no probe file is ever created.
func contextWritable(dir string) bool {
	existing := dir
	for {
		canonical, err := canonicalizePath(existing)
		if err == nil {
			if info, statErr := os.Stat(canonical); statErr == nil && info.IsDir() {
				return directoryWritable(canonical)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return false
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return false
		}
		existing = parent
	}
}
