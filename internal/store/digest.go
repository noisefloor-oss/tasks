package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
)

// §8.5 versioned digests. Version 1 algorithms and class orders never change;
// a binary that does not implement both digest versions fails closed.
const (
	semanticDigestDomain   = "noise-tasks-semantic-v1\x00"
	comparisonDigestDomain = "noise-tasks-legacy-compare-v1\x00"

	SemanticDigestVersion   = 1
	ComparisonDigestVersion = 1
)

// semanticClasses is the fixed §8.5 semantic digest class order. The
// comparison digest covers the shared subset marked below, in the same order.
var semanticClasses = []struct {
	name       string
	comparison bool
}{
	{"store", false},
	{"tags", true},
	{"milestones", true},
	{"tasks", true},
	{"dependencies", true},
	{"subtasks", true},
	{"bindings", true},
	{"assignments", false},
	{"claims", true},
	{"fence", true},
	{"events", true},
	{"receipts", false},
	{"plans", false},
	{"planMappings", false},
	{"runs", false},
	{"attempts", false},
	{"worktreeSagas", false},
}

// ClassInventory counts every variable-cardinality durable record class for
// the §8.5 manifest and receipt. The singleton family/owner and fence records
// have no redundant count. Plans, mappings, and assignments are physical v3
// records; runs and attempts are physical v4 records; worktree sagas are
// physical v5 records.
type ClassInventory struct {
	Tags          int `json:"tags"`
	Milestones    int `json:"milestones"`
	Tasks         int `json:"tasks"`
	Dependencies  int `json:"dependencies"`
	Subtasks      int `json:"subtasks"`
	Bindings      int `json:"bindings"`
	Assignments   int `json:"assignments"`
	Claims        int `json:"claims"`
	Events        int `json:"events"`
	Receipts      int `json:"receipts"`
	Plans         int `json:"plans"`
	PlanMappings  int `json:"planMappings"`
	Runs          int `json:"runs"`
	Attempts      int `json:"attempts"`
	WorktreeSagas int `json:"worktreeSagas"`
}

