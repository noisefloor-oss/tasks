package main

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// listOut is the machine-readable shape of `list --json`.
type listOut struct {
	Tag    string         `json:"tag"`
	Total  int            `json:"total"`
	Counts map[string]int `json:"counts"`
	Tasks  []taskOut      `json:"tasks"`
}

func cmdList(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("list", stderr, &cf)
	statusF := fs.String("status", "", "filter by status")
	priorityF := fs.String("priority", "", "filter by priority")
	milestoneF := fs.String("milestone", "", "filter by milestone")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "noise-tasks list: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *statusF != "" && !backlog.ValidStatuses[*statusF] {
		fmt.Fprintf(stderr, "noise-tasks list: unknown status %q\n", *statusF)
		return 2
	}
	if *priorityF != "" && !backlog.ValidPriorities[*priorityF] {
		fmt.Fprintf(stderr, "noise-tasks list: unknown priority %q\n", *priorityF)
		return 2
	}
	if *milestoneF != "" {
		if err := backlog.ValidateMilestoneName(*milestoneF); err != nil {
			return reportMilestoneError(stderr, cf.json, err)
		}
	}

	if cf.file != "" {
		f, tag, err := loadFixture(&cf)
		if err != nil {
			return fail(stderr, err)
		}
		entries, err := f.Milestones(tag)
		if err != nil {
			return reportMilestoneError(stderr, cf.json, err)
		}
		if code := requireKnownMilestone(entries, *milestoneF, stderr); code != 0 {
			return code
		}
		tasks, err := f.Tasks(tag)
		if err != nil {
			return fail(stderr, err)
		}
		return renderList(stdout, stderr, tag, tasks, nil, cf.json, *statusF, *priorityF, *milestoneF)
	}

	ps, tag, err := resolveStoreTag(&cf)
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()
	entries, err := ps.db.MilestoneEntries(tag)
	if err != nil {
		return reportMilestoneError(stderr, cf.json, err)
	}
	if code := requireKnownMilestone(entries, *milestoneF, stderr); code != 0 {
		return code
	}
	rows, err := ps.db.ListTasks(tag)
	if err != nil {
		return fail(stderr, err)
	}
	return renderList(stdout, stderr, tag, nil, rows, cf.json, *statusF, *priorityF, *milestoneF)
}

// renderList filters and prints one tag's tasks. Exactly one of tasks (the
// fixture surface, which emits the plain taskOut shape like next) or rows
// (the SQLite authority, which emits liveTaskOut with revision and claim) is
// non-nil.
func renderList(stdout, stderr io.Writer, tag string, tasks []backlog.Task, rows []*store.Task, jsonOut bool, statusF, priorityF, milestoneF string) int {
	counts := make(map[string]int)
	keep := func(status, priority, milestone string) bool {
		if statusF != "" && status != statusF {
			return false
		}
		if priorityF != "" && effectivePriority(priority) != priorityF {
			return false
		}
		if milestoneF != "" && milestone != milestoneF {
			return false
		}
		return true
	}
	var filteredRows []liveTaskOut
	var filteredTasks []taskOut
	if rows != nil {
		for _, t := range rows {
			if !keep(t.Status, t.Priority, t.Milestone) {
				continue
			}
			counts[t.Status]++
			filteredRows = append(filteredRows, toLiveTaskOut(t))
		}
	} else {
		for i := range tasks {
			t := &tasks[i]
			if !keep(t.Status, t.Priority, t.Milestone) {
				continue
			}
			counts[t.Status]++
			filteredTasks = append(filteredTasks, toTaskOut(t))
		}
	}

	if jsonOut {
		if rows != nil {
			out := struct {
				Tag    string         `json:"tag"`
				Total  int            `json:"total"`
				Counts map[string]int `json:"counts"`
				Tasks  []liveTaskOut  `json:"tasks"`
			}{tag, len(filteredRows), counts, filteredRows}
			if out.Tasks == nil {
				out.Tasks = []liveTaskOut{}
			}
			if err := writeJSON(stdout, out); err != nil {
				return fail(stderr, err)
			}
			return 0
		}
		out := listOut{Tag: tag, Total: len(filteredTasks), Counts: counts, Tasks: filteredTasks}
		if out.Tasks == nil {
			out.Tasks = []taskOut{}
		}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tPRIORITY\tDEPS\tTITLE")
	print := func(id, status, priority string, deps []string, title string) {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			id, status, effectivePriority(priority), depsString(deps), title)
	}
	for _, t := range filteredRows {
		print(t.ID, t.Status, t.Priority, t.Dependencies, t.Title)
	}
	for _, t := range filteredTasks {
		print(t.ID, t.Status, t.Priority, t.Dependencies, t.Title)
	}
	tw.Flush()
	fmt.Fprintln(stdout, summaryLine(counts, len(filteredRows)+len(filteredTasks)))
	return 0
}

