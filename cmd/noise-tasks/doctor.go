package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// doctorLegacyLayout is one retired task-state file found at a family
// boundary (§2.4): named, sized, and stamped, never classified.
type doctorLegacyLayout struct {
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
	ModTime string `json:"modTime"`
}

// doctorFamily is one discovered repository family's authority facts.
type doctorFamily struct {
	Boundary              string               `json:"boundary"`
	CommonDir             string               `json:"commonDir"`
	StorePath             string               `json:"storePath"`
	Initialized           bool                 `json:"initialized"`
	StoreOwnerBoundary    string               `json:"storeOwnerBoundary"`
	StoreSchemaVersion    int                  `json:"storeSchemaVersion"`
	AuthorityError        string               `json:"authorityError"`
	RecoveryJournal       bool                 `json:"recoveryJournal"`
	LegacyLiveJSON        bool                 `json:"legacyLiveJson"`
	LegacyLayouts         []doctorLegacyLayout `json:"legacyLayouts"`
	UnboundWorktrees      []HygieneWorktreeRow `json:"unboundWorktrees"`
	UnboundWorktreesError string               `json:"unboundWorktreesError"`
}

// doctorReport is the §2.4 workspace inventory.
type doctorReport struct {
	SchemaVersion int            `json:"schemaVersion"`
	Root          string         `json:"root"`
	Families      []doctorFamily `json:"families"`
}

var doctorLegacyPaths = []string{
	".taskmaster/tasks/tasks.json",
	".noisetask/tasks.json",
	".tasks/tasks/tasks.json",
}