// countClassInventory counts every §8.5 record class inside the caller's
// transaction without rendering records. Counts mirror the digester's
// exactly — stored rows and registry entries, regardless of lease expiry —
// so a receipt inventory of a state agrees with its manifest inventory.
// Classes this schema does not carry stay zero.
func countClassInventory(t *tx) (*ClassInventory, error) {
	inv := &ClassInventory{}
	count := func(dst *int, query string) error {
		var n int64
		if err := t.QueryRow(query).Scan(&n); err != nil {
			return err
		}
		*dst = int(n)
		return nil
	}
	if err := count(&inv.Tags, `SELECT COUNT(*) FROM tags`); err != nil {
		return nil, fmt.Errorf("count tags: %w", err)
	}
	if err := count(&inv.Tasks, `SELECT COUNT(*) FROM tasks`); err != nil {
		return nil, fmt.Errorf("count tasks: %w", err)
	}
	if err := count(&inv.Bindings, `SELECT COUNT(*) FROM tasks WHERE wt_path IS NOT NULL`); err != nil {
		return nil, fmt.Errorf("count bindings: %w", err)
	}
	if err := count(&inv.Claims, `SELECT COUNT(*) FROM tasks WHERE claim_owner IS NOT NULL`); err != nil {
		return nil, fmt.Errorf("count claims: %w", err)
	}
	if err := count(&inv.Events, `SELECT COUNT(*) FROM events`); err != nil {
		return nil, fmt.Errorf("count events: %w", err)
	}
	if err := count(&inv.Receipts, `SELECT COUNT(*) FROM receipts`); err != nil {
		return nil, fmt.Errorf("count receipts: %w", err)
	}
	// Versioned classes carry physical tables exactly when the digester renders
	// them (user_version guards both), so a receipt inventory of a state agrees
	// with its manifest inventory on every class.
	if ok, err := t.hasSchemaVersion(orchestratorSchemaVersion); err != nil {
		return nil, fmt.Errorf("read schema version for inventory: %w", err)
	} else if ok {
		if err := count(&inv.Assignments, `SELECT COUNT(*) FROM assignments`); err != nil {
			return nil, fmt.Errorf("count assignments: %w", err)
		}
		if err := count(&inv.Plans, `SELECT COUNT(*) FROM plans`); err != nil {
			return nil, fmt.Errorf("count plans: %w", err)
		}
		if err := count(&inv.PlanMappings, `SELECT COUNT(*) FROM plan_mappings`); err != nil {
			return nil, fmt.Errorf("count plan mappings: %w", err)
		}
	}
	if ok, err := t.hasSchemaVersion(runSchemaVersion); err != nil {
		return nil, fmt.Errorf("read schema version for run inventory: %w", err)
	} else if ok {
		if err := count(&inv.Runs, `SELECT COUNT(*) FROM runs`); err != nil {
			return nil, fmt.Errorf("count runs: %w", err)
		}
		if err := count(&inv.Attempts, `SELECT COUNT(*) FROM attempts`); err != nil {
			return nil, fmt.Errorf("count attempts: %w", err)
		}
	}
	if ok, err := t.hasSchemaVersion(worktreeSagaSchemaVersion); err != nil {
		return nil, fmt.Errorf("read schema version for worktree saga inventory: %w", err)
	} else if ok {
		if err := count(&inv.WorktreeSagas, `SELECT COUNT(*) FROM worktree_sagas`); err != nil {
			return nil, fmt.Errorf("count worktree sagas: %w", err)
		}
	}
	rows, err := t.Query(`SELECT milestones FROM tags WHERE milestones IS NOT NULL AND milestones != ''`)
	if err != nil {
		return nil, fmt.Errorf("read milestone registries: %w", err)
	}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, err
		}
		entries, err := decodeJSONArray([]byte(raw))
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("decode milestone registry: %w", err)
		}
		inv.Milestones += len(entries)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	rows, err = t.Query(`SELECT dependencies, subtasks FROM tasks`)
	if err != nil {
		return nil, fmt.Errorf("read task registries: %w", err)
	}
	for rows.Next() {
		var deps, subs string
		if err := rows.Scan(&deps, &subs); err != nil {
			rows.Close()
			return nil, err
		}
		if deps == "" {
			deps = "[]"
		}
		depsList, err := decodeJSONArray([]byte(deps))
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("decode dependencies: %w", err)
		}
		inv.Dependencies += len(depsList)
		if subs == "" {
			continue
		}
		list, err := decodeJSONArray([]byte(subs))
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("decode subtasks: %w", err)
		}
		inv.Subtasks += len(list)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return inv, nil
}

// classDigests is one class's framed byte stream for hashing: the ASCII name,
// one NUL, then per record an unsigned 64-bit big-endian byte length and the
// canonical JSON, terminated by eight zero bytes (SPEC §8.5).
type classDigester struct {
	semantic   hash.Hash
	comparison hash.Hash
}

func newClassDigester() *classDigester {
	s := sha256.New()
	s.Write([]byte(semanticDigestDomain))
	c := sha256.New()
	c.Write([]byte(comparisonDigestDomain))
	return &classDigester{semantic: s, comparison: c}
}

func (cd *classDigester) emit(className string, comparison bool, records [][]byte) {
	write := func(h hash.Hash) {
		h.Write([]byte(className))
		h.Write([]byte{0})
		var frame [8]byte
		for _, rec := range records {
			binary.BigEndian.PutUint64(frame[:], uint64(len(rec)))
			h.Write(frame[:])
			h.Write(rec)
		}
		h.Write(make([]byte, 8))
	}
	write(cd.semantic)
	if comparison {
		write(cd.comparison)
	}
}

func (cd *classDigester) sums() (semantic, comparison string) {
	return "sha256:" + hex.EncodeToString(cd.semantic.Sum(nil)),
		"sha256:" + hex.EncodeToString(cd.comparison.Sum(nil))
}

// ClassDigests computes the §8.5 versioned semantic and comparison digests
// and the inventory over one consistent deferred snapshot of the database.
// The database must carry its §8.2 identity. Physical layout, user_version,
// WAL state, and journal files are excluded by construction: only logical
// records feed the digest.
func (d *DB) ClassDigests() (semantic, comparison string, inv *ClassInventory, err error) {
	cd := newClassDigester()
	inv = &ClassInventory{}
	err = d.deferred(func(t *tx) error {
		return cd.compute(t, inv)
	})
	if err != nil {
		return "", "", nil, err
	}
	semantic, comparison = cd.sums()
	return semantic, comparison, inv, nil
}

