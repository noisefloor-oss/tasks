package store

import (
	"fmt"
	"time"

	"github.com/noisefloor-oss/tasks/internal/backlog"
)

// selectClaimNext applies the §8.3 claim-next candidate rule to one tag's
// tasks inside the caller's transaction: `next`'s rule (pending status, all
// dependencies resolved in the tag and done/cancelled, milestone absent or
// open, optional exact --milestone filter) plus the claim-specific
// exclusions — no unexpired claim, no nonterminal run, and with an assignment
// filter set, a recorded assignment to that owner (both exclusion maps come
// from the same transaction). Ordering is deterministic: priority high >
// medium (absent) > low, ties by lower id. now is the single clock reading
// every expiry judgment in the transaction shares.
func selectClaimNext(tasks []*Task, entries []backlog.MilestoneEntry, milestoneFilter, assignmentOwner string, assigned, nonterminal map[string]bool, now time.Time) *Task {
	open := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Status == "open" {
			open[e.Name] = true
		}
	}
	status := make(map[string]string, len(tasks))
	for _, t := range tasks {
		status[t.ID] = t.Status
	}
	var best *Task
	for _, t := range tasks {
		if t.Status != "pending" {
			continue
		}
		// Expired leases were cleared in memory by expireClaim; a claim
		// still present is unexpired and excludes its task.
		if t.Claim != nil || nonterminal[t.ID] {
			continue
		}
		if t.Milestone != "" && !open[t.Milestone] {
			continue
		}
		if milestoneFilter != "" && t.Milestone != milestoneFilter {
			continue
		}
		if assignmentOwner != "" && !assigned[t.ID] {
			// An explicit assignment filter never falls back to unassigned
			// work (SPEC §8.3).
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
			priorityRankVal(t.Priority) < priorityRankVal(best.Priority) ||
			(priorityRankVal(t.Priority) == priorityRankVal(best.Priority) && idLessVal(t.ID, best.ID)) {
			best = t
		}
	}
	return best
}

// assignedTaskIDs resolves §9.2 plan assignments inside claim-next's write
// transaction. The covering assignments_owner_task index serves this exact
// tag/owner lookup; an empty set makes an explicit --assignment filter return
// no work rather than falling back to unassigned tasks.
func assignedTaskIDs(t *tx, tag, owner string) (map[string]bool, error) {
	rows, err := t.Query(`SELECT task_id FROM assignments WHERE tag=? AND owner=? ORDER BY task_id`, tag, owner)
	if err != nil {
		return nil, fmt.Errorf("list assignments for owner %q in tag %q: %w", owner, tag, err)
	}
	defer rows.Close()
	assigned := make(map[string]bool)
	for rows.Next() {
		var taskID string
		if err := rows.Scan(&taskID); err != nil {
			return nil, err
		}
		assigned[taskID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return assigned, nil
}

// priorityRankVal is the store-side §4 ranking: high > medium (absent) > low.
func priorityRankVal(p string) int {
	switch p {
	case "high":
		return 0
	case "low":
		return 2
	default:
		return 1
	}
}

// idLessVal compares validated positive-integer ids numerically.
func idLessVal(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// NonterminalRunTaskIDs snapshots the live-run candidate exclusion used by
// the advisory live `next` command. Released schemas before v4 have no run
// state and therefore return an empty set without being upgraded by this
// read. A v4 or v5 catalog is queried directly, so a malformed current schema fails
// closed instead of being mistaken for an older store.
func (d *DB) NonterminalRunTaskIDs(tag string) (map[string]bool, error) {
	ids := make(map[string]bool)
	err := d.deferred(func(t *tx) error {
		var version int
		if err := t.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
			return fmt.Errorf("read schema version for nonterminal runs: %w", err)
		}
		switch version {
		case 1, identitySchemaVersion, orchestratorSchemaVersion:
			return nil
		case runSchemaVersion, SchemaVersion:
			var err error
			ids, err = t.nonterminalRunTaskIDs(tag)
			return err
		default:
			return fmt.Errorf("task store schema %d is not readable for nonterminal runs", version)
		}
	})
	return ids, err
}

// ClaimNext selects and acquires one task in a single immediate transaction
// (SPEC §8.3). Every dependency and filter is (re-)evaluated after the write
// lock is held, so no task is returned that this transaction did not claim;
// a nil task with a nil error is the successful empty result, which records
// no receipt — a request id is never reserved without a committed effect.
// The task revision is neither required nor incremented; the acquisition
// appends one claim event and its immutable §8.4 receipt. Acquisition
// requires a request id: replay with the same owner and normalized inputs
// returns the originally recorded task and token without repeating effects
// (replayed reports that replay).
func (d *DB) ClaimNext(tag, owner string, ttl int, milestoneFilter, assignmentOwner, requestID string) (*Task, *Claim, bool, error) {
	if err := ValidateOwnerLabel(owner); err != nil {
		return nil, nil, false, fmt.Errorf("claim owner %v", err)
	}
	if assignmentOwner != "" {
		if err := ValidateOwnerLabel(assignmentOwner); err != nil {
			return nil, nil, false, fmt.Errorf("assignment owner %v", err)
		}
	}
	if err := validateClaimRequestID(requestID); err != nil {
		return nil, nil, false, err
	}
	if ttl <= 0 {
		ttl = DefaultClaimTTL
	}
	r := claimRequest{operation: "claim-next", requestID: requestID, tag: tag,
		owner: owner, ttl: ttl, milestone: milestoneFilter, assignment: assignmentOwner}
	var claimed *Task
	var claim *Claim
	replayed := false
	err := d.immediate(func(t *tx) error {
		now := time.Now().UTC()
		inputDigest, err := claimInputDigest(r)
		if err != nil {
			return err
		}
		task, recordedRevision, found, err := lookupClaimReplay(t, r, inputDigest, now)
		if err != nil {
			return err
		}
		if found {
			// Replay reports the revision recorded with the acquisition,
			// even if the task was mutated later (SPEC §8.3).
			task.Revision = recordedRevision
			claimed, claim, replayed = task, task.Claim, true
			return nil
		}
		cols, err := t.taskSelectColumns()
		if err != nil {
			return err
		}
		rows, err := t.Query(`SELECT `+cols+` FROM tasks WHERE tag=? ORDER BY CAST(id AS INTEGER)`, tag)
		if err != nil {
			return fmt.Errorf("list tag %q tasks: %w", tag, err)
		}
		tasks := []*Task{}
		for rows.Next() {
			task, err := scanTask(rows)
			if err != nil {
				rows.Close()
				return err
			}
			expireClaim(task, now)
			tasks = append(tasks, task)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

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
		// The assignment filter resolves inside the same write transaction as
		// selection and acquisition; an empty set excludes every candidate.
		var assigned map[string]bool
		if assignmentOwner != "" {
			assigned, err = assignedTaskIDs(t, tag, assignmentOwner)
			if err != nil {
				return err
			}
		}
		nonterminal, err := t.nonterminalRunTaskIDs(tag)
		if err != nil {
			return err
		}
		task = selectClaimNext(tasks, entries, milestoneFilter, assignmentOwner, assigned, nonterminal, now)
		if task == nil {
			return nil
		}
		claim, err = writeClaimAcquisition(t, r, task, now)
		if err != nil {
			return err
		}
		claimed = task
		return nil
	})
	if err != nil {
		return nil, nil, false, err
	}
	return claimed, claim, replayed, nil
}
