package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// Diagnostics fixed by SPEC §2, §8.1, and §8.7 (exit 1, stderr, empty stdout).
var (
	errProjectNotInitialized = errors.New("project is not initialized; run noise tasks setup")
	errLegacyJSONStore       = errors.New("legacy live store found; create a verified backup and run noise tasks migrate plan")
	errMigrationRecovery     = errors.New("migration recovery required; rerun noise tasks migrate apply with the original plan and backup")
	errAdoptionRecovery      = errors.New("adoption recovery required; rerun noise tasks migrate adopt apply with the original plan and backup")
	errOutsideGitRehoming    = errors.New("existing outside-Git task store cannot be rehomed automatically; back it up and restore it into this repository family")
)

// outsideGitRehomingGate refuses to switch from an existing fallback store to
// a newly discovered Git authority (§8.1). Non-regular entries and inspection
// failures fail closed without following or replacing them.
func outsideGitRehomingGate(boundary string) error {
	if _, inGit := store.Path(boundary); !inGit {
		return nil
	}
	info, err := statRegular(filepath.Join(boundary, ".tasks", "tasks.sqlite"))
	if err != nil {
		return err
	}
	if info != nil {
		return errOutsideGitRehoming
	}
	return nil
}

// migrationJournalGate enforces the §2/§8.7 journal fencing for one derived
// store path: a malformed journal fails closed as an operational failure, a
// strict-valid journal with no published database is the recovery state only
// matching migrate apply may advance, and a valid journal beside a published
// database leaves normal commands on the database. It returns (proceed, err).
func migrationJournalGate(storePath string) error {
	recovery, err := store.ReadMigrationRecovery(filepath.Dir(storePath))
	if err != nil {
		return fmt.Errorf("invalid migration recovery journal: %w", err)
	}
	if recovery == store.RecoveryRequired {
		return errMigrationRecovery
	}
	return nil
}

// adoptionJournalGate is the adoption half of the §8.7 fencing: a valid
// adoption journal with no published database is the recovery state only the
// matching migrate adopt apply may advance.
func adoptionJournalGate(storePath string) error {
	adoption, err := store.ReadAdoptionRecovery(filepath.Dir(storePath))
	if err != nil {
		return fmt.Errorf("invalid adoption recovery journal: %w", err)
	}
	if adoption == store.RecoveryRequired {
		return errAdoptionRecovery
	}
	return nil
}

// migrationProjectGate is the onboarding-strength recovery gate. In
// addition to the recovery-required and structural checks, a published
// target beside a journal must prove its exact plan-bound migration receipt
// before init or setup may observe or mutate any project state.
func migrationProjectGate(boundary, storePath string) error {
	recovery, err := store.ReadMigrationRecovery(filepath.Dir(storePath))
	if err != nil {
		return fmt.Errorf("invalid migration recovery journal: %w", err)
	}
	if recovery == store.RecoveryRequired {
		return errMigrationRecovery
	}
	adoption, err := store.ReadAdoptionRecovery(filepath.Dir(storePath))
	if err != nil {
		return fmt.Errorf("invalid adoption recovery journal: %w", err)
	}
	if adoption == store.RecoveryRequired {
		return errAdoptionRecovery
	}
	if recovery != store.RecoveryPublished && adoption != store.RecoveryPublished {
		return nil
	}
	owner, err := derivedOwnerBoundary(boundary)
	if err != nil {
		return err
	}
	db, err := store.Open(storePath)
	if err != nil {
		return err
	}
	defer db.Close()
	if recovery == store.RecoveryPublished {
		if err := store.ValidatePublishedMigrationRecovery(db, filepath.Dir(filepath.Dir(storePath)), owner); err != nil {
			return fmt.Errorf("invalid published migration recovery state: %w", err)
		}
	}
	if adoption == store.RecoveryPublished {
		if err := store.ValidatePublishedAdoptionRecovery(db, filepath.Dir(filepath.Dir(storePath)), owner); err != nil {
			return fmt.Errorf("invalid published adoption recovery state: %w", err)
		}
	}
	return nil
}

// commonFlags are accepted by every command (SPEC §4).
type commonFlags struct {
	file string
	tag  string
	json bool
}