func (cd *classDigester) compute(t *tx, inv *ClassInventory) error {
	for _, class := range semanticClasses {
		records, count, err := cd.classRecords(t, class.name)
		if err != nil {
			return err
		}
		cd.emit(class.name, class.comparison, records)
		if err := inv.add(class.name, count); err != nil {
			return err
		}
	}
	return nil
}

func (inv *ClassInventory) add(class string, count int) error {
	switch class {
	case "store", "fence":
		// Singleton records carry no redundant count (SPEC §8.5).
	case "tags":
		inv.Tags = count
	case "milestones":
		inv.Milestones = count
	case "tasks":
		inv.Tasks = count
	case "dependencies":
		inv.Dependencies = count
	case "subtasks":
		inv.Subtasks = count
	case "bindings":
		inv.Bindings = count
	case "assignments":
		inv.Assignments = count
	case "claims":
		inv.Claims = count
	case "events":
		inv.Events = count
	case "receipts":
		inv.Receipts = count
	case "plans":
		inv.Plans = count
	case "planMappings":
		inv.PlanMappings = count
	case "runs":
		inv.Runs = count
	case "attempts":
		inv.Attempts = count
	case "worktreeSagas":
		inv.WorktreeSagas = count
	default:
		return fmt.Errorf("unknown digest class %q", class)
	}
	return nil
}

