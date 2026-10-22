package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/noisefloor-oss/tasks/internal/backlog"
)

// Milestone registry and task-field operations (SPEC §12.3). Registry
// mutations are §8.2 immediate-transaction mutations: they touch only the
// tag row's milestones value and append one content-free `milestone` event;
// set-milestone is a §1 field mutation in full — CAS revision, revision
// increment, `update` event.

// MilestoneEntries parses the tag's registry (strict; a malformed registry
// refuses with backlog.ErrMilestoneRegistry).
func (d *DB) MilestoneEntries(tag string) ([]backlog.MilestoneEntry, error) {
	raw, err := d.Milestones(tag)
	if err != nil || len(raw) == 0 {
		return nil, err
	}
	return backlog.ParseMilestones(tag, raw)
}

// MilestoneCountsForList returns, per milestone name, the assigned-task
// total and done count for the tag (the `milestone list` rollup).
func (d *DB) MilestoneCountsForList(tag string) (map[string]int, map[string]int, error) {
	total := make(map[string]int)
	done := make(map[string]int)
	tasks, err := d.ListTasks(tag)
	if err != nil {
		return nil, nil, err
	}
	for _, task := range tasks {
		if task.Milestone == "" {
			continue
		}
		total[task.Milestone]++
		if task.Status == "done" {
			done[task.Milestone]++
		}
	}
	return total, done, nil
}

// milestoneRemaining counts tag tasks assigned to name whose status is
// neither done nor cancelled.
func milestoneRemaining(tasks []*Task, name string) int {
	remaining := 0
	for _, task := range tasks {
		if task.Milestone != name {
			continue
		}
		if task.Status != "done" && task.Status != "cancelled" {
			remaining++
		}
	}
	return remaining
}

func (t *tx) milestonesRaw(tag string) (json.RawMessage, error) {
	info, err := t.tagInfo(tag)
	if err != nil || info == nil {
		return nil, err
	}
	return info.Milestones, nil
}

func (t *tx) writeMilestones(tag string, raw json.RawMessage) error {
	var val any
	if len(raw) > 0 {
		val = string(raw)
	}
	if _, err := t.Exec(`UPDATE tags SET milestones=? WHERE name=?`, val, tag); err != nil {
		return fmt.Errorf("write tag %q milestone registry: %w", tag, err)
	}
	return nil
}

// checkMilestoneKnown refuses a task milestone assignment naming no registry
// entry. Called inside the writing transaction so a concurrent milestone
// remove cannot strand the reference (SPEC §8.2).
func (t *tx) checkMilestoneKnown(tag, name string) error {
	raw, err := t.milestonesRaw(tag)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return &backlog.ErrMilestoneNotFound{Tag: tag, Name: name}
	}
	entries, err := backlog.ParseMilestones(tag, raw)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name == name {
			return nil
		}
	}
	return &backlog.ErrMilestoneNotFound{Tag: tag, Name: name}
}