func cmdNext(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("next", stderr, &cf)
	milestoneF := fs.String("milestone", "", "restrict candidates to this milestone")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "noise-tasks next: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *milestoneF != "" {
		if err := backlog.ValidateMilestoneName(*milestoneF); err != nil {
			return reportMilestoneError(stderr, cf.json, err)
		}
	}

	if cf.file != "" {
		f, tag, err := loadFixture(&cf)
		if err != nil {
			return fail(stderr, err)
		}
		tasks, err := f.Tasks(tag)
		if err != nil {
			return fail(stderr, err)
		}
		entries, err := f.Milestones(tag)
		if err != nil {
			return reportMilestoneError(stderr, cf.json, err)
		}
		if code := requireKnownMilestone(entries, *milestoneF, stderr); code != 0 {
			return code
		}
		next := selectNextCandidates(tasks, entries, *milestoneF, nil)
		if cf.json {
			var v any
			if next != nil {
				v = toTaskOut(next)
			}
			if err := writeJSON(stdout, v); err != nil {
				return fail(stderr, err)
			}
			return 0
		}
		if next == nil {
			fmt.Fprintln(stdout, "no actionable task")
			return 0
		}
		printTask(stdout, next)
		return 0
	}

	ps, tag, err := resolveStoreTag(&cf)
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()
	rows, err := ps.db.ListTasks(tag)
	if err != nil {
		return fail(stderr, err)
	}
	entries, err := ps.db.MilestoneEntries(tag)
	if err != nil {
		return reportMilestoneError(stderr, cf.json, err)
	}
	if code := requireKnownMilestone(entries, *milestoneF, stderr); code != 0 {
		return code
	}
	nonterminal, err := ps.db.NonterminalRunTaskIDs(tag)
	if err != nil {
		return fail(stderr, err)
	}
	tasks := make([]backlog.Task, 0, len(rows))
	byID := make(map[string]*store.Task, len(rows))
	claimed := make(map[string]bool, len(rows))
	for _, row := range rows {
		// Every task stays in the dependency universe: a dependency
		// resolves against the tag's rows regardless of claims, so a
		// claimed done/cancelled dependency still satisfies its dependents.
		// Claim and nonterminal-run exclusions apply to candidates only
		// (SPEC §4): next never advertises work that cannot be acquired
		// (§8.3). ListTasks has already expired stale claims in memory.
		task, err := storeToBacklogTask(row)
		if err != nil {
			return fail(stderr, err)
		}
		tasks = append(tasks, *task)
		byID[row.ID] = row
		claimed[row.ID] = row.Claim != nil
	}
	next := selectNextCandidates(tasks, entries, *milestoneF,
		func(t *backlog.Task) bool { return claimed[t.ID] || nonterminal[t.ID] })
	if cf.json {
		var v any
		if next != nil {
			v = toLiveTaskOut(byID[next.ID])
		}
		if err := writeJSON(stdout, v); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	if next == nil {
		fmt.Fprintln(stdout, "no actionable task")
		return 0
	}
	printTask(stdout, next)
	return 0
}

// selectNext implements the SPEC §4 rule: the highest-priority task whose
// status is pending and whose dependencies are all done/cancelled; ties
// break by lower id. Per the same rule, a dependency that does not
// resolve to a task in the tag counts as unsatisfied and gates its task.
func selectNext(tasks []backlog.Task) *backlog.Task {
	return selectNextExcluded(tasks, func(*backlog.Task) bool { return false })
}