func newFlagSet(name string, stderr io.Writer, cf *commonFlags) *flag.FlagSet {
	fs := newCommandFlagSet(name, stderr)
	fs.StringVar(&cf.file, "file", "", "path to a standalone JSON fixture document (never authoritative)")
	fs.StringVar(&cf.tag, "tag", "", `tag to operate on (default: sole tag, else "main")`)
	fs.BoolVar(&cf.json, "json", false, "machine-readable JSON on stdout")
	return fs
}

func newCommandFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: noise-tasks %s [options]\n", name)
		fs.PrintDefaults()
	}
	return fs
}

// parseFlags parses args and reports whether the command should proceed.
// On a help flag (-h, -help, --help) it prints the command's usage to
// stdout and returns exit code 0; on a bad flag, 2. Parse output is
// captured and routed per outcome, so help never leaks to stderr. After a
// successful parse it resolves any fencing-token -file twins, so one choke
// point covers every command.
func parseFlags(fs *flag.FlagSet, stdout io.Writer, args []string) (code int, ok bool) {
	output := fs.Output()
	var parsed bytes.Buffer
	fs.SetOutput(&parsed)
	err := fs.Parse(args)
	fs.SetOutput(output)
	switch err {
	case flag.ErrHelp:
		fmt.Fprint(stdout, parsed.String())
		return 0, false
	case nil:
	default:
		fmt.Fprint(output, parsed.String())
		return 2, false
	}
	if err := resolveTokenFileFlags(fs); err != nil {
		fmt.Fprintln(output, "noise-tasks: "+err.Error())
		return 2, false
	}
	return 0, true
}

// projectStore is one invocation's resolved §8.1 authority.
type projectStore struct {
	db       *store.DB
	path     string
	boundary string
	owner    string
}

// openProjectStore resolves the §8.1 SQLite authority for an implicit
// invocation: the database derived from the repository family's Git common
// directory. A legacy JSON live store with no database is the §8.6 refusal;
// neither present is the §2 no-database refusal. No worktree file is ever
// selected as task state.
func openProjectStore() (*projectStore, error) {
	_, boundary, err := backlog.ProjectContext()
	if err != nil {
		return nil, err
	}
	return openProjectStoreAt(boundary)
}

func openProjectStoreAt(boundary string) (*projectStore, error) {
	path, _ := store.Path(boundary)
	// A matching recovery journal takes precedence over the legacy-found
	// refusal and every missing-state/onboarding diagnostic (§8.7); a
	// malformed journal fails closed even with a database present. An
	// adoption recovery journal takes the same precedence (§8.6A).
	recovery, recoveryErr := store.ReadMigrationRecovery(filepath.Dir(path))
	if recoveryErr != nil {
		return nil, fmt.Errorf("invalid migration recovery journal: %w", recoveryErr)
	}
	if recovery == store.RecoveryRequired {
		return nil, errMigrationRecovery
	}
	adoption, adoptionErr := store.ReadAdoptionRecovery(filepath.Dir(path))
	if adoptionErr != nil {
		return nil, fmt.Errorf("invalid adoption recovery journal: %w", adoptionErr)
	}
	if adoption == store.RecoveryRequired {
		return nil, errAdoptionRecovery
	}
	if err := migrationJournalGate(path); err != nil {
		return nil, err
	}
	if err := outsideGitRehomingGate(boundary); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if legacy, lerr := os.Lstat(filepath.Join(filepath.Dir(path), store.LegacyFile)); lerr == nil && legacy.Mode().IsRegular() {
			return nil, errLegacyJSONStore
		}
		return nil, errProjectNotInitialized
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("task store %s is not a regular file", path)
	}
	db, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	// Derive the canonical §8.1 owner boundary: it is what the
	// transactional v1→v2 step mints the family identity with.
	owner, err := derivedOwnerBoundary(boundary)
	if err != nil {
		db.Close()
		return nil, err
	}
	if recovery == store.RecoveryPublished {
		if err := store.ValidatePublishedMigrationRecovery(db, filepath.Dir(filepath.Dir(path)), owner); err != nil {
			db.Close()
			return nil, fmt.Errorf("invalid published migration recovery state: %w", err)
		}
	}
	if adoption == store.RecoveryPublished {
		if err := store.ValidatePublishedAdoptionRecovery(db, filepath.Dir(filepath.Dir(path)), owner); err != nil {
			db.Close()
			return nil, fmt.Errorf("invalid published adoption recovery state: %w", err)
		}
	}
	// §8.2 normal-open gate: configure the derived canonical owner for a
	// possible older-schema mutation, and on every identity-bearing schema
	// validate the stored familyId/ownerBoundary pair immediately, before any
	// read or write is served. A wrong boundary, a partial or missing identity,
	// or a malformed familyId fails closed; an older store stays readable and its first normal
	// mutation traverses the required upgrades atomically.
	if _, err := db.PrepareIdentity(owner); err != nil {
		db.Close()
		return nil, err
	}
	return &projectStore{db: db, path: path, boundary: boundary, owner: owner}, nil
}

