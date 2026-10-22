package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// verify implements SPEC §4 `verify [--against <git-ref>]`: structural
// integrity of the current §8.1 database state, or of the standalone --file
// fixture document (unique ids, valid statuses, dependency references exist,
// no dependency cycles, counters consistent). --against is valid only with
// --file and proves no task present in the ref's version of the file was
// removed, plus a report of every field-level change. Exit 0 clean,
// 1 findings, 2 error.

type verifyFinding struct {
	Tag     string `json:"tag,omitempty"`
	Task    string `json:"task,omitempty"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type verifyChange struct {
	Tag     string `json:"tag"`
	Task    string `json:"task,omitempty"`
	Kind    string `json:"kind"` // "task-added" | "tag-added" | "field-change"
	Field   string `json:"field,omitempty"`
	Old     string `json:"old,omitempty"`
	New     string `json:"new,omitempty"`
	Message string `json:"message"`
}

type verifyReport struct {
	File     string          `json:"file"`
	Against  string          `json:"against,omitempty"`
	Clean    bool            `json:"clean"`
	Findings []verifyFinding `json:"findings"`
	Changes  []verifyChange  `json:"changes"`

	// Human-output summary counts; not serialized.
	taskCount int
	tagCount  int
}

func runVerify(args []string) int {
	return verifyMain(args, os.Stdout, os.Stderr)
}

func verifyMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fileFlag := fs.String("file", "", "path to a standalone fixture document")
	tagFlag := fs.String("tag", "", "verify a single tag (default: all tags)")
	jsonFlag := fs.Bool("json", false, "machine-readable output")
	againstFlag := fs.String("against", "", "git ref to prove no tasks were removed since (--file only)")
	// parseFlags keeps the -help exit code aligned at 0 across commands.
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "verify: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *againstFlag != "" && *fileFlag == "" {
		fmt.Fprintln(stderr, "verify: --against is only valid with --file")
		return 2
	}

	report := &verifyReport{
		Against:  *againstFlag,
		Findings: []verifyFinding{},
		Changes:  []verifyChange{},
	}

	var cur *backlog.File
	if *fileFlag != "" {
		path, err := fixturePath(*fileFlag)
		if err != nil {
			fmt.Fprintf(stderr, "verify: %v\n", err)
			return 2
		}
		report.File = path
		cur, err = backlog.ReadFile(path)
		if err != nil {
			// The codec enforces the SPEC §1 structural rules (unique ids,
			// well-formed ids, known statuses and priorities) at parse time;
			// those refusals are verify findings, not operational errors.
			if f := codecFinding(err); f != nil {
				report.Findings = append(report.Findings, *f)
				return emitReport(report, *jsonFlag, stdout, stderr)
			}
			fmt.Fprintf(stderr, "verify: %v\n", err)
			return 2
		}
	} else {
		// Without --file, verify checks the current §8.1 database state; the
		// §2 refusal diagnostics apply on the implicit path.
		ps, err := openProjectStore()
		if err != nil {
			return fail(stderr, err)
		}
		defer ps.db.Close()
		report.File = ps.path
		doc, err := verifyStoreDoc(ps.db)
		if err != nil {
			return fail(stderr, err)
		}
		cur, err = backlog.Parse(doc)
		if err != nil {
			if f := codecFinding(err); f != nil {
				report.Findings = append(report.Findings, *f)
				return emitReport(report, *jsonFlag, stdout, stderr)
			}
			fmt.Fprintf(stderr, "verify: %v\n", err)
			return 2
		}
	}

	tags := cur.Tags()
	if *tagFlag != "" {
		if !cur.HasTag(*tagFlag) {
			fmt.Fprintf(stderr, "verify: tag %q not found in %s\n", *tagFlag, report.File)
			return 2
		}
		tags = []string{*tagFlag}
	}
	report.tagCount = len(tags)
	for _, tag := range tags {
		tasks, _ := cur.Tasks(tag)
		report.taskCount += len(tasks)
	}

	report.Findings = append(report.Findings, structuralFindings(cur, tags)...)

	if *againstFlag != "" {
		old, err := readRefBacklog(*againstFlag, report.File)
		if err != nil {
			fmt.Fprintf(stderr, "verify: %v\n", err)
			return 2
		}
		diffTags := tags
		if *tagFlag == "" {
			diffTags = unionTags(old, cur)
		}
		df, dc := diffAgainst(old, cur, diffTags, *againstFlag)
		report.Findings = append(report.Findings, df...)
		report.Changes = append(report.Changes, dc...)
	}

	return emitReport(report, *jsonFlag, stdout, stderr)
}

// verifyStoreDoc renders the database as a §1 JSON document so verify runs
// the same structural checks on both surfaces. Task fields come from the
// store's canonical §1 rendering; the tag block carries the counters the
// store maintains and the byte-faithful milestone registry.
func verifyStoreDoc(db *store.DB) ([]byte, error) {
	if err := db.ValidateStore(); err != nil {
		return nil, err
	}
	snap, err := db.Snapshot()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(snap.Tags))
	for name := range snap.Tags {
		names = append(names, name)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, name := range names {
		if i > 0 {
			buf.WriteByte(',')
		}
		nameJSON, _ := json.Marshal(name)
		buf.Write(nameJSON)
		buf.WriteString(`:{"tasks":[`)
		entry := snap.Tags[name]
		for j, task := range entry.Tasks {
			if j > 0 {
				buf.WriteByte(',')
			}
			fields, err := task.Fields()
			if err != nil {
				return nil, err
			}
			buf.Write(fields)
		}
		buf.WriteString(`],"metadata":`)
		meta := backlog.Metadata{
			Version:        "1.0.0",
			LastModified:   time.Now().UTC().Format(time.RFC3339),
			TaskCount:      entry.Info.TaskCount,
			CompletedCount: entry.Info.CompletedCount,
			Tags:           []string{name},
		}
		metaJSON, err := json.Marshal(meta)
		if err != nil {
			return nil, err
		}
		buf.Write(metaJSON)
		if len(entry.Info.Milestones) > 0 {
			buf.WriteString(`,"milestones":`)
			buf.Write(entry.Info.Milestones)
		}
		extraKeys := make([]string, 0, len(entry.Info.Extra))
		for k := range entry.Info.Extra {
			extraKeys = append(extraKeys, k)
		}
		sort.Strings(extraKeys)
		for _, k := range extraKeys {
			keyJSON, _ := json.Marshal(k)
			buf.WriteByte(',')
			buf.Write(keyJSON)
			buf.WriteByte(':')
			buf.Write(entry.Info.Extra[k])
		}
		buf.WriteByte('}')
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// codecFinding maps a codec parse refusal onto a verify finding. Anything
// that is not one of the codec's typed structural errors (unreadable file,
// malformed JSON, missing tasks key) is an operational error instead.
func codecFinding(err error) *verifyFinding {
	var dup *backlog.ErrDuplicateID
	if errors.As(err, &dup) {
		return &verifyFinding{Tag: dup.Tag, Task: dup.ID, Kind: "duplicate-id", Message: dup.Error()}
	}
	var badID *backlog.ErrInvalidID
	if errors.As(err, &badID) {
		return &verifyFinding{Tag: badID.Tag, Kind: "invalid-id", Message: badID.Error()}
	}
	var badStatus *backlog.ErrInvalidStatus
	if errors.As(err, &badStatus) {
		return &verifyFinding{Task: badStatus.ID, Kind: "invalid-status", Message: badStatus.Error()}
	}
	var badPriority *backlog.ErrInvalidPriority
	if errors.As(err, &badPriority) {
		return &verifyFinding{Task: badPriority.ID, Kind: "invalid-priority", Message: badPriority.Error()}
	}
	return nil
}

// structuralFindings runs the integrity checks the codec does not enforce:
// dependency references exist, no dependency cycles, completedCount matches
// the number of done tasks. (taskCount is set on file creation and never
// trusted afterward per SPEC §1, so it is deliberately not checked.)
func structuralFindings(f *backlog.File, tags []string) []verifyFinding {
	var out []verifyFinding
	for _, tag := range tags {
		tasks, err := f.Tasks(tag)
		if err != nil {
			continue
		}
		ids := make(map[string]bool, len(tasks))
		for _, t := range tasks {
			ids[t.ID] = true
		}
		for _, t := range tasks {
			for _, dep := range t.Dependencies {
				if !ids[dep] {
					out = append(out, verifyFinding{
						Tag:     tag,
						Task:    t.ID,
						Kind:    "missing-dependency",
						Message: fmt.Sprintf("task %s: dependency %s does not exist", t.ID, dep),
					})
				}
			}
		}
		out = append(out, cycleFindings(tag, tasks)...)
		out = append(out, milestoneFindings(f, tag, tasks)...)
		if meta, _ := f.Metadata(tag); meta != nil {
			done := 0
			for _, t := range tasks {
				if t.Status == "done" {
					done++
				}
			}
			if meta.CompletedCount != done {
				out = append(out, verifyFinding{
					Tag:     tag,
					Kind:    "counter-mismatch",
					Message: fmt.Sprintf("completedCount is %d, want %d", meta.CompletedCount, done),
				})
			}
		}
	}
	return out
}

// cycleFindings detects dependency cycles within a tag by depth-first
// search in file order. Dependencies on missing tasks are skipped here;
// they are already reported as missing-dependency findings.
func cycleFindings(tag string, tasks []backlog.Task) []verifyFinding {
	ids := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		ids[t.ID] = true
	}
	deps := make(map[string][]string, len(tasks))
	order := make([]string, 0, len(tasks))
	for _, t := range tasks {
		order = append(order, t.ID)
		for _, d := range t.Dependencies {
			if ids[d] {
				deps[t.ID] = append(deps[t.ID], d)
			}
		}
	}

	const (
		white = 0 // unvisited
		grey  = 1 // on the current DFS stack
		black = 2 // fully explored
	)
	color := make(map[string]int, len(tasks))
	var stack []string
	var out []verifyFinding
	var visit func(id string)
	visit = func(id string) {
		color[id] = grey
		stack = append(stack, id)
		for _, d := range deps[id] {
			switch color[d] {
			case white:
				visit(d)
			case grey:
				start := 0
				for i, s := range stack {
					if s == d {
						start = i
						break
					}
				}
				cycle := append(append([]string(nil), stack[start:]...), d)
				if len(cycle) == 2 {
					out = append(out, verifyFinding{
						Tag:     tag,
						Task:    id,
						Kind:    "dependency-cycle",
						Message: fmt.Sprintf("task %s depends on itself", id),
					})
				} else {
					out = append(out, verifyFinding{
						Tag:     tag,
						Task:    d,
						Kind:    "dependency-cycle",
						Message: "dependency cycle: " + strings.Join(cycle, " -> "),
					})
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
	}
	for _, id := range order {
		if color[id] == white {
			visit(id)
		}
	}
	return out
}

// milestoneFindings runs the §12.4 registry checks leniently, per entry,
// so one malformed entry does not hide the rest: registry shape, entry
// shape and status, name grammar, duplicate names, and task references
// that resolve to no registry entry.
func milestoneFindings(f *backlog.File, tag string, tasks []backlog.Task) []verifyFinding {
	var out []verifyFinding
	raw, hasRegistry := f.MilestonesRaw(tag)
	if !hasRegistry {
		// No registry: every assigned task is an unresolvable reference.
		for _, t := range tasks {
			if t.Milestone != "" {
				out = append(out, milestoneRefFinding(tag, t.ID, t.Milestone))
			}
		}
		return out
	}
	var elems []json.RawMessage
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return append(out, verifyFinding{
			Tag:     tag,
			Kind:    "milestone-registry",
			Message: "milestones registry is null, not an array",
		})
	}
	if err := json.Unmarshal(raw, &elems); err != nil {
		return append(out, verifyFinding{
			Tag:     tag,
			Kind:    "milestone-registry",
			Message: "milestones registry is not a JSON array",
		})
	}
	known := make(map[string]bool, len(elems))
	for i, elem := range elems {
		var entry struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(elem, &entry); err != nil || entry.Name == "" {
			out = append(out, verifyFinding{
				Tag:     tag,
				Kind:    "milestone-registry",
				Message: fmt.Sprintf("milestones entry %d is not an object with a name", i),
			})
			continue
		}
		if !backlog.ValidMilestoneStatuses[entry.Status] {
			out = append(out, verifyFinding{
				Tag:     tag,
				Kind:    "milestone-registry",
				Message: fmt.Sprintf("milestones entry %q has invalid status %q", entry.Name, entry.Status),
			})
		}
		if err := backlog.ValidateMilestoneName(entry.Name); err != nil {
			out = append(out, verifyFinding{
				Tag:     tag,
				Kind:    "milestone-registry",
				Message: fmt.Sprintf("milestones entry %d: %v", i, err),
			})
		}
		if known[entry.Name] {
			out = append(out, verifyFinding{
				Tag:     tag,
				Kind:    "milestone-duplicate",
				Message: fmt.Sprintf("milestone %q is defined more than once", entry.Name),
			})
		}
		known[entry.Name] = true
	}
	for _, t := range tasks {
		if t.Milestone != "" && !known[t.Milestone] {
			out = append(out, milestoneRefFinding(tag, t.ID, t.Milestone))
		}
	}
	return out
}

func milestoneRefFinding(tag, taskID, name string) verifyFinding {
	return verifyFinding{
		Tag:     tag,
		Task:    taskID,
		Kind:    "milestone-ref",
		Message: fmt.Sprintf("task %s: milestone %q does not exist in the registry", taskID, name),
	}
}

// readRefBacklog reads the backlog file as it exists at ref via
// `git show <ref>:<path>` with the path relative to the git worktree root.
// `git rev-parse --show-toplevel` resolves the root, which also handles
// linked worktrees where .git is a file rather than a directory.
func readRefBacklog(ref, path string) (*backlog.File, error) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	top, err := gitRun(filepath.Dir(path), "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("locating git root for %s: %s", path, err)
	}
	root := strings.TrimSpace(string(top))
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%s is outside the git worktree %s", path, root)
	}
	rel = filepath.ToSlash(rel)
	candidates := []string{rel}
	switch rel {
	case ".tasks/tasks/tasks.json":
		candidates = append(candidates, ".noisetask/tasks.json", ".taskmaster/tasks/tasks.json")
	case ".noisetask/tasks.json", ".taskmaster/tasks/tasks.json":
		candidates = append(candidates, ".tasks/tasks/tasks.json")
	}
	var data []byte
	var showErr error
	var spec string
	for _, candidate := range candidates {
		spec = ref + ":" + candidate
		data, showErr = gitRun(root, "show", spec)
		if showErr == nil {
			break
		}
	}
	if showErr != nil {
		return nil, fmt.Errorf("git show %s: %s", spec, showErr)
	}
	f, err := backlog.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", spec, err)
	}
	return f, nil
}

func gitRun(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	return out.Bytes(), nil
}

func unionTags(old, cur *backlog.File) []string {
	seen := make(map[string]bool)
	var out []string
	for _, lists := range [][]string{old.Tags(), cur.Tags()} {
		for _, tag := range lists {
			if !seen[tag] {
				seen[tag] = true
				out = append(out, tag)
			}
		}
	}
	return out
}

// diffAgainst compares the ref's backlog (old) with the working copy (cur).
// A task present in old but absent from cur is a finding — removal is the
// fatal direction. Additions and field-level changes are reported for
// confirmation but are not findings.
func diffAgainst(old, cur *backlog.File, tags []string, ref string) ([]verifyFinding, []verifyChange) {
	var findings []verifyFinding
	var changes []verifyChange
	for _, tag := range tags {
		oldHas, curHas := old.HasTag(tag), cur.HasTag(tag)
		var oldTasks, curTasks []backlog.Task
		if oldHas {
			oldTasks, _ = old.Tasks(tag)
		}
		if curHas {
			curTasks, _ = cur.Tasks(tag)
		}
		if oldHas && !curHas {
			findings = append(findings, verifyFinding{
				Tag:     tag,
				Kind:    "tag-removed",
				Message: fmt.Sprintf("tag %q removed (present in %s with %d tasks)", tag, ref, len(oldTasks)),
			})
		}
		if !oldHas && curHas {
			changes = append(changes, verifyChange{
				Tag:     tag,
				Kind:    "tag-added",
				Message: fmt.Sprintf("tag %q added (not in %s)", tag, ref),
			})
		}

		curByID := make(map[string]backlog.Task, len(curTasks))
		for _, t := range curTasks {
			curByID[t.ID] = t
		}
		oldByID := make(map[string]backlog.Task, len(oldTasks))
		for _, t := range oldTasks {
			oldByID[t.ID] = t
		}

		for _, ot := range oldTasks {
			nt, ok := curByID[ot.ID]
			if !ok {
				findings = append(findings, verifyFinding{
					Tag:     tag,
					Task:    ot.ID,
					Kind:    "task-removed",
					Message: fmt.Sprintf("task %s %q removed (present in %s)", ot.ID, ot.Title, ref),
				})
				continue
			}
			findings = append(findings, removedSubtaskFindings(tag, ot, nt, ref)...)
			changes = append(changes, fieldChanges(tag, ot, nt)...)
		}
		for _, nt := range curTasks {
			if _, ok := oldByID[nt.ID]; !ok {
				changes = append(changes, verifyChange{
					Tag:     tag,
					Task:    nt.ID,
					Kind:    "task-added",
					Message: fmt.Sprintf("task %s %q added (not in %s)", nt.ID, nt.Title, ref),
				})
			}
		}
	}
	return findings, changes
}

type verifySubtaskIdentity struct {
	ID    string
	Title string
}

func removedSubtaskFindings(tag string, old, cur backlog.Task, ref string) []verifyFinding {
	current := make(map[string]bool, len(cur.Subtasks))
	for _, raw := range cur.Subtasks {
		if subtask, ok := decodeVerifySubtask(raw); ok {
			current[subtask.ID] = true
		}
	}
	var findings []verifyFinding
	for _, raw := range old.Subtasks {
		subtask, ok := decodeVerifySubtask(raw)
		if !ok || current[subtask.ID] {
			continue
		}
		qualified := old.ID + "." + subtask.ID
		findings = append(findings, verifyFinding{
			Tag:     tag,
			Task:    qualified,
			Kind:    "subtask-removed",
			Message: fmt.Sprintf("subtask %s %q removed (present in %s)", qualified, subtask.Title, ref),
		})
	}
	return findings
}

func decodeVerifySubtask(raw json.RawMessage) (verifySubtaskIdentity, bool) {
	var value struct {
		ID    json.RawMessage `json:"id"`
		Title string          `json:"title"`
	}
	if err := json.Unmarshal(raw, &value); err != nil || len(value.ID) == 0 {
		return verifySubtaskIdentity{}, false
	}
	var id string
	if err := json.Unmarshal(value.ID, &id); err != nil {
		var number json.Number
		decoder := json.NewDecoder(bytes.NewReader(value.ID))
		decoder.UseNumber()
		if err := decoder.Decode(&number); err != nil {
			return verifySubtaskIdentity{}, false
		}
		id = number.String()
	}
	return verifySubtaskIdentity{ID: id, Title: value.Title}, true
}

func fieldChanges(tag string, old, cur backlog.Task) []verifyChange {
	pairs := []struct{ name, oldVal, newVal string }{
		{"title", old.Title, cur.Title},
		{"description", old.Description, cur.Description},
		{"details", old.Details, cur.Details},
		{"status", old.Status, cur.Status},
		{"priority", old.Priority, cur.Priority},
		{"milestone", old.Milestone, cur.Milestone},
		{"dependencies", strings.Join(old.Dependencies, ","), strings.Join(cur.Dependencies, ",")},
		{"subtasks", compactJSONList(old.Subtasks), compactJSONList(cur.Subtasks)},
		{"updatedAt", old.UpdatedAt, cur.UpdatedAt},
	}
	var out []verifyChange
	for _, p := range pairs {
		if p.oldVal == p.newVal {
			continue
		}
		out = append(out, verifyChange{
			Tag:     tag,
			Task:    old.ID,
			Kind:    "field-change",
			Field:   p.name,
			Old:     p.oldVal,
			New:     p.newVal,
			Message: fmt.Sprintf("task %s %s: %q -> %q", old.ID, p.name, clip(p.oldVal), clip(p.newVal)),
		})
	}
	return out
}

func compactJSONList(raws []json.RawMessage) string {
	if len(raws) == 0 {
		return "[]"
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, r := range raws {
		if i > 0 {
			buf.WriteByte(',')
		}
		var c bytes.Buffer
		if err := json.Compact(&c, r); err != nil {
			buf.Write(bytes.TrimSpace(r))
		} else {
			buf.Write(c.Bytes())
		}
	}
	buf.WriteByte(']')
	return buf.String()
}

func clip(s string) string {
	const max = 48
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-3]) + "..."
}

func emitReport(r *verifyReport, jsonOut bool, stdout, stderr io.Writer) int {
	r.Clean = len(r.Findings) == 0
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
			fmt.Fprintf(stderr, "verify: encoding report: %v\n", err)
			return 2
		}
	} else {
		printHumanReport(stdout, r)
	}
	if r.Clean {
		return 0
	}
	return 1
}

func printHumanReport(w io.Writer, r *verifyReport) {
	for _, f := range r.Findings {
		if f.Tag != "" {
			fmt.Fprintf(w, "finding: %s: %s\n", f.Tag, f.Message)
		} else {
			fmt.Fprintf(w, "finding: %s\n", f.Message)
		}
	}
	added, changed := 0, 0
	for _, c := range r.Changes {
		label := "changed"
		if c.Kind == "task-added" || c.Kind == "tag-added" {
			label = "added"
			added++
		} else {
			changed++
		}
		fmt.Fprintf(w, "%s: %s: %s\n", label, c.Tag, c.Message)
	}
	if r.Clean {
		fmt.Fprintf(w, "ok: %d task(s) across %d tag(s) verified\n", r.taskCount, r.tagCount)
		if r.Against != "" {
			fmt.Fprintf(w, "against %s: no tasks removed (%d added, %d changed)\n", r.Against, added, changed)
		}
	} else {
		fmt.Fprintf(w, "verify: %d finding(s)\n", len(r.Findings))
	}
}