// selectNextCandidates keeps every task in the dependency-status universe
// while applying milestone and caller exclusions only to candidacy. A task in
// a closed or different milestone can therefore still satisfy a dependency
// when its status is done/cancelled (SPEC §4/§12.2).
func selectNextCandidates(tasks []backlog.Task, entries []backlog.MilestoneEntry, filter string, excluded func(*backlog.Task) bool) *backlog.Task {
	eligible := make(map[string]bool)
	for _, task := range nextCandidates(tasks, entries, filter) {
		eligible[task.ID] = true
	}
	return selectNextExcluded(tasks, func(task *backlog.Task) bool {
		if !eligible[task.ID] {
			return true
		}
		return excluded != nil && excluded(task)
	})
}

// selectNextExcluded is selectNext with a candidacy-exclusion predicate.
// Excluded tasks still resolve as dependencies — their status counts in
// the universe — but are never selected; next uses this so a claimed
// done/cancelled dependency satisfies its dependents while no claimed
// task is ever advertised (SPEC §4/§8.3).
func selectNextExcluded(tasks []backlog.Task, excluded func(*backlog.Task) bool) *backlog.Task {
	status := make(map[string]string, len(tasks))
	for _, t := range tasks {
		status[t.ID] = t.Status
	}

	var best *backlog.Task
	for i := range tasks {
		t := &tasks[i]
		if t.Status != "pending" || excluded(t) {
			continue
		}
		ready := true
		for _, dep := range t.Dependencies {
			st, ok := status[dep]
			if !ok || (st != "done" && st != "cancelled") {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		if best == nil ||
			priorityRank(t.Priority) < priorityRank(best.Priority) ||
			(priorityRank(t.Priority) == priorityRank(best.Priority) && idLess(t.ID, best.ID)) {
			best = t
		}
	}
	return best
}

// requireKnownMilestone refuses an unknown --milestone filter value as a
// usage error (§12.2); an empty name always passes.
func requireKnownMilestone(entries []backlog.MilestoneEntry, name string, stderr io.Writer) int {
	if name == "" {
		return 0
	}
	for _, e := range entries {
		if e.Name == name {
			return 0
		}
	}
	fmt.Fprintf(stderr, "noise-tasks: milestone %q does not exist\n", name)
	return 2
}

// nextCandidates applies §12.2's next rule: only tasks whose milestone is
// absent or names an open entry are candidates, and a --milestone filter
// restricts them to exactly that milestone.
func nextCandidates(tasks []backlog.Task, entries []backlog.MilestoneEntry, filter string) []backlog.Task {
	open := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Status == "open" {
			open[e.Name] = true
		}
	}
	out := tasks[:0:0]
	for _, t := range tasks {
		if t.Milestone != "" && !open[t.Milestone] {
			continue
		}
		if filter != "" && t.Milestone != filter {
			continue
		}
		out = append(out, t)
	}
	return out
}

// priorityRank orders high > medium > low. An absent priority is medium
// per SPEC §1.
func priorityRank(p string) int {
	switch p {
	case "high":
		return 0
	case "low":
		return 2
	default:
		return 1
	}
}

// idLess compares ids numerically. Ids are validated positive-integer
// strings without leading zeros, so shorter means smaller and equal
// lengths compare lexicographically.
func idLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func cmdShow(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("show", stderr, &cf)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks show [options] <id>")
		return 2
	}
	id := fs.Arg(0)

	if cf.file != "" {
		f, tag, err := loadFixture(&cf)
		if err != nil {
			return fail(stderr, err)
		}
		task, subtask, err := f.ResolveTaskReference(tag, id)
		if err != nil {
			return fail(stderr, err)
		}
		if cf.json {
			var out any
			if subtask != nil {
				out = toSubtaskOut(subtask)
			} else {
				out = toTaskOut(task)
			}
			if err := writeJSON(stdout, out); err != nil {
				return fail(stderr, err)
			}
			return 0
		}
		if subtask != nil {
			printSubtask(stdout, subtask)
			return 0
		}
		printTask(stdout, task)
		return 0
	}

	ps, tag, err := resolveStoreTag(&cf)
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()
	record, subtask, err := resolveStoreReference(ps.db, tag, id)
	if err != nil {
		return fail(stderr, err)
	}
	if cf.json {
		var out any
		if subtask != nil {
			out = toLiveSubtaskOut(subtask, record.Revision)
		} else {
			out = toLiveTaskOut(record)
		}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	if subtask != nil {
		printSubtask(stdout, subtask)
		return 0
	}
	task, err := storeToBacklogTask(record)
	if err != nil {
		return fail(stderr, err)
	}
	printTask(stdout, task)
	return 0
}