func (t *tx) listTasks(tag string) ([]*Task, error) {
	cols, err := t.taskSelectColumns()
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(`SELECT `+cols+` FROM tasks WHERE tag=? ORDER BY CAST(id AS INTEGER)`, tag)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []*Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// AddMilestone appends one open entry to the tag's registry and records one
// content-free milestone event.
func (d *DB) AddMilestone(tag, name string) error {
	if err := backlog.ValidateMilestoneName(name); err != nil {
		return err
	}
	return d.immediate(func(t *tx) error {
		if _, err := t.ensureTag(tag); err != nil {
			return err
		}
		raw, err := t.milestonesRaw(tag)
		if err != nil {
			return err
		}
		var entries []backlog.MilestoneEntry
		if len(raw) > 0 {
			entries, err = backlog.ParseMilestones(tag, raw)
			if err != nil {
				return err
			}
		}
		for _, e := range entries {
			if e.Name == name {
				return &backlog.ErrMilestoneExists{Tag: tag, Name: name}
			}
		}
		entry, err := backlog.MarshalMilestoneEntry(name, "open")
		if err != nil {
			return err
		}
		if len(raw) > 0 {
			raw, err = backlog.AppendArrayElement(raw, entry)
			if err != nil {
				return err
			}
		} else {
			raw, err = backlog.MarshalMilestones([]backlog.MilestoneEntry{{Name: name, Status: "open", Raw: entry}})
			if err != nil {
				return err
			}
		}
		if err := t.writeMilestones(tag, raw); err != nil {
			return err
		}
		return t.appendEvent(Event{Tag: tag, Kind: "milestone", Count: 1, At: nowUTC()})
	})
}

// SetMilestoneStatus flips one entry's status in place (splicing only that
// value; every other byte of the registry stays identical) and reports the
// count of assigned tasks not done/cancelled after the flip. Flipping to the
// current status is a successful no-op that appends no event.
func (d *DB) SetMilestoneStatus(tag, name, status string) (int, error) {
	if !backlog.ValidMilestoneStatuses[status] {
		return 0, fmt.Errorf("invalid milestone status %q", status)
	}
	var remaining int
	err := d.immediate(func(t *tx) error {
		info, err := t.tagInfo(tag)
		if err != nil {
			return err
		}
		if info == nil || len(info.Milestones) == 0 {
			return &backlog.ErrMilestoneNotFound{Tag: tag, Name: name}
		}
		entries, err := backlog.ParseMilestones(tag, info.Milestones)
		if err != nil {
			return err
		}
		idx := -1
		for i, e := range entries {
			if e.Name == name {
				idx = i
				break
			}
		}
		if idx < 0 {
			return &backlog.ErrMilestoneNotFound{Tag: tag, Name: name}
		}
		if entries[idx].Status != status {
			spliced, err := backlog.SpliceObjectValues(entries[idx].Raw, map[string]json.RawMessage{
				"status": json.RawMessage(strconv.Quote(status)),
			})
			if err != nil {
				return err
			}
			raw, err := backlog.SpliceArrayElement(info.Milestones, idx, spliced)
			if err != nil {
				return err
			}
			if err := t.writeMilestones(tag, raw); err != nil {
				return err
			}
			if err := t.appendEvent(Event{Tag: tag, Kind: "milestone", Count: 1, At: nowUTC()}); err != nil {
				return err
			}
		}
		tasks, err := t.listTasks(tag)
		if err != nil {
			return err
		}
		remaining = milestoneRemaining(tasks, name)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return remaining, nil
}

// RemoveMilestone drops one entry; with force it also clears the field from
// referencing tasks, bumping each revision and appending one update event
// per cleared task, then one milestone event.
func (d *DB) RemoveMilestone(tag, name string, force bool) ([]string, error) {
	var cleared []string
	err := d.immediate(func(t *tx) error {
		info, err := t.tagInfo(tag)
		if err != nil {
			return err
		}
		if info == nil || len(info.Milestones) == 0 {
			return &backlog.ErrMilestoneNotFound{Tag: tag, Name: name}
		}
		entries, err := backlog.ParseMilestones(tag, info.Milestones)
		if err != nil {
			return err
		}
		idx := -1
		for i, e := range entries {
			if e.Name == name {
				idx = i
				break
			}
		}
		if idx < 0 {
			return &backlog.ErrMilestoneNotFound{Tag: tag, Name: name}
		}

		tasks, err := t.listTasks(tag)
		if err != nil {
			return err
		}
		var refs []*Task
		for _, task := range tasks {
			if task.Milestone == name {
				refs = append(refs, task)
			}
		}
		if len(refs) > 0 && !force {
			ids := make([]string, len(refs))
			for i, task := range refs {
				ids[i] = task.ID
			}
			sort.Strings(ids)
			return &backlog.ErrMilestoneInUse{Tag: tag, Name: name, TaskIDs: ids}
		}
		at := nowUTC()
		// Clearing the milestone field is a §8.3 field mutation of every
		// referencing task: an unexpired claim refuses the whole removal —
		// there is no operator override for field mutation. Expired leases
		// are cleared by the writes below.
		for _, task := range refs {
			expireClaim(task, time.Now())
			if task.Claim != nil {
				return &ClaimConflict{Tag: tag, ID: task.ID, Kind: "update",
					Owner: task.Claim.Owner, Expiry: task.Claim.ExpiresAt, FencingToken: task.Claim.Token}
			}
		}
		for _, task := range refs {
			task.Milestone = ""
			task.UpdatedAt = at
			old := task.Revision
			task.Revision++
			if _, err := t.writeTask(task, old); err != nil {
				return err
			}
			cleared = append(cleared, task.ID)
			if err := t.appendEvent(Event{Task: task.ID, Tag: tag, Kind: "update", Revision: task.Revision, At: at}); err != nil {
				return err
			}
		}

		var raw json.RawMessage
		if len(entries) > 1 {
			// Dropping the last entry clears the registry entirely.
			raw, err = backlog.RemoveArrayElement(info.Milestones, idx)
			if err != nil {
				return err
			}
		}
		if err := t.writeMilestones(tag, raw); err != nil {
			return err
		}
		return t.appendEvent(Event{Tag: tag, Kind: "milestone", Count: 1, At: nowUTC()})
	})
	if err != nil {
		return nil, err
	}
	return cleared, nil
}

// SetTaskMilestone assigns (name != "") or clears (name == "") a task's
// milestone field under CAS and the §8.3 claim gate (a field mutation on an
// unexpired claim requires its owner and token). A no-op assignment
// validates the revision but changes nothing and appends no event. Returns
// the previous value.
func (d *DB) SetTaskMilestone(tag, id, name string, revision int, gate *ClaimGate) (string, error) {
	if err := ValidateTaskRunGate(gate); err != nil {
		return "", err
	}
	var previous string
	err := d.immediate(func(t *tx) error {
		task, err := t.getTask(tag, id)
		if err != nil {
			return err
		}
		if task == nil {
			return &TaskNotFound{Tag: tag, ID: id}
		}
		now := time.Now().UTC()
		expireClaim(task, now)
		if task.Revision != revision {
			return t.staleConflict(tag, id, revision)
		}
		previous = task.Milestone
		// Input and milestone validation precedes fencing (SPEC §8.3): an
		// independently invalid request reports its own error even when the
		// task is claimed and ungated.
		if name != "" {
			if err := backlog.ValidateMilestoneName(name); err != nil {
				return err
			}
			if err := t.checkMilestoneKnown(tag, name); err != nil {
				return err
			}
		}
		claim := cloneClaim(task.Claim)
		fence, err := t.taskMutationRunFence(task, gate, now, "update")
		if err != nil {
			return err
		}
		if task.Milestone == name {
			return nil
		}
		task.Milestone = name
		task.UpdatedAt = now.Format(time.RFC3339)
		old := task.Revision
		task.Revision++
		if err := taskMutationFaultHook(t, "before-write"); err != nil {
			return err
		}
		n, err := t.writeTaskWithRunFence(task, old, gate, claim, fence, task.UpdatedAt)
		if err != nil {
			return err
		}
		if n == 0 {
			return t.taskMutationWriteConflict(fence, claim, gate, "update", tag, id, revision)
		}
		return t.appendEvent(Event{Task: id, Tag: tag, Kind: "update", Revision: task.Revision, At: task.UpdatedAt})
	})
	if err != nil {
		return "", err
	}
	return previous, nil
}