// resolveStoreTag opens the project store and resolves the §4 tag default.
func resolveStoreTag(cf *commonFlags) (*projectStore, string, error) {
	ps, err := openProjectStore()
	if err != nil {
		return nil, "", err
	}
	tag, err := ps.db.ResolveTag(cf.tag)
	if err != nil {
		ps.db.Close()
		return nil, "", err
	}
	return ps, tag, nil
}

// fixturePath resolves the explicit --file fixture document without following
// a symlink at its final component. It is an ordinary file error when missing,
// never project onboarding guidance (§2).
func fixturePath(file string) (string, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("backlog file not found: %s", abs)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("backlog path is not a regular file: %s", abs)
	}
	return abs, nil
}

// loadFixture reads the --file fixture document and resolves the tag.
func loadFixture(cf *commonFlags) (*backlog.File, string, error) {
	path, err := fixturePath(cf.file)
	if err != nil {
		return nil, "", err
	}
	f, err := backlog.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	tag, err := resolveTag(f, cf.tag)
	if err != nil {
		return nil, "", err
	}
	return f, tag, nil
}

// mutateFixture loads the --file fixture document and applies fn, which
// persists through the codec's atomic write path (SPEC §3).
func mutateFixture(cf *commonFlags, fn func(*backlog.File, string) error) error {
	f, tag, err := loadFixture(cf)
	if err != nil {
		return err
	}
	return fn(f, tag)
}

// canonicalizePath renders an absolute, symlink-resolved, clean path.
func canonicalizePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}

// statRegular stats path (without following symlinks), requiring a regular
// file when it exists — a symlink at the path is never mistaken for the
// absent-or-regular store document (§8.5). A missing path reports (nil, nil).
func statRegular(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return info, nil
}

// legacyStorePresent reports whether the retired §8.6 JSON live store exists
// next to where the §8.1 database would live.
func legacyStorePresent(dbPath string) bool {
	info, err := os.Lstat(filepath.Join(filepath.Dir(dbPath), store.LegacyFile))
	return err == nil && info.Mode().IsRegular()
}

// resolveTag applies the SPEC §4 default: sole tag, else "main".
func resolveTag(f *backlog.File, flagTag string) (string, error) {
	tags := f.Tags()
	if flagTag != "" {
		if !f.HasTag(flagTag) {
			return "", fmt.Errorf("tag %q not found (available: %s)", flagTag, strings.Join(tags, ", "))
		}
		return flagTag, nil
	}
	if len(tags) == 1 {
		return tags[0], nil
	}
	if f.HasTag("main") {
		return "main", nil
	}
	return "", fmt.Errorf("no --tag given and no %q tag (available: %s)", "main", strings.Join(tags, ", "))
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "noise-tasks: %v\n", err)
	return 1
}

// storeToBacklogTask renders a store row as the §1 task shape used by the
// human and fixture output paths.
func storeToBacklogTask(t *store.Task) (*backlog.Task, error) {
	var subtasks []json.RawMessage
	if len(t.Subtasks) > 0 {
		if err := json.Unmarshal(t.Subtasks, &subtasks); err != nil {
			return nil, fmt.Errorf("decode task %s subtasks: %w", t.ID, err)
		}
	}
	return &backlog.Task{
		ID:           t.ID,
		Title:        t.Title,
		Description:  t.Description,
		Details:      t.Details,
		Status:       t.Status,
		Priority:     t.Priority,
		Milestone:    t.Milestone,
		Link:         t.Link,
		Dependencies: t.Dependencies,
		Subtasks:     subtasks,
		UpdatedAt:    t.UpdatedAt,
	}, nil
}