// cmdDoctor prints the §2.4 read-only workspace inventory. It never
// mutates and appends no event; its only Git use is the same read-only
// registered-worktree listing §8.9 runs, and only for an initialized,
// readable saga-capable store.
func cmdDoctor(args []string, stdout, stderr io.Writer) int {
	fs := newCommandFlagSet("doctor", stderr)
	rootFlag := fs.String("root", "", "directory to walk (default: project start)")
	jsonOutput := fs.Bool("json", false, "machine-readable JSON on stdout")
	if code, ok := parseFlags(fs, stdout, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: noise-tasks doctor [--root <dir>] [--json]")
		return 2
	}
	root := *rootFlag
	if root == "" {
		start, _, err := backlog.ProjectContext()
		if err != nil {
			fmt.Fprintln(stderr, "noise-tasks: project context unavailable")
			return 1
		}
		root = start
	} else {
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			fmt.Fprintf(stderr, "noise-tasks: doctor root is not a directory: %s\n", root)
			return 1
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return fail(stderr, err)
	}
	report, err := walkDoctorFamilies(root)
	if err != nil {
		return fail(stderr, err)
	}
	if *jsonOutput {
		if err := writeJSON(stdout, report); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	writeDoctorHuman(stdout, report)
	return 0
}

// doctorEntry tracks one family's discovery state for deduplication:
// the resolved common directory keys the family, and a primary checkout
// (a directory .git marker) is preferred over a linked worktree.
type doctorEntry struct {
	index     int
	markerDir bool
}

// walkDoctorFamilies walks root depth-first without following symlinks
// or descending into .git, resolving every .git marker to its family's
// common directory so linked worktrees report once. All paths are
// canonicalized to absolute before comparison and reporting, and the
// store path is derived directly from the resolved common directory so
// a separately located Git directory still finds the family store.
func walkDoctorFamilies(root string) (*doctorReport, error) {
	report := &doctorReport{SchemaVersion: 1, Root: root, Families: []doctorFamily{}}
	seen := map[string]*doctorEntry{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An entry the walk cannot read is skipped, not a failure:
			// a directory whose contents cannot be listed is not
			// descended into, while its own family may already have
			// been reported from the first callback (§2.4 — traversal
			// needs execute permission, not list permission). Only the
			// root itself failing is fatal.
			if path == root {
				return err
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == ".git" {
			return fs.SkipDir
		}
		marker, err := os.Lstat(filepath.Join(path, ".git"))
		if err != nil || (!marker.IsDir() && !marker.Mode().IsRegular()) {
			return nil
		}
		common, ok := backlog.GitCommonDir(path)
		if !ok {
			return nil
		}
		common, err = filepath.Abs(common)
		if err != nil {
			return err
		}
		boundary, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if entry, hit := seen[common]; hit {
			if marker.IsDir() && !entry.markerDir {
				// Prefer the primary checkout over the linked worktree
				// already recorded for this family, and refresh the
				// boundary-dependent facts from the new boundary.
				family := &report.Families[entry.index]
				family.Boundary = boundary
				family.LegacyLayouts = collectDoctorLegacyLayouts(boundary)
				entry.markerDir = true
			}
			return nil
		}
		family := doctorFamily{
			Boundary:         boundary,
			CommonDir:        common,
			LegacyLayouts:    []doctorLegacyLayout{},
			UnboundWorktrees: []HygieneWorktreeRow{},
		}
		family.StorePath = filepath.Join(common, "noise-tasks", store.StoreFile)
		family.Initialized = regularFile(family.StorePath)
		if family.Initialized {
			facts, ferr := readDoctorStoreFacts(family.StorePath)
			if ferr != nil {
				family.AuthorityError = ferr.Error()
			} else {
				family.StoreOwnerBoundary, family.StoreSchemaVersion = facts.OwnerBoundary, facts.SchemaVersion
				werr := facts.SagaError
				if werr == nil && facts.SagaCapable() {
					rows, rerr := doctorUnboundWorktrees(boundary, facts.SagaPaths)
					if rerr != nil {
						werr = rerr
					} else {
						// Assigned only on success: the initialized
						// empty slice stays put on failure, so the
						// JSON array is always present (§2.4).
						family.UnboundWorktrees = rows
					}
				}
				if werr != nil {
					// Contained per family like authorityError: one
					// family's unreadable worktree evidence never
					// silences the rest of the walk (§2.4).
					family.UnboundWorktreesError = werr.Error()
				}
			}
		}
		storeDir := filepath.Dir(family.StorePath)
		recovery, rerr := store.ReadMigrationRecovery(storeDir)
		adoption, aerr := store.ReadAdoptionRecovery(storeDir)
		family.RecoveryJournal = recovery != store.RecoveryNone || rerr != nil ||
			adoption != store.RecoveryNone || aerr != nil
		family.LegacyLiveJSON = regularFile(filepath.Join(storeDir, store.LegacyFile))
		family.LegacyLayouts = collectDoctorLegacyLayouts(family.Boundary)
		seen[common] = &doctorEntry{index: len(report.Families), markerDir: marker.IsDir()}
		report.Families = append(report.Families, family)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	sort.Slice(report.Families, func(i, j int) bool {
		return report.Families[i].Boundary < report.Families[j].Boundary
	})
	return report, nil
}

// doctorUnboundWorktrees reads one family's §2.4 hard finding rows: every
// Git-registered worktree no §9.4 saga row of the family names — the
// primary checkout excluded, terminal rows owning their path — judged by
// the same ownership judgment §8.9's unowned-worktree report runs. It
// returns a nil slice only together with an error; the caller assigns
// the rows only on success and contains failures per family (§2.4).
func doctorUnboundWorktrees(boundary string, sagaPaths []string) ([]HygieneWorktreeRow, error) {
	worktrees, err := listRegisteredWorktrees(boundary)
	if err != nil {
		return nil, err
	}
	if len(worktrees) == 0 {
		return nil, errors.New("git returned no worktrees")
	}
	return unboundWorktreeRows(worktrees, sagaPaths), nil
}

// doctorStoreFacts are one family's read-only store facts: the two §2.2
// identity facts, plus — when the store is old enough to carry the §9.4
// saga surface — every saga row's target path, the ownership evidence
// §2.4's unbound-worktree finding judges against.
type doctorStoreFacts struct {
	OwnerBoundary string
	SchemaVersion int
	SagaPaths     []string
	SagaError     error
}

// SagaCapable reports whether the store's schema is old enough to carry
// the §9.4 saga surface: only then does the family have ownership
// evidence to judge registered worktrees against.
func (f doctorStoreFacts) SagaCapable() bool {
	return f.SchemaVersion >= store.WorktreeSagaSchemaVersion
}

// readDoctorStoreFacts reads one family's store facts through a single
// lock-free read-only open: the walk never creates or modifies a file,
// including maintenance.lock. A store older than the saga surface
// carries no ownership evidence: SagaPaths stays empty and SagaError
// nil, so the family reports no worktree findings. A saga read that
// fails is reported separately from the identity facts so only the
// worktree class is marked troubled.
func readDoctorStoreFacts(storePath string) (doctorStoreFacts, error) {
	db, err := store.OpenReadOnly(storePath)
	if err != nil {
		return doctorStoreFacts{}, err
	}
	defer db.Close()
	var facts doctorStoreFacts
	_, facts.OwnerBoundary, err = db.Identity()
	if err != nil {
		return doctorStoreFacts{}, err
	}
	facts.SchemaVersion, err = db.UserVersion()
	if err != nil {
		return doctorStoreFacts{}, err
	}
	if facts.SagaCapable() {
		facts.SagaPaths, facts.SagaError = db.WorktreeSagaPaths()
	}
	return facts, nil
}

func regularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

// collectDoctorLegacyLayouts reads the retired task-state layouts at
// one boundary, sorted by path.
func collectDoctorLegacyLayouts(boundary string) []doctorLegacyLayout {
	layouts := []doctorLegacyLayout{}
	for _, rel := range doctorLegacyPaths {
		full := filepath.Join(boundary, rel)
		if info, err := os.Lstat(full); err == nil && info.Mode().IsRegular() {
			layouts = append(layouts, doctorLegacyLayout{
				Path:    rel,
				Bytes:   info.Size(),
				ModTime: info.ModTime().UTC().Format(time.RFC3339),
			})
		}
	}
	sort.Slice(layouts, func(i, j int) bool { return layouts[i].Path < layouts[j].Path })
	return layouts
}

// writeDoctorHuman renders the §2.4 human inventory.
func writeDoctorHuman(w io.Writer, r *doctorReport) {
	noStore, legacy := 0, 0
	for _, family := range r.Families {
		if !family.Initialized {
			noStore++
		}
		if len(family.LegacyLayouts) > 0 {
			legacy++
		}
		fmt.Fprintf(w, "%s: ", family.Boundary)
		if family.Initialized && family.AuthorityError == "" {
			fmt.Fprintf(w, "store ok schema %d", family.StoreSchemaVersion)
		} else if family.Initialized {
			fmt.Fprintf(w, "store unreadable: %s", family.AuthorityError)
		} else {
			fmt.Fprintf(w, "no store")
		}
		if family.RecoveryJournal {
			fmt.Fprintf(w, ", recovery journal")
		}
		if family.LegacyLiveJSON {
			fmt.Fprintf(w, ", live.json")
		}
		for _, layout := range family.LegacyLayouts {
			fmt.Fprintf(w, ", legacy %s (%d bytes, %s)", layout.Path, layout.Bytes, layout.ModTime)
		}
		fmt.Fprintln(w)
		if family.UnboundWorktreesError != "" {
			// Whitespace is collapsed so a multi-line Git diagnostic
			// cannot break the one-line-per-finding shape; the JSON
			// field keeps the raw text.
			fmt.Fprintf(w, "  unbound worktree check unavailable: %s\n",
				strings.Join(strings.Fields(family.UnboundWorktreesError), " "))
		}
		for _, row := range family.UnboundWorktrees {
			fmt.Fprintf(w, "  hard finding: unbound worktree %s %s %s%s\n", row.Path,
				worktreeStateLabel(row.Branch, row.Detached, row.Head),
				existsLabel(row.Exists), dirtyLabel(row.Dirty, row.Exists))
		}
	}
	initialized := len(r.Families) - noStore
	unbound := 0
	for _, family := range r.Families {
		unbound += len(family.UnboundWorktrees)
	}
	fmt.Fprintf(w, "%d families: %d initialized, %d without a store, %d carrying retired layouts, %d unbound registered worktree(s)\n",
		len(r.Families), initialized, noStore, legacy, unbound)
}
