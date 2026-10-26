package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// The write commands (SPEC §4): init, add, set-status, append, remove.
// Implicit operation reads and writes the §8.1 SQLite authority; --file
// operates on the standalone fixture document through the codec's atomic
// write path (SPEC §3) and supports no claims, revisions, or feed.

func cmdInit(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("init", stderr, &cf)
	if code, ok := parseFlags(fs, stdout, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "noise-tasks init: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	tag := cf.tag
	if tag == "" {
		tag = "main"
	}

	var target string
	if cf.file != "" {
		// Fixture bootstrap: create the named standalone JSON document only.
		path, err := fixturePathAbsent(cf.file)
		if err != nil {
			return fail(stderr, err)
		}
		if _, err := backlog.InitFileWithParents(path, tag); err != nil {
			return fail(stderr, err)
		}
		target = path
	} else {
		// §8.1 bootstrap: create the database with an empty main tag. A legacy
		// JSON live store refuses like every normal command (§8.6): the cutover
		// is explicit, never an implicit side effect of init.
		_, boundary, err := backlog.ProjectContext()
		if err != nil {
			return fail(stderr, err)
		}
		path, _ := store.Path(boundary)
		if err := migrationProjectGate(boundary, path); err != nil {
			return fail(stderr, err)
		}
		if err := outsideGitRehomingGate(boundary); err != nil {
			return fail(stderr, err)
		}
		if legacy := legacyStorePresent(path); legacy {
			return fail(stderr, errLegacyJSONStore)
		}
		if info, statErr := statRegular(path); statErr != nil {
			return fail(stderr, statErr)
		} else if info != nil {
			return fail(stderr, fmt.Errorf("backlog already exists at %s", path))
		}
		owner, err := derivedOwnerBoundary(boundary)
		if err != nil {
			return fail(stderr, err)
		}
		// §8.2: init mints the immutable family identity at creation, in the
		// same transaction that stamps the current store schema, and publishes the
		// complete database atomically.
		db, err := store.CreateWithOwner(path, owner)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return fail(stderr, fmt.Errorf("backlog already exists at %s", path))
			}
			return fail(stderr, err)
		}
		if err := db.EnsureTag(tag); err != nil {
			db.Close()
			return fail(stderr, err)
		}
		db.Close()
		target = path
	}

	abs, err := canonicalizePath(target)
	if err != nil {
		abs = target
	}

	if cf.json {
		out := struct {
			Path string `json:"path"`
			Tag  string `json:"tag"`
		}{abs, tag}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "created %s with empty tag %q\n", abs, tag)
	return 0
}

// fixturePathAbsent resolves the --file target for init, refusing when the
// document already exists.
func fixturePathAbsent(file string) (string, error) {
	abs, err := canonicalizePath(file)
	if err != nil {
		return "", err
	}
	info, err := statRegular(abs)
	if err != nil {
		return "", err
	}
	if info != nil {
		return "", fmt.Errorf("backlog already exists at %s", abs)
	}
	return abs, nil
}