// resolveStoreReference reads one task by top-level or hierarchical id.
func resolveStoreReference(db *store.DB, tag, raw string) (task *store.Task, subtask *backlog.Subtask, err error) {
	parentID, subtaskID, err := backlog.ParseTaskReference(raw)
	if err != nil {
		return nil, nil, err
	}
	task, err = db.GetTask(tag, parentID)
	if err != nil {
		return nil, nil, err
	}
	if task == nil {
		return nil, nil, fmt.Errorf("task %s not found in tag %q", raw, tag)
	}
	if subtaskID == "" {
		return task, nil, nil
	}
	var raws []json.RawMessage
	if len(task.Subtasks) > 0 {
		if err := json.Unmarshal(task.Subtasks, &raws); err != nil {
			return nil, nil, err
		}
	}
	for _, rawSub := range raws {
		sub, err := backlog.DecodeSubtask(parentID, rawSub)
		if err != nil {
			return nil, nil, err
		}
		if sub.ID == subtaskID {
			return task, sub, nil
		}
	}
	return nil, nil, fmt.Errorf("task %s not found in tag %q", raw, tag)
}

// taskOut is the machine-readable task shape. Every field is always
// present so consumers get a stable schema; dependencies and subtasks
// are never null.
type taskOut struct {
	ID           string            `json:"id"`
	Title        string            `json:"title"`
	Description  string            `json:"description"`
	Details      string            `json:"details"`
	Status       string            `json:"status"`
	Priority     string            `json:"priority"`
	Milestone    string            `json:"milestone"`
	ExternalLink *externalLinkOut  `json:"externalLink"`
	Dependencies []string          `json:"dependencies"`
	Subtasks     []json.RawMessage `json:"subtasks"`
	UpdatedAt    string            `json:"updatedAt"`
}

// externalLinkOut is the machine-readable externalLink member (SPEC §4):
// always present on task output — null when unset — with the issue
// rendered as the §1 string form.
type externalLinkOut struct {
	Family     string `json:"family"`
	Repository string `json:"repository"`
	Issue      string `json:"issue"`
}

func toExternalLinkOut(l *backlog.ExternalLink) externalLinkOut {
	if l == nil {
		return externalLinkOut{}
	}
	return externalLinkOut{Family: l.Family, Repository: l.Repository, Issue: strconv.FormatInt(l.Issue, 10)}
}