// classRecords renders one class's records as canonical JSON in the §8.5
// fixed within-class order. Classes without a physical table in this schema
// are empty (the class header and terminator still frame the digest).
func (cd *classDigester) classRecords(t *tx, class string) ([][]byte, int, error) {
	switch class {
	case "store":
		familyID, ownerBoundary, err := t.requireIdentity()
		if err != nil {
			return nil, 0, err
		}
		rec, err := canonicalJSON(map[string]any{"familyId": familyID, "ownerBoundary": ownerBoundary})
		if err != nil {
			return nil, 0, err
		}
		return [][]byte{rec}, 1, nil
	case "tags", "milestones":
		return tagClassRecords(t, class)
	case "tasks", "dependencies", "subtasks", "bindings", "claims":
		return taskClassRecords(t, class)
	case "assignments":
		if ok, err := t.hasSchemaVersion(orchestratorSchemaVersion); err != nil || !ok {
			return nil, 0, err
		}
		return assignmentClassRecords(t)
	case "fence":
		var fencing int64
		if err := t.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key='fencing'`).Scan(&fencing); err != nil {
			return nil, 0, fmt.Errorf("read fencing counter: %w", err)
		}
		rec, err := canonicalJSON(map[string]any{"counter": fencing})
		if err != nil {
			return nil, 0, err
		}
		return [][]byte{rec}, 1, nil
	case "events":
		return eventClassRecords(t)
	case "receipts":
		return receiptClassRecords(t)
	case "plans":
		if ok, err := t.hasSchemaVersion(orchestratorSchemaVersion); err != nil || !ok {
			return nil, 0, err
		}
		return planClassRecords(t)
	case "planMappings":
		if ok, err := t.hasSchemaVersion(orchestratorSchemaVersion); err != nil || !ok {
			return nil, 0, err
		}
		return planMappingClassRecords(t)
	case "runs":
		if ok, err := t.hasSchemaVersion(runSchemaVersion); err != nil || !ok {
			return nil, 0, err
		}
		return runClassRecords(t)
	case "attempts":
		if ok, err := t.hasSchemaVersion(runSchemaVersion); err != nil || !ok {
			return nil, 0, err
		}
		return attemptClassRecords(t)
	case "worktreeSagas":
		if ok, err := t.hasSchemaVersion(worktreeSagaSchemaVersion); err != nil || !ok {
			return nil, 0, err
		}
		return worktreeSagaClassRecords(t)
	default:
		return nil, 0, fmt.Errorf("unknown digest class %q", class)
	}
}

func (t *tx) hasSchemaVersion(minimum int) (bool, error) {
	var version int
	if err := t.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return false, fmt.Errorf("read schema version for semantic digest: %w", err)
	}
	return version >= minimum, nil
}

func assignmentClassRecords(t *tx) ([][]byte, int, error) {
	rows, err := t.Query(`SELECT tag, task_id, assignment_key, owner, plan_id
		FROM assignments ORDER BY tag, CAST(task_id AS INTEGER), assignment_key`)
	if err != nil {
		return nil, 0, fmt.Errorf("read assignments: %w", err)
	}
	defer rows.Close()
	var records [][]byte
	for rows.Next() {
		var tag, taskID, key, owner, planID string
		if err := rows.Scan(&tag, &taskID, &key, &owner, &planID); err != nil {
			return nil, 0, err
		}
		rec, err := canonicalJSON(map[string]any{
			"tag": tag, "taskId": taskID, "key": key, "owner": owner, "planId": planID,
		})
		if err != nil {
			return nil, 0, err
		}
		records = append(records, rec)
	}
	return records, len(records), rows.Err()
}

func planClassRecords(t *tx) ([][]byte, int, error) {
	rows, err := t.Query(`SELECT plan_id, tag, digest, canonical_json, request_id, receipt_id, applied_at
		FROM plans ORDER BY plan_id`)
	if err != nil {
		return nil, 0, fmt.Errorf("read plans: %w", err)
	}
	defer rows.Close()
	var records [][]byte
	for rows.Next() {
		var planID, tag, digest, canonicalPlan, requestID, receiptID, appliedAt string
		if err := rows.Scan(&planID, &tag, &digest, &canonicalPlan, &requestID, &receiptID, &appliedAt); err != nil {
			return nil, 0, err
		}
		planValue, err := decodeJSON([]byte(canonicalPlan))
		if err != nil {
			return nil, 0, fmt.Errorf("plan %q canonical JSON: %w", planID, err)
		}
		rec, err := canonicalJSON(map[string]any{
			"planId": planID, "tag": tag, "digest": digest, "plan": planValue,
			"requestId": requestID, "receiptId": receiptID, "appliedAt": appliedAt,
		})
		if err != nil {
			return nil, 0, err
		}
		records = append(records, rec)
	}
	return records, len(records), rows.Err()
}

func planMappingClassRecords(t *tx) ([][]byte, int, error) {
	rows, err := t.Query(`SELECT plan_id, local_key, task_id FROM plan_mappings ORDER BY plan_id, local_key`)
	if err != nil {
		return nil, 0, fmt.Errorf("read plan mappings: %w", err)
	}
	defer rows.Close()
	var records [][]byte
	for rows.Next() {
		var planID, key, taskID string
		if err := rows.Scan(&planID, &key, &taskID); err != nil {
			return nil, 0, err
		}
		rec, err := canonicalJSON(map[string]any{"planId": planID, "localKey": key, "taskId": taskID})
		if err != nil {
			return nil, 0, err
		}
		records = append(records, rec)
	}
	return records, len(records), rows.Err()
}

// runClassRecords renders runs in numeric run-id order. Every named logical
// field is present; nullable physical columns remain explicit JSON nulls, and
// private fencing values participate in the trusted semantic digest.
func runClassRecords(t *tx) ([][]byte, int, error) {
	rows, err := t.Query(`SELECT run_id, tag, task_id, owner, claim_acquisition_id,
		claim_token, status, current_attempt, start_request_id,
		terminal_result_digest, force_cancel_fence, started_at, updated_at,
		terminal_at, extra FROM runs ORDER BY run_id`)
	if err != nil {
		return nil, 0, fmt.Errorf("read runs: %w", err)
	}
	defer rows.Close()
	var records [][]byte
	for rows.Next() {
		var runID, claimToken, currentAttempt int64
		var tag, taskID, owner, claimAcquisitionID, status, startRequestID string
		var startedAt, updatedAt string
		var terminalResultDigest, terminalAt, extra sql.NullString
		var forceCancelFence sql.NullInt64
		if err := rows.Scan(&runID, &tag, &taskID, &owner, &claimAcquisitionID,
			&claimToken, &status, &currentAttempt, &startRequestID,
			&terminalResultDigest, &forceCancelFence, &startedAt, &updatedAt,
			&terminalAt, &extra); err != nil {
			return nil, 0, err
		}
		record := map[string]any{
			"runId": runID, "tag": tag, "task": taskID, "owner": owner,
			"claimAcquisitionId": claimAcquisitionID, "claimToken": claimToken,
			"status": status, "currentAttempt": currentAttempt,
			"startRequestId":       startRequestID,
			"terminalResultDigest": nullableString(terminalResultDigest),
			"forceCancelFence":     nullableInt64(forceCancelFence),
			"startedAt":            startedAt,
			"updatedAt":            updatedAt,
			"terminalAt":           nullableString(terminalAt),
		}
		if err := mergeExtra(record, extra); err != nil {
			return nil, 0, fmt.Errorf("run %d: %w", runID, err)
		}
		rec, err := canonicalJSON(record)
		if err != nil {
			return nil, 0, err
		}
		records = append(records, rec)
	}
	return records, len(records), rows.Err()
}

// attemptClassRecords renders attempts in numeric (run id, attempt number)
// order with the same explicit-null and private-fence rules as runs.
func attemptClassRecords(t *tx) ([][]byte, int, error) {
	rows, err := t.Query(`SELECT run_id, attempt_no, acquisition_id, attempt_token,
		status, ttl_seconds, lease_generation, created_request_id, started_at,
		expires_at, updated_at, terminal_at, terminal_result_digest, extra
		FROM attempts ORDER BY run_id, attempt_no`)
	if err != nil {
		return nil, 0, fmt.Errorf("read attempts: %w", err)
	}
	defer rows.Close()
	var records [][]byte
	for rows.Next() {
		var runID, attempt, attemptToken, ttlSeconds, leaseGeneration int64
		var acquisitionID, status, createdRequestID, startedAt, expiresAt, updatedAt string
		var terminalAt, terminalResultDigest, extra sql.NullString
		if err := rows.Scan(&runID, &attempt, &acquisitionID, &attemptToken,
			&status, &ttlSeconds, &leaseGeneration, &createdRequestID, &startedAt,
			&expiresAt, &updatedAt, &terminalAt, &terminalResultDigest, &extra); err != nil {
			return nil, 0, err
		}
		record := map[string]any{
			"runId": runID, "attempt": attempt, "acquisitionId": acquisitionID,
			"attemptToken": attemptToken, "status": status, "ttlSeconds": ttlSeconds,
			"leaseGeneration": leaseGeneration, "createdRequestId": createdRequestID,
			"startedAt": startedAt, "expiresAt": expiresAt, "updatedAt": updatedAt,
			"terminalAt":           nullableString(terminalAt),
			"terminalResultDigest": nullableString(terminalResultDigest),
		}
		if err := mergeExtra(record, extra); err != nil {
			return nil, 0, fmt.Errorf("run %d attempt %d: %w", runID, attempt, err)
		}
		rec, err := canonicalJSON(record)
		if err != nil {
			return nil, 0, err
		}
		records = append(records, rec)
	}
	return records, len(records), rows.Err()
}

func nullableString(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

func nullableInt64(value sql.NullInt64) any {
	if !value.Valid {
		return nil
	}
	return value.Int64
}

// mergeExtra flattens a stored unknown-fields JSON object into record,
// preserving every extension field in the digest (SPEC §8.5: every logical
// field and unknown-field extension is present in its record).
func mergeExtra(record map[string]any, extra sql.NullString) error {
	if !extra.Valid || extra.String == "" {
		return nil
	}
	obj, err := decodeJSONObject([]byte(extra.String))
	if err != nil {
		return fmt.Errorf("decode unknown fields: %w", err)
	}
	for k, v := range obj {
		if _, clash := record[k]; clash {
			return fmt.Errorf("unknown field %q collides with a logical field", k)
		}
		record[k] = v
	}
	return nil
}

// tagClassRecords renders the tags and milestones classes. Tag order is tag
// name bytes (the BINARY collation); milestone entries follow their registry
// index within each tag.
func tagClassRecords(t *tx, class string) ([][]byte, int, error) {
	rows, err := t.Query(`SELECT name, next_id, task_count, completed_count, milestones, extra FROM tags ORDER BY name`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var records [][]byte
	for rows.Next() {
		var name string
		var nextID, taskCount, completedCount int64
		var milestones, extra sql.NullString
		if err := rows.Scan(&name, &nextID, &taskCount, &completedCount, &milestones, &extra); err != nil {
			return nil, 0, err
		}
		switch class {
		case "tags":
			record := map[string]any{
				"name":           name,
				"nextId":         nextID,
				"taskCount":      taskCount,
				"completedCount": completedCount,
			}
			if err := mergeExtra(record, extra); err != nil {
				return nil, 0, fmt.Errorf("tag %q: %w", name, err)
			}
			rec, err := canonicalJSON(record)
			if err != nil {
				return nil, 0, err
			}
			records = append(records, rec)
		case "milestones":
			if !milestones.Valid || milestones.String == "" {
				continue
			}
			registry, err := decodeJSONArray([]byte(milestones.String))
			if err != nil {
				return nil, 0, fmt.Errorf("tag %q milestones registry: %w", name, err)
			}
			for i, entry := range registry {
				rec, err := canonicalJSON(map[string]any{"tag": name, "index": int64(i), "value": entry})
				if err != nil {
					return nil, 0, err
				}
				records = append(records, rec)
			}
		}
	}
	return records, len(records), rows.Err()
}

// taskClassRecords renders the task-derived classes. Within each class the
// order is (tag, numeric task id), then dependency or subtask registry index.
// The tasks record itself excludes dependencies and subtasks, which are their
// own classes.
func taskClassRecords(t *tx, class string) ([][]byte, int, error) {
	cols, err := t.taskSelectColumns()
	if err != nil {
		return nil, 0, err
	}
	rows, err := t.Query(`SELECT ` + cols + ` FROM tasks ORDER BY tag, CAST(id AS INTEGER)`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var records [][]byte
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, 0, err
		}
		recs, err := taskRecords(task, class)
		if err != nil {
			return nil, 0, err
		}
		records = append(records, recs...)
	}
	return records, len(records), rows.Err()
}

func taskRecords(task *Task, class string) ([][]byte, error) {
	switch class {
	case "tasks":
		taskObj := map[string]any{
			"id":          task.ID,
			"title":       task.Title,
			"description": task.Description,
			"details":     task.Details,
			"status":      task.Status,
			"updatedAt":   task.UpdatedAt,
		}
		if task.Priority != "" {
			taskObj["priority"] = task.Priority
		}
		if task.Milestone != "" {
			taskObj["milestone"] = task.Milestone
		}
		if task.Link != nil {
			taskObj["externalLink"] = map[string]any{
				"family":     task.Link.Family,
				"repository": task.Link.Repository,
				"issue":      fmt.Sprintf("%d", task.Link.Issue),
			}
		}
		if err := mergeExtraRaw(taskObj, task.Extra); err != nil {
			return nil, fmt.Errorf("task %s: %w", task.ID, err)
		}
		// The task object is nested: tag and revision frame it at the record
		// level, and outer-record unknown fields render under their own
		// reserved member. Inner and outer unknown fields therefore live in
		// separate namespaces and can never collide — an inner unknown field
		// literally named "outerExtra" is fine.
		record := map[string]any{
			"tag":      task.Tag,
			"revision": int64(task.Revision),
			"task":     taskObj,
		}
		if len(task.OuterExtra) > 0 {
			outer := map[string]any{}
			if err := mergeExtraRaw(outer, task.OuterExtra); err != nil {
				return nil, fmt.Errorf("task %s record: %w", task.ID, err)
			}
			record["outerExtra"] = outer
		}
		rec, err := canonicalJSON(record)
		if err != nil {
			return nil, err
		}
		return [][]byte{rec}, nil
	case "dependencies":
		var records [][]byte
		for i, dep := range task.Dependencies {
			rec, err := canonicalJSON(map[string]any{
				"tag": task.Tag, "taskId": task.ID, "index": int64(i), "dependency": dep,
			})
			if err != nil {
				return nil, err
			}
			records = append(records, rec)
		}
		return records, nil
	case "subtasks":
		raw := task.Subtasks
		if len(raw) == 0 {
			raw = []byte(`[]`)
		}
		subs, err := decodeJSONArray(raw)
		if err != nil {
			return nil, fmt.Errorf("task %s subtasks: %w", task.ID, err)
		}
		var records [][]byte
		for i, sub := range subs {
			rec, err := canonicalJSON(map[string]any{
				"tag": task.Tag, "parentId": task.ID, "index": int64(i), "subtask": sub,
			})
			if err != nil {
				return nil, err
			}
			records = append(records, rec)
		}
		return records, nil
	case "bindings":
		if task.Binding == nil {
			return nil, nil
		}
		record := map[string]any{
			"tag":       task.Tag,
			"taskId":    task.ID,
			"path":      task.Binding.Path,
			"boundary":  task.Binding.Boundary,
			"branch":    task.Binding.Branch,
			"createdAt": task.Binding.CreatedAt,
			"keep":      task.Binding.Keep,
		}
		if err := mergeExtraRaw(record, task.Binding.Extra); err != nil {
			return nil, fmt.Errorf("task %s binding: %w", task.ID, err)
		}
		rec, err := canonicalJSON(record)
		if err != nil {
			return nil, err
		}
		return [][]byte{rec}, nil
	case "claims":
		if task.Claim == nil {
			return nil, nil
		}
		record := map[string]any{
			"tag":        task.Tag,
			"taskId":     task.ID,
			"owner":      task.Claim.Owner,
			"token":      task.Claim.Token,
			"ttl":        int64(task.Claim.TTL),
			"acquiredAt": task.Claim.AcquiredAt,
			"expiresAt":  task.Claim.ExpiresAt,
		}
		if err := mergeExtraRaw(record, task.Claim.Extra); err != nil {
			return nil, fmt.Errorf("task %s claim: %w", task.ID, err)
		}
		rec, err := canonicalJSON(record)
		if err != nil {
			return nil, err
		}
		return [][]byte{rec}, nil
	}
	return nil, fmt.Errorf("unknown task digest class %q", class)
}

// mergeExtraRaw flattens a decoded unknown-fields map into record.
func mergeExtraRaw(record map[string]any, extra map[string]json.RawMessage) error {
	for k, raw := range extra {
		if _, clash := record[k]; clash {
			return fmt.Errorf("unknown field %q collides with a logical field", k)
		}
		v, err := decodeJSON(raw)
		if err != nil {
			return fmt.Errorf("unknown field %q: %w", k, err)
		}
		record[k] = v
	}
	return nil
}

// eventClassRecords renders the events class in numeric sequence order.
func eventClassRecords(t *tx) ([][]byte, int, error) {
	rows, err := t.Query(`SELECT seq, tag, task_id, kind, status, owner, revision, count, action, extra, created_at FROM events ORDER BY seq`)
	if err != nil {
		return nil, 0, fmt.Errorf("read change feed: %w", err)
	}
	defer rows.Close()
	var records [][]byte
	for rows.Next() {
		var seq int64
		var tag, taskID, kind, status, owner, action, extra, createdAt sql.NullString
		var revision, count sql.NullInt64
		if err := rows.Scan(&seq, &tag, &taskID, &kind, &status, &owner, &revision, &count, &action, &extra, &createdAt); err != nil {
			return nil, 0, err
		}
		record := map[string]any{"seq": seq, "kind": kind.String, "createdAt": createdAt.String}
		if tag.Valid {
			record["tag"] = tag.String
		}
		if taskID.Valid {
			record["task"] = taskID.String
		}
		if status.Valid {
			record["status"] = status.String
		}
		if owner.Valid {
			record["owner"] = owner.String
		}
		if revision.Valid {
			record["revision"] = revision.Int64
		}
		if count.Valid {
			record["count"] = count.Int64
		}
		if action.Valid {
			record["action"] = action.String
		}
		if err := mergeExtra(record, extra); err != nil {
			return nil, 0, fmt.Errorf("event %d: %w", seq, err)
		}
		rec, err := canonicalJSON(record)
		if err != nil {
			return nil, 0, err
		}
		records = append(records, rec)
	}
	return records, len(records), rows.Err()
}

// receiptClassRecords renders the receipts class in (kind, receipt id) byte
// order. The receipt body — itself canonical JSON — is the record payload.
func receiptClassRecords(t *tx) ([][]byte, int, error) {
	rows, err := t.Query(`SELECT kind, receipt_id, body FROM receipts ORDER BY kind, receipt_id`)
	if err != nil {
		return nil, 0, fmt.Errorf("read receipts: %w", err)
	}
	defer rows.Close()
	var records [][]byte
	for rows.Next() {
		var kind, receiptID, body string
		if err := rows.Scan(&kind, &receiptID, &body); err != nil {
			return nil, 0, err
		}
		decoded, err := decodeJSON([]byte(body))
		if err != nil {
			return nil, 0, fmt.Errorf("receipt %s: %w", receiptID, err)
		}
		rec, err := canonicalJSON(map[string]any{"kind": kind, "receiptId": receiptID, "receipt": decoded})
		if err != nil {
			return nil, 0, err
		}
		records = append(records, rec)
	}
	return records, len(records), rows.Err()
}