func cmdAdd(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("add", stderr, &cf)
	titleF := fs.String("title", "", "task title (required)")
	descF := fs.String("description", "", "task description")
	detailsF := fs.String("details", "", "task details")
	priorityF := fs.String("priority", "", "high|medium|low (default: absent, treated as medium)")
	depsF := fs.String("dependencies", "", "comma-separated ids of prerequisite tasks")
	milestoneF := fs.String("milestone", "", "assign the new task to this milestone")
	linkF := fs.String("link", "", "set-once external link <family>/<repository>#<issue>")
	if code, ok := parseFlags(fs, stdout, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "noise-tasks add: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *titleF == "" {
		fmt.Fprintln(stderr, "noise-tasks add: --title is required")
		return 2
	}
	if *priorityF != "" && !backlog.ValidPriorities[*priorityF] {
		fmt.Fprintf(stderr, "noise-tasks add: unknown priority %q\n", *priorityF)
		return 2
	}
	deps := splitDeps(*depsF)
	var link *backlog.ExternalLink
	if *linkF != "" {
		parsed, err := backlog.ParseExternalLinkText(*linkF)
		if err != nil {
			return reportLinkError(stderr, cf.json, err)
		}
		link = parsed
	}

	var id string
	if cf.file != "" {
		err := mutateFixture(&cf, func(f *backlog.File, tag string) error {
			for _, dep := range deps {
				if _, err := f.Task(tag, dep); err != nil {
					return fmt.Errorf("dependency %s does not exist in tag %q", dep, tag)
				}
			}
			if *milestoneF != "" {
				if err := backlog.ValidateMilestoneName(*milestoneF); err != nil {
					return err
				}
				entries, err := f.Milestones(tag)
				if err != nil {
					return err
				}
				if !milestoneKnown(entries, *milestoneF) {
					return &backlog.ErrMilestoneNotFound{Tag: tag, Name: *milestoneF}
				}
			}
			// A link already carried by a task of this tag makes the add a
			// replay (SPEC §4): return the existing id and write nothing.
			// A holder in another tag is a typed conflict.
			if link != nil {
				if holderTag, existingID, ok := f.FindTaskByLink(*link); ok {
					if holderTag != tag {
						return &backlog.ErrLinkExists{Link: *link, ExistingTag: holderTag, ExistingID: existingID}
					}
					id = existingID
					return nil
				}
			}
			var err error
			id, err = f.AddTask(tag, backlog.Task{
				Title:        *titleF,
				Description:  *descF,
				Details:      *detailsF,
				Status:       "pending",
				Priority:     *priorityF,
				Dependencies: deps,
				Milestone:    *milestoneF,
				Link:         link,
			})
			return err
		})
		if err != nil {
			// A link conflict (cross-tag replay, set-once) is typed
			// (exit 3); other failures keep the milestone mapping.
			var linkExists *backlog.ErrLinkExists
			if errors.As(err, &linkExists) {
				return reportLinkError(stderr, cf.json, err)
			}
			return reportMilestoneError(stderr, cf.json, err)
		}
	} else {
		ps, tag, err := resolveStoreTag(&cf)
		if err != nil {
			return fail(stderr, err)
		}
		defer ps.db.Close()
		if *milestoneF != "" {
			if err := backlog.ValidateMilestoneName(*milestoneF); err != nil {
				return reportMilestoneError(stderr, cf.json, err)
			}
		}
		// AddTask validates the milestone against the registry inside its
		// immediate transaction (SPEC §8.2).
		id, err = ps.db.AddTask(tag, &store.Task{
			Title:        *titleF,
			Description:  *descF,
			Details:      *detailsF,
			Status:       "pending",
			Priority:     *priorityF,
			Dependencies: deps,
			Milestone:    *milestoneF,
			Link:         link,
		})
		if err != nil {
			// A cross-tag link replay is a typed link conflict (exit 3);
			// every other failure keeps the milestone reporter's mapping.
			var linkExists *store.LinkExistsConflict
			if errors.As(err, &linkExists) {
				return reportLinkError(stderr, cf.json, err)
			}
			return reportMilestoneError(stderr, cf.json, err)
		}
	}

	if cf.json {
		out := struct {
			ID string `json:"id"`
		}{id}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintln(stdout, id)
	return 0
}

func splitDeps(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func cmdSetStatus(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("set-status", stderr, &cf)
	var lf liveFlags
	lf.register(fs)
	lf.registerRemovedKeepWorktree(fs)
	var rf taskMutationRunFlags
	rf.register(fs)
	if code, ok := parseFlags(fs, stdout, args); !ok {
		return code
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: noise-tasks set-status [options] <id> <status>")
		return 2
	}
	id, status := fs.Arg(0), fs.Arg(1)
	if !backlog.ValidStatuses[status] {
		return fail(stderr, fmt.Errorf("unknown status %q", status))
	}
	// §9.4 v5 cutover: --keep-worktree is removed; the guided usage error
	// replaces the keep flag.
	if lf.keepWorktree {
		return worktreeCutoverRefusal(stderr, "set-status: --keep-worktree")
	}

	if flagWasSet(fs, "file") {
		if code := lf.rejectOnFixturePath(stderr, "set-status"); code >= 0 {
			return code
		}
		if code := rf.rejectOnFixturePath(fs, stderr, "set-status"); code >= 0 {
			return code
		}
		var previous string
		err := mutateFixture(&cf, func(f *backlog.File, tag string) error {
			var err error
			previous, err = f.SetStatusByReference(tag, id, status)
			return err
		})
		if err != nil {
			return fail(stderr, err)
		}
		return writeStatusResult(stdout, stderr, cf.json, id, status, previous)
	}

	gate, ok := taskMutationGate(fs, &lf, &rf, stderr, "set-status")
	if !ok {
		return 2
	}
	ps, err := openProjectStore()
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()
	if !lf.requireRevision(stderr) {
		return 2
	}
	tag, err := ps.db.ResolveTag(cf.tag)
	if err != nil {
		return fail(stderr, err)
	}
	previous, err := ps.db.SetStatus(tag, id, status, lf.revision, gate)
	if err != nil {
		var sw *store.StaleWriteConflict
		if errors.As(err, &sw) {
			return writeLiveConflict(stderr, cf.json, staleConflictOut(sw), sw.Error())
		}
		var cc *store.ClaimConflict
		if errors.As(err, &cc) {
			return writeLiveConflict(stderr, cf.json, claimConflictOut(cc), cc.Error())
		}
		var rc *store.RunConflict
		if errors.As(err, &rc) {
			return writeLiveConflict(stderr, cf.json, runConflictOut(rc), rc.Error())
		}
		return fail(stderr, err)
	}
	return writeStatusResult(stdout, stderr, cf.json, id, status, previous)
}

func writeStatusResult(stdout, stderr io.Writer, jsonOut bool, id, status, previous string) int {
	if jsonOut {
		out := struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			Previous string `json:"previous"`
		}{id, status, previous}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "task %s: %s -> %s\n", id, previous, status)
	return 0
}

func cmdAppend(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("append", stderr, &cf)
	var lf liveFlags
	lf.register(fs)
	var rf taskMutationRunFlags
	rf.register(fs)
	if code, ok := parseFlags(fs, stdout, args); !ok {
		return code
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: noise-tasks append [options] <id> <text>")
		return 2
	}
	id, text := fs.Arg(0), fs.Arg(1)

	var stamp string
	if flagWasSet(fs, "file") {
		if code := lf.rejectOnFixturePath(stderr, "append"); code >= 0 {
			return code
		}
		if code := rf.rejectOnFixturePath(fs, stderr, "append"); code >= 0 {
			return code
		}
		err := mutateFixture(&cf, func(f *backlog.File, tag string) error {
			idx, err := f.TaskIndex(tag, id)
			if err != nil {
				return err
			}
			t, err := f.Task(tag, id)
			if err != nil {
				return err
			}
			stamp = time.Now().UTC().Format(time.RFC3339)
			details := t.Details + fmt.Sprintf("\n<info added %s>\n%s\n</info>", stamp, text)
			return f.WriteMinimal(tag, idx, map[string]json.RawMessage{"details": backlog.MarshalString(details)}, "")
		})
		if err != nil {
			return fail(stderr, err)
		}
	} else {
		gate, ok := taskMutationGate(fs, &lf, &rf, stderr, "append")
		if !ok {
			return 2
		}
		ps, tag, err := resolveStoreTag(&cf)
		if err != nil {
			return fail(stderr, err)
		}
		defer ps.db.Close()
		if !lf.requireRevision(stderr) {
			return 2
		}
		stamp, err = ps.db.AppendDetails(tag, id, text, lf.revision, gate)
		if err != nil {
			var sw *store.StaleWriteConflict
			if errors.As(err, &sw) {
				return writeLiveConflict(stderr, cf.json, staleConflictOut(sw), sw.Error())
			}
			var cc *store.ClaimConflict
			if errors.As(err, &cc) {
				return writeLiveConflict(stderr, cf.json, claimConflictOut(cc), cc.Error())
			}
			var rc *store.RunConflict
			if errors.As(err, &rc) {
				return writeLiveConflict(stderr, cf.json, runConflictOut(rc), rc.Error())
			}
			return fail(stderr, err)
		}
	}

	if cf.json {
		out := struct {
			ID        string `json:"id"`
			Timestamp string `json:"timestamp"`
		}{id, stamp}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "appended to task %s\n", id)
	return 0
}

func cmdRemove(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("remove", stderr, &cf)
	var lf liveFlags
	lf.register(fs)
	var rf taskMutationRunFlags
	rf.register(fs)
	if code, ok := parseFlags(fs, stdout, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks remove [options] <id>")
		return 2
	}
	id := fs.Arg(0)
	// --force is a top-level-only grammar choice, not a store-dependent
	// validation. Reject a hierarchical target before resolving either the
	// fixture or authoritative store (SPEC §4/§6).
	if _, subtaskID, err := backlog.ParseTaskReference(id); err == nil && subtaskID != "" && lf.force {
		fmt.Fprintf(stderr, "noise-tasks remove: --force is not supported for hierarchical task %s\n", id)
		return 2
	}

	var stripped []string
	if flagWasSet(fs, "file") {
		if code := lf.rejectOnFixturePath(stderr, "remove"); code >= 0 {
			return code
		}
		if code := rf.rejectOnFixturePath(fs, stderr, "remove"); code >= 0 {
			return code
		}
		err := mutateFixture(&cf, func(f *backlog.File, tag string) error {
			task, subtask, err := f.ResolveTaskReference(tag, id)
			if err != nil {
				return err
			}
			if subtask != nil {
				stripped, err = f.RemoveTaskByReference(tag, id, lf.force)
				return err
			}
			tasks, err := f.Tasks(tag)
			if err != nil {
				return err
			}
			var dependents []string
			for _, t := range tasks {
				if t.ID == task.ID {
					continue
				}
				for _, d := range t.Dependencies {
					if d == task.ID {
						dependents = append(dependents, t.ID)
						break
					}
				}
			}
			if len(dependents) > 0 && !lf.force {
				return fmt.Errorf(
					"task %s is a dependency of task(s) %s (use --force to remove it and strip the references)",
					id, strings.Join(dependents, ", "))
			}
			stripped, err = f.RemoveTaskByReference(tag, id, lf.force)
			return err
		})
		if err != nil {
			return fail(stderr, err)
		}
	} else {
		gate, ok := taskMutationGate(fs, &lf, &rf, stderr, "remove")
		if !ok {
			return 2
		}
		ps, tag, err := resolveStoreTag(&cf)
		if err != nil {
			return fail(stderr, err)
		}
		defer ps.db.Close()
		if !lf.requireRevision(stderr) {
			return 2
		}
		stripped, err = ps.db.RemoveTask(tag, id, lf.revision, lf.force, gate)
		if err != nil {
			var sw *store.StaleWriteConflict
			if errors.As(err, &sw) {
				return writeLiveConflict(stderr, cf.json, staleConflictOut(sw), sw.Error())
			}
			var cc *store.ClaimConflict
			if errors.As(err, &cc) {
				return writeLiveConflict(stderr, cf.json, claimConflictOut(cc), cc.Error())
			}
			var rc *store.RunConflict
			if errors.As(err, &rc) {
				return writeLiveConflict(stderr, cf.json, runConflictOut(rc), rc.Error())
			}
			var wc *store.WorktreeConflict
			if errors.As(err, &wc) {
				return writeLiveConflict(stderr, cf.json, worktreeConflictOut(wc), wc.Error())
			}
			return fail(stderr, err)
		}
	}
	if stripped == nil {
		stripped = []string{}
	}

	if cf.json {
		out := struct {
			ID       string   `json:"id"`
			Stripped []string `json:"stripped"`
		}{id, stripped}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "removed task %s\n", id)
	if len(stripped) > 0 {
		fmt.Fprintf(stdout, "stripped dangling reference(s) to %s from task(s) %s\n",
			id, strings.Join(stripped, ", "))
	}
	return 0
}