// subtaskOut is the stable machine-readable shape for `show parent.child`.
// ID is always qualified even when the stored child id is an integer.
type subtaskOut struct {
	ID           string   `json:"id"`
	ParentTaskID string   `json:"parentTaskId"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Details      string   `json:"details"`
	Status       string   `json:"status"`
	Dependencies []string `json:"dependencies"`
	UpdatedAt    string   `json:"updatedAt"`
}

// liveSubtaskOut extends subtaskOut with the live revision so a caller obtains
// a CAS handle in the same step as reading state (SPEC §8.2).
type liveSubtaskOut struct {
	subtaskOut
	Revision int `json:"revision"`
}

// effectivePriority applies the SPEC §1 default: a task whose priority
// is absent is medium — ranked, filtered, and reported as such.
func effectivePriority(p string) string {
	if p == "" {
		return "medium"
	}
	return p
}

func toTaskOut(t *backlog.Task) taskOut {
	out := taskOut{
		ID:           t.ID,
		Title:        t.Title,
		Description:  t.Description,
		Details:      t.Details,
		Status:       t.Status,
		Priority:     effectivePriority(t.Priority),
		Milestone:    t.Milestone,
		ExternalLink: toExternalLinkOutPtr(t.Link),
		Dependencies: t.Dependencies,
		Subtasks:     t.Subtasks,
		UpdatedAt:    t.UpdatedAt,
	}
	if out.Dependencies == nil {
		out.Dependencies = []string{}
	}
	if out.Subtasks == nil {
		out.Subtasks = []json.RawMessage{}
	}
	return out
}

// toExternalLinkOutPtr renders an unset link as the JSON null member.
func toExternalLinkOutPtr(l *backlog.ExternalLink) *externalLinkOut {
	if l == nil {
		return nil
	}
	out := toExternalLinkOut(l)
	return &out
}

func toSubtaskOut(subtask *backlog.Subtask) subtaskOut {
	out := subtaskOut{
		ID:           subtask.ParentID + "." + subtask.ID,
		ParentTaskID: subtask.ParentID,
		Title:        subtask.Title,
		Description:  subtask.Description,
		Details:      subtask.Details,
		Status:       subtask.Status,
		Dependencies: subtask.Dependencies,
		UpdatedAt:    subtask.UpdatedAt,
	}
	if out.Dependencies == nil {
		out.Dependencies = []string{}
	}
	return out
}

func toLiveSubtaskOut(subtask *backlog.Subtask, revision int) liveSubtaskOut {
	return liveSubtaskOut{subtaskOut: toSubtaskOut(subtask), Revision: revision}
}

func writeJSON(w io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", data)
	return err
}

func depsString(deps []string) string {
	if len(deps) == 0 {
		return "-"
	}
	return strings.Join(deps, ",")
}

// printTask renders one task for humans (used by show and next).
func printTask(w io.Writer, t *backlog.Task) {
	fmt.Fprintf(w, "id:           %s\n", t.ID)
	fmt.Fprintf(w, "title:        %s\n", t.Title)
	fmt.Fprintf(w, "status:       %s\n", t.Status)
	fmt.Fprintf(w, "priority:     %s\n", effectivePriority(t.Priority))
	if t.Milestone != "" {
		fmt.Fprintf(w, "milestone:    %s\n", t.Milestone)
	}
	if t.Link != nil {
		fmt.Fprintf(w, "externalLink: %s\n", t.Link.String())
	}
	fmt.Fprintf(w, "dependencies: %s\n", depsString(t.Dependencies))
	if t.UpdatedAt != "" {
		fmt.Fprintf(w, "updated:      %s\n", t.UpdatedAt)
	}
	if n := len(t.Subtasks); n > 0 {
		fmt.Fprintf(w, "subtasks:     %d\n", n)
	}
	if t.Description != "" {
		fmt.Fprintf(w, "\ndescription:\n%s\n", indentBlock(t.Description))
	}
	if t.Details != "" {
		fmt.Fprintf(w, "\ndetails:\n%s\n", indentBlock(t.Details))
	}
}

func printSubtask(w io.Writer, subtask *backlog.Subtask) {
	fmt.Fprintf(w, "id:           %s.%s\n", subtask.ParentID, subtask.ID)
	fmt.Fprintf(w, "parent:       %s\n", subtask.ParentID)
	fmt.Fprintf(w, "title:        %s\n", subtask.Title)
	fmt.Fprintf(w, "status:       %s\n", subtask.Status)
	fmt.Fprintf(w, "dependencies: %s\n", depsString(subtask.Dependencies))
	if subtask.UpdatedAt != "" {
		fmt.Fprintf(w, "updated:      %s\n", subtask.UpdatedAt)
	}
	if subtask.Description != "" {
		fmt.Fprintf(w, "\ndescription:\n%s\n", indentBlock(subtask.Description))
	}
	if subtask.Details != "" {
		fmt.Fprintf(w, "\ndetails:\n%s\n", indentBlock(subtask.Details))
	}
}

func indentBlock(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// statusOrder is the canonical display order for summary counts.
var statusOrder = []string{
	"pending", "in-progress", "review", "done", "deferred", "cancelled", "blocked",
}

func summaryLine(counts map[string]int, total int) string {
	if total == 0 {
		return "0 tasks"
	}
	noun := "tasks"
	if total == 1 {
		noun = "task"
	}
	var parts []string
	for _, s := range statusOrder {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[s], s))
		}
	}
	return fmt.Sprintf("%d %s: %s", total, noun, strings.Join(parts, ", "))
}
