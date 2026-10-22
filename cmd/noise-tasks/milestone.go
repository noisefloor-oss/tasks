package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// The §12 milestone command group and set-milestone. Registry commands
// mutate the project database or, with --file, the standalone fixture
// document; set-milestone is a §1 field mutation with full §8.2 CAS on the
// database path.

// cmdMilestone dispatches the §12 `milestone` subcommands.
func cmdMilestone(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: noise-tasks milestone <add|list|close|reopen|remove> [options] <name>")
		return 2
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "add":
		return cmdMilestoneAdd(rest, stdout, stderr)
	case "list":
		return cmdMilestoneList(rest, stdout, stderr)
	case "close", "reopen":
		return cmdMilestoneFlip(sub, rest, stdout, stderr)
	case "remove":
		return cmdMilestoneRemove(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "noise-tasks milestone: unknown subcommand %q\n", sub)
		return 2
	}
}

// reportMilestoneError maps codec/store milestone errors to §6 exit codes:
// typed conflicts (duplicate, in-use) exit 3, usage errors (invalid name,
// unknown name) exit 2, everything else exits 1.
func reportMilestoneError(stderr io.Writer, jsonOut bool, err error) int {
	var exists *backlog.ErrMilestoneExists
	if errors.As(err, &exists) {
		return writeLiveConflict(stderr, jsonOut, milestoneConflictOut("milestone-exists", exists.Tag, exists.Name), exists.Error())
	}
	var inUse *backlog.ErrMilestoneInUse
	if errors.As(err, &inUse) {
		return writeLiveConflict(stderr, jsonOut, milestoneInUseOut(inUse), inUse.Error())
	}
	var notFound *backlog.ErrMilestoneNotFound
	if errors.As(err, &notFound) {
		return milestoneUsageError(stderr, notFound.Error())
	}
	var badName *backlog.ErrMilestoneName
	if errors.As(err, &badName) {
		return milestoneUsageError(stderr, badName.Error())
	}
	return fail(stderr, err)
}

func milestoneUsageError(stderr io.Writer, msg string) int {
	fmt.Fprintln(stderr, "noise-tasks: "+msg)
	return 2
}

func milestoneConflictOut(kind, tag, name string) any {
	return struct {
		Kind string `json:"kind"`
		Tag  string `json:"tag"`
		Name string `json:"name"`
	}{kind, tag, name}
}

func milestoneInUseOut(e *backlog.ErrMilestoneInUse) any {
	return struct {
		Kind  string   `json:"kind"`
		Tag   string   `json:"tag"`
		Name  string   `json:"name"`
		Tasks []string `json:"tasks"`
	}{"milestone-in-use", e.Tag, e.Name, e.TaskIDs}
}

func cmdMilestoneAdd(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("milestone add", stderr, &cf)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks milestone add [options] <name>")
		return 2
	}
	name := fs.Arg(0)

	var err error
	if cf.file != "" {
		err = mutateFixture(&cf, func(f *backlog.File, tag string) error {
			return f.AddMilestone(tag, name)
		})
	} else {
		var ps *projectStore
		var tag string
		ps, tag, err = resolveStoreTag(&cf)
		if err == nil {
			defer ps.db.Close()
			err = ps.db.AddMilestone(tag, name)
		}
	}
	if err != nil {
		return reportMilestoneError(stderr, cf.json, err)
	}

	if cf.json {
		return writeMilestoneJSON(stdout, stderr, struct {
			Name string `json:"name"`
		}{name})
	}
	fmt.Fprintln(stdout, name)
	return 0
}

// milestoneRollup is one `milestone list` entry.
type milestoneRollup struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Order  int    `json:"order"`
	Total  int    `json:"total"`
	Done   int    `json:"done"`
}

func cmdMilestoneList(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("milestone list", stderr, &cf)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: noise-tasks milestone list [options]")
		return 2
	}

	build := func(entries []backlog.MilestoneEntry, total, done map[string]int) []milestoneRollup {
		out := make([]milestoneRollup, 0, len(entries))
		for i, e := range entries {
			out = append(out, milestoneRollup{
				Name:   e.Name,
				Status: e.Status,
				Order:  i,
				Total:  total[e.Name],
				Done:   done[e.Name],
			})
		}
		return out
	}

	var rollup []milestoneRollup
	var tag string
	if cf.file != "" {
		f, ftag, err := loadFixture(&cf)
		if err != nil {
			return fail(stderr, err)
		}
		tag = ftag
		entries, err := f.Milestones(tag)
		if err != nil {
			return reportMilestoneError(stderr, cf.json, err)
		}
		total, done, err := f.MilestoneCountsForList(tag)
		if err != nil {
			return fail(stderr, err)
		}
		rollup = build(entries, total, done)
	} else {
		ps, rtag, err := resolveStoreTag(&cf)
		if err != nil {
			return fail(stderr, err)
		}
		defer ps.db.Close()
		tag = rtag
		entries, err := ps.db.MilestoneEntries(tag)
		if err != nil {
			return reportMilestoneError(stderr, cf.json, err)
		}
		total, done, err := ps.db.MilestoneCountsForList(tag)
		if err != nil {
			return fail(stderr, err)
		}
		rollup = build(entries, total, done)
	}

	if cf.json {
		return writeMilestoneJSON(stdout, stderr, struct {
			Tag        string            `json:"tag"`
			Milestones []milestoneRollup `json:"milestones"`
		}{tag, rollup})
	}
	if len(rollup) == 0 {
		fmt.Fprintln(stdout, "no milestones")
		return 0
	}
	for _, m := range rollup {
		fmt.Fprintf(stdout, "%d\t%s\t%s\t%d/%d\n", m.Order, m.Name, m.Status, m.Done, m.Total)
	}
	return 0
}

func writeMilestoneJSON(stdout, stderr io.Writer, value any) int {
	if err := writeJSON(stdout, value); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func cmdMilestoneFlip(sub string, args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("milestone "+sub, stderr, &cf)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "usage: noise-tasks milestone %s [options] <name>\n", sub)
		return 2
	}
	name := fs.Arg(0)
	status := "closed"
	if sub == "reopen" {
		status = "open"
	}

	var remaining int
	var err error
	if cf.file != "" {
		err = mutateFixture(&cf, func(f *backlog.File, tag string) error {
			if err := f.SetMilestoneStatus(tag, name, status); err != nil {
				return err
			}
			remaining = f.MilestoneRemaining(tag, name)
			return nil
		})
	} else {
		var ps *projectStore
		var tag string
		ps, tag, err = resolveStoreTag(&cf)
		if err == nil {
			defer ps.db.Close()
			remaining, err = ps.db.SetMilestoneStatus(tag, name, status)
		}
	}
	if err != nil {
		return reportMilestoneError(stderr, cf.json, err)
	}

	if cf.json {
		return writeMilestoneJSON(stdout, stderr, struct {
			Name      string `json:"name"`
			Status    string `json:"status"`
			Remaining int    `json:"remaining"`
		}{name, status, remaining})
	}
	fmt.Fprintf(stdout, "milestone %s: %s (remaining tasks: %d)\n", name, status, remaining)
	return 0
}

func cmdMilestoneRemove(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("milestone remove", stderr, &cf)
	forceF := fs.Bool("force", false, "also clear the milestone from referencing tasks")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks milestone remove [options] <name>")
		return 2
	}
	name := fs.Arg(0)

	var cleared []string
	var err error
	if cf.file != "" {
		err = mutateFixture(&cf, func(f *backlog.File, tag string) error {
			var ferr error
			cleared, ferr = f.RemoveMilestone(tag, name, *forceF)
			return ferr
		})
	} else {
		var ps *projectStore
		var tag string
		ps, tag, err = resolveStoreTag(&cf)
		if err == nil {
			defer ps.db.Close()
			cleared, err = ps.db.RemoveMilestone(tag, name, *forceF)
		}
	}
	if err != nil {
		// A force removal that would clear a claimed task's milestone field
		// is a typed claim conflict (§8.3), ahead of the generic mapper.
		var cc *store.ClaimConflict
		if errors.As(err, &cc) {
			return writeLiveConflict(stderr, cf.json, claimConflictOut(cc), cc.Error())
		}
		return reportMilestoneError(stderr, cf.json, err)
	}

	if cf.json {
		return writeMilestoneJSON(stdout, stderr, struct {
			Name    string   `json:"name"`
			Cleared []string `json:"cleared"`
		}{name, cleared})
	}
	if len(cleared) > 0 {
		fmt.Fprintf(stdout, "removed milestone %s; cleared from tasks %v\n", name, cleared)
	} else {
		fmt.Fprintf(stdout, "removed milestone %s\n", name)
	}
	return 0
}

func cmdSetMilestone(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("set-milestone", stderr, &cf)
	var lf liveFlags
	lf.register(fs)
	var rf taskMutationRunFlags
	rf.register(fs)
	clearF := fs.Bool("clear", false, "remove the task's milestone assignment")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	var id string
	name := ""
	switch {
	case fs.NArg() == 2 && *clearF:
		fmt.Fprintln(stderr, "noise-tasks set-milestone: supply either <name> or --clear, not both")
		return 2
	case fs.NArg() == 2:
		id, name = fs.Arg(0), fs.Arg(1)
	case fs.NArg() == 1 && *clearF:
		id = fs.Arg(0)
	default:
		fmt.Fprintln(stderr, "usage: noise-tasks set-milestone [options] <id> (<name>|--clear)")
		return 2
	}
	if id == "" {
		fmt.Fprintln(stderr, "usage: noise-tasks set-milestone [options] <id> (<name>|--clear)")
		return 2
	}
	if _, subID, refErr := backlog.ParseTaskReference(id); refErr != nil || subID != "" {
		fmt.Fprintln(stderr, "noise-tasks set-milestone: subtask ids do not carry a milestone")
		return 2
	}

	if flagWasSet(fs, "file") {
		if code := lf.rejectOnFixturePath(stderr, "set-milestone"); code >= 0 {
			return code
		}
		if code := rf.rejectOnFixturePath(fs, stderr, "set-milestone"); code >= 0 {
			return code
		}
		err := mutateFixture(&cf, func(f *backlog.File, tag string) error {
			return f.SetTaskMilestone(tag, id, name)
		})
		if err != nil {
			return reportMilestoneError(stderr, cf.json, err)
		}
	} else {
		gate, ok := taskMutationGate(fs, &lf, &rf, stderr, "set-milestone")
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
		_, err = ps.db.SetTaskMilestone(tag, id, name, lf.revision, gate)
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
			return reportMilestoneError(stderr, cf.json, err)
		}
	}

	if cf.json {
		assigned := name
		if *clearF {
			assigned = ""
		}
		return writeMilestoneJSON(stdout, stderr, struct {
			ID        string `json:"id"`
			Milestone string `json:"milestone"`
		}{id, assigned})
	}
	if *clearF {
		fmt.Fprintf(stdout, "task %s: milestone cleared\n", id)
	} else {
		fmt.Fprintf(stdout, "task %s: milestone %s\n", id, name)
	}
	return 0
}

// milestoneKnown reports whether name is a registry entry.
func milestoneKnown(entries []backlog.MilestoneEntry, name string) bool {
	for _, e := range entries {
		if e.Name == name {
			return true
		}
	}
	return false
}
