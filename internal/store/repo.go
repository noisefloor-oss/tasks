package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/noisefloor-oss/tasks/internal/backlog"
)

// taskColumns is the canonical column list for one task row scan on the
// current schema.
const taskColumns = `tag, id, title, description, details, status, priority, milestone,
	dependencies, subtasks, extra, revision, updated_at,
	claim_owner, claim_token, claim_ttl, claim_acquired_at, claim_expires_at, claim_extra,
	wt_path, wt_boundary, wt_branch, wt_created_at, wt_keep, wt_extra, record_extra`

// taskColumnsV1 is taskColumns against a released v1 schema, where
// tasks.record_extra does not exist yet (only the first mutation's §8.2
// upgrade adds it): the column is synthesized as NULL so read paths work
// without mutating the store.
const taskColumnsV1 = `tag, id, title, description, details, status, priority, milestone,
	dependencies, subtasks, extra, revision, updated_at,
	claim_owner, claim_token, claim_ttl, claim_acquired_at, claim_expires_at, claim_extra,
	wt_path, wt_boundary, wt_branch, wt_created_at, wt_keep, wt_extra, NULL AS record_extra`

// taskSelectColumns returns the task-row select list valid for this
// database's schema. user_version is authoritative for the column's
// presence: bootstrap and the v1→v2 upgrade stamp version 2 in the same
// transaction that creates the column, so a pre-upgrade v1 store reads
// record_extra as NULL instead of failing with "no such column" (SPEC
// §8.7: earlier releases' databases stay readable, and reads never mutate).
func (t *tx) taskSelectColumns() (string, error) {
	var version int
	if err := t.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return "", fmt.Errorf("read user_version for task columns: %w", err)
	}
	if version >= recordExtraSchemaVersion {
		return taskColumns, nil
	}
	return taskColumnsV1, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func scanTask(row rowScanner) (*Task, error) {
	var t Task
	var priority, milestone, extraRaw, recordExtra, updatedAt sql.NullString
	var depsRaw, subtasksRaw string
	var claimOwner, claimAcquired, claimExpires, claimExtra sql.NullString
	var claimToken, claimTTL sql.NullInt64
	var wtPath, wtBoundary, wtBranch, wtCreated, wtExtra sql.NullString
	var wtKeep sql.NullInt64
	err := row.Scan(&t.Tag, &t.ID, &t.Title, &t.Description, &t.Details, &t.Status,
		&priority, &milestone, &depsRaw, &subtasksRaw, &extraRaw, &t.Revision, &updatedAt,
		&claimOwner, &claimToken, &claimTTL, &claimAcquired, &claimExpires, &claimExtra,
		&wtPath, &wtBoundary, &wtBranch, &wtCreated, &wtKeep, &wtExtra, &recordExtra)
	if err != nil {
		return nil, err
	}
	t.Priority, t.Milestone, t.UpdatedAt = priority.String, milestone.String, updatedAt.String
	if err := json.Unmarshal([]byte(depsRaw), &t.Dependencies); err != nil {
		return nil, fmt.Errorf("decode task %s dependencies: %w", t.ID, err)
	}
	t.Subtasks = json.RawMessage(subtasksRaw)
	if err := decodeExtra(extraRaw, &t.Extra); err != nil {
		return nil, fmt.Errorf("decode task %s unknown fields: %w", t.ID, err)
	}
	if err := decodeExtra(recordExtra, &t.OuterExtra); err != nil {
		return nil, fmt.Errorf("decode task %s record unknown fields: %w", t.ID, err)
	}
	if claimOwner.Valid {
		t.Claim = &Claim{
			Owner:      claimOwner.String,
			Token:      claimToken.Int64,
			TTL:        int(claimTTL.Int64),
			AcquiredAt: claimAcquired.String,
			ExpiresAt:  claimExpires.String,
		}
		if err := decodeExtra(claimExtra, &t.Claim.Extra); err != nil {
			return nil, fmt.Errorf("decode task %s claim unknown fields: %w", t.ID, err)
		}
	}
	if wtPath.Valid {
		t.Binding = &WorktreeBinding{
			Path:      wtPath.String,
			Boundary:  wtBoundary.String,
			Branch:    wtBranch.String,
			CreatedAt: wtCreated.String,
			Keep:      wtKeep.Int64 != 0,
		}
		if err := decodeExtra(wtExtra, &t.Binding.Extra); err != nil {
			return nil, fmt.Errorf("decode task %s binding unknown fields: %w", t.ID, err)
		}
	}
	return &t, nil
}

func decodeExtra(raw sql.NullString, dst *map[string]json.RawMessage) error {
	if !raw.Valid || raw.String == "" {
		return nil
	}
	return json.Unmarshal([]byte(raw.String), dst)
}

func encodeExtra(extra map[string]json.RawMessage) sql.NullString {
	if len(extra) == 0 {
		return sql.NullString{}
	}
	raw, err := jsonMarshal(extra)
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(raw), Valid: true}
}

func cloneRawMessages(src map[string]json.RawMessage) map[string]json.RawMessage {
	if src == nil {
		return nil
	}
	dst := make(map[string]json.RawMessage, len(src))
	for key, value := range src {
		dst[key] = append(json.RawMessage(nil), value...)
	}
	return dst
}

func cloneClaim(src *Claim) *Claim {
	if src == nil {
		return nil
	}
	dst := *src
	dst.Extra = cloneRawMessages(src.Extra)
	return &dst
}

func cloneBinding(src *WorktreeBinding) *WorktreeBinding {
	if src == nil {
		return nil
	}
	dst := *src
	dst.Extra = cloneRawMessages(src.Extra)
	return &dst
}

func (t *Task) claimArgs() (owner, acquired, expires, extra sql.NullString, token, ttl sql.NullInt64) {
	if t.Claim == nil {
		return
	}
	return nullString(t.Claim.Owner), nullString(t.Claim.AcquiredAt), nullString(t.Claim.ExpiresAt),
		encodeExtra(t.Claim.Extra),
		sql.NullInt64{Int64: t.Claim.Token, Valid: true},
		sql.NullInt64{Int64: int64(t.Claim.TTL), Valid: true}
}

func (t *Task) bindingArgs() (path, boundary, branch, created, extra sql.NullString, keep sql.NullInt64) {
	if t.Binding == nil {
		return
	}
	keepVal := sql.NullInt64{Int64: 0, Valid: true}
	if t.Binding.Keep {
		keepVal.Int64 = 1
	}
	return nullString(t.Binding.Path), nullString(t.Binding.Boundary), nullString(t.Binding.Branch),
		nullString(t.Binding.CreatedAt), encodeExtra(t.Binding.Extra), keepVal
}

// writeTask updates every mutable column of one row. When casRevision >= 0
// the update is conditional on the stored revision matching (SPEC §8.2 CAS).
func (t *tx) writeTask(task *Task, casRevision int) (int64, error) {
	return t.writeTaskConditional(task, casRevision, "", nil)
}

func (t *tx) writeTaskConditional(task *Task, casRevision int, condition string, conditionArgs []any) (int64, error) {
	deps, err := jsonMarshal(task.Dependencies)
	if err != nil {
		return 0, err
	}
	subtasks := task.Subtasks
	if len(subtasks) == 0 {
		subtasks = json.RawMessage(`[]`)
	}
	claimOwner, claimAcquired, claimExpires, claimExtra, claimToken, claimTTL := task.claimArgs()
	wtPath, wtBoundary, wtBranch, wtCreated, wtExtra, wtKeep := task.bindingArgs()
	query := `UPDATE tasks SET title=?, description=?, details=?, status=?, priority=?, milestone=?,
		dependencies=?, subtasks=?, extra=?, record_extra=?, revision=?, updated_at=?,
		claim_owner=?, claim_token=?, claim_ttl=?, claim_acquired_at=?, claim_expires_at=?, claim_extra=?,
		wt_path=?, wt_boundary=?, wt_branch=?, wt_created_at=?, wt_keep=?, wt_extra=?
		WHERE tag=? AND id=?`
	args := []any{
		task.Title, task.Description, task.Details, task.Status,
		nullString(task.Priority), nullString(task.Milestone),
		string(deps), string(subtasks), encodeExtra(task.Extra), encodeExtra(task.OuterExtra), task.Revision, task.UpdatedAt,
		claimOwner, claimToken, claimTTL, claimAcquired, claimExpires, claimExtra,
		wtPath, wtBoundary, wtBranch, wtCreated, wtKeep, wtExtra,
		task.Tag, task.ID,
	}
	if casRevision >= 0 {
		query += ` AND revision=?`
		args = append(args, casRevision)
	}
	query += condition
	args = append(args, conditionArgs...)
	res, err := t.Exec(query, args...)
	if err != nil {
		return 0, fmt.Errorf("write task %s: %w", task.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (t *tx) writeTaskWithRunFence(task *Task, casRevision int, gate *ClaimGate, claim *Claim,
	fence *taskRunFence, at string) (int64, error) {
	if claim == nil {
		return t.writeTaskConditional(task, casRevision,
			` AND (claim_owner IS NULL OR claim_expires_at<=?)`, []any{at})
	}
	condition := ` AND claim_owner=? AND claim_token=? AND claim_expires_at>? AND claim_extra IS ?`
	args := []any{gate.Owner, gate.Token, at, encodeExtra(claim.Extra)}
	if fence != nil {
		condition += ` AND EXISTS(SELECT 1 FROM runs r JOIN attempts a
		ON a.run_id=r.run_id AND a.attempt_no=r.current_attempt
		WHERE r.run_id=? AND r.tag=tasks.tag AND r.task_id=tasks.id AND r.owner=?
		AND r.claim_acquisition_id=? AND r.claim_token=? AND r.status='running'
		AND r.current_attempt=? AND a.status='running' AND a.attempt_token=? AND a.expires_at>?
		AND (SELECT COUNT(*) FROM attempts history WHERE history.run_id=r.run_id)=r.current_attempt
		AND NOT EXISTS(SELECT 1 FROM attempts history WHERE history.run_id=r.run_id
			AND history.attempt_no>r.current_attempt)
		AND NOT EXISTS(SELECT 1 FROM attempts history WHERE history.run_id=r.run_id
			AND history.attempt_no<>r.current_attempt AND history.status NOT IN ('failed','lost')))`
		args = append(args, fence.runID, fence.owner, fence.claimAcquisition,
			fence.claimToken, fence.attempt, fence.attemptToken, at)
	}
	return t.writeTaskConditional(task, casRevision, condition, args)
}

func (t *tx) getTask(tag, id string) (*Task, error) {
	cols, err := t.taskSelectColumns()
	if err != nil {
		return nil, err
	}
	row := t.QueryRow(`SELECT `+cols+` FROM tasks WHERE tag=? AND id=?`, tag, id)
	task, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read task %s: %w", id, err)
	}
	return task, nil
}

// expireClaim clears an in-memory claim that has expired by the local clock
// (SPEC §8.3: expiry judged on access, no reaper). It reports whether the
// claim was expired; the caller inside a mutation persists the clearing.
func expireClaim(task *Task, now time.Time) bool {
	if task.Claim != nil && ClaimExpired(task.Claim, now) {
		task.Claim = nil
		return true
	}
	return false
}

func (t *tx) tagInfo(tag string) (*TagInfo, error) {
	row := t.QueryRow(`SELECT name, next_id, task_count, completed_count, milestones, extra FROM tags WHERE name=?`, tag)
	var info TagInfo
	var milestones, extra sql.NullString
	err := row.Scan(&info.Name, &info.NextID, &info.TaskCount, &info.CompletedCount, &milestones, &extra)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read tag %q: %w", tag, err)
	}
	if milestones.Valid {
		info.Milestones = json.RawMessage(milestones.String)
	}
	if err := decodeExtra(extra, &info.Extra); err != nil {
		return nil, fmt.Errorf("decode tag %q unknown fields: %w", tag, err)
	}
	return &info, nil
}

// ensureTag inserts the tag row when absent and returns its info.
func (t *tx) ensureTag(tag string) (*TagInfo, error) {
	info, err := t.tagInfo(tag)
	if err != nil || info != nil {
		return info, err
	}
	if _, err := t.Exec(`INSERT INTO tags(name, next_id, task_count, completed_count) VALUES (?, 1, 0, 0)`, tag); err != nil {
		return nil, fmt.Errorf("create tag %q: %w", tag, err)
	}
	return &TagInfo{Name: tag, NextID: 1}, nil
}

func (t *tx) setNextID(tag string, nextID int) error {
	if _, err := t.Exec(`UPDATE tags SET next_id=? WHERE name=?`, nextID, tag); err != nil {
		return fmt.Errorf("advance tag %q id counter: %w", tag, err)
	}
	return nil
}

// recountTag recomputes task_count and completed_count from the rows.
func (t *tx) recountTag(tag string) error {
	if _, err := t.Exec(`UPDATE tags SET
		task_count=(SELECT COUNT(*) FROM tasks WHERE tag=?),
		completed_count=(SELECT COUNT(*) FROM tasks WHERE tag=? AND status='done')
		WHERE name=?`, tag, tag, tag); err != nil {
		return fmt.Errorf("recount tag %q: %w", tag, err)
	}
	return nil
}

// nextFence increments and returns the store-wide fencing counter.
func (t *tx) nextFence() (int64, error) {
	var raw string
	if err := t.QueryRow(`SELECT value FROM meta WHERE key='fencing'`).Scan(&raw); err != nil {
		return 0, fmt.Errorf("read fencing counter: %w", err)
	}
	current, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || current < 0 || strconv.FormatInt(current, 10) != raw {
		return 0, fmt.Errorf("advance fencing counter: counter is not a canonical nonnegative integer")
	}
	if current == int64(^uint64(0)>>1) {
		return 0, fmt.Errorf("advance fencing counter: counter is exhausted")
	}
	next := current + 1
	res, err := t.Exec(`UPDATE meta SET value=? WHERE key='fencing' AND value=?`, strconv.FormatInt(next, 10), raw)
	if err != nil {
		return 0, fmt.Errorf("advance fencing counter: %w", err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("confirm fencing counter advance: %w", err)
	}
	if changed != 1 {
		return 0, fmt.Errorf("advance fencing counter: concurrent or missing counter row")
	}
	return next, nil
}

// appendEvent inserts one feed event and compacts the retention window inside
// the same transaction (SPEC §8.4: the feed can never diverge from the state
// change it records). Seq zero draws the next AUTOINCREMENT value.
func (t *tx) appendEvent(e Event) error {
	extra, err := encodeExtraChecked(e.Extra)
	if err != nil {
		return err
	}
	var seq any
	if e.Seq != 0 {
		seq = e.Seq
	}
	if _, err := t.Exec(`INSERT INTO events(seq, tag, task_id, kind, status, owner, revision, count, action, extra, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		seq, nullString(e.Tag), nullString(e.Task), e.Kind,
		nullString(e.Status), nullString(e.Owner),
		nullInt(e.Revision), nullInt(e.Count), nullString(e.Action),
		extra, e.At); err != nil {
		return fmt.Errorf("append %s event: %w", e.Kind, err)
	}
	return t.compactFeed()
}

func encodeExtraChecked(extra map[string]json.RawMessage) (sql.NullString, error) {
	if len(extra) == 0 {
		return sql.NullString{}, nil
	}
	raw, err := jsonMarshal(extra)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(raw), Valid: true}, nil
}

func nullInt(v int) sql.NullInt64 {
	if v == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(v), Valid: true}
}

// compactFeed keeps the newest MaxRetainedEvents rows.
func (t *tx) compactFeed() error {
	_, err := t.Exec(`DELETE FROM events WHERE seq < (
		SELECT MIN(seq) FROM (SELECT seq FROM events ORDER BY seq DESC LIMIT ?))`, MaxRetainedEvents)
	if err != nil {
		return fmt.Errorf("compact change feed: %w", err)
	}
	return nil
}

// --- read snapshots ---

// GetTask reads one task in a deferred transaction. An expired claim is
// reported as absent (SPEC §8.3).
func (d *DB) GetTask(tag, id string) (*Task, error) {
	var task *Task
	err := d.deferred(func(t *tx) error {
		var err error
		task, err = t.getTask(tag, id)
		if err == nil && task != nil {
			expireClaim(task, time.Now())
		}
		return err
	})
	return task, err
}

// ListTasks reads one tag's tasks in id order.
func (d *DB) ListTasks(tag string) ([]*Task, error) {
	var tasks []*Task
	err := d.deferred(func(t *tx) error {
		cols, err := t.taskSelectColumns()
		if err != nil {
			return err
		}
		rows, err := t.Query(`SELECT `+cols+` FROM tasks WHERE tag=? ORDER BY CAST(id AS INTEGER)`, tag)
		if err != nil {
			return fmt.Errorf("list tag %q tasks: %w", tag, err)
		}
		defer rows.Close()
		now := time.Now()
		for rows.Next() {
			task, err := scanTask(rows)
			if err != nil {
				return err
			}
			expireClaim(task, now)
			tasks = append(tasks, task)
		}
		return rows.Err()
	})
	return tasks, err
}

// Tag reads one tag's metadata row, or nil when absent.
func (d *DB) Tag(tag string) (*TagInfo, error) {
	var info *TagInfo
	err := d.deferred(func(t *tx) error {
		var err error
		info, err = t.tagInfo(tag)
		return err
	})
	return info, err
}

// ListTags returns all tag names, sorted.
func (d *DB) ListTags() ([]string, error) {
	var names []string
	err := d.deferred(func(t *tx) error {
		rows, err := t.Query(`SELECT name FROM tags ORDER BY name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			names = append(names, name)
		}
		return rows.Err()
	})
	return names, err
}

// ResolveTag applies the SPEC §4 default: sole tag, else "main".
func (d *DB) ResolveTag(flagTag string) (string, error) {
	names, err := d.ListTags()
	if err != nil {
		return "", err
	}
	has := func(name string) bool {
		for _, n := range names {
			if n == name {
				return true
			}
		}
		return false
	}
	if flagTag != "" {
		if !has(flagTag) {
			return "", fmt.Errorf("tag %q not found (available: %s)", flagTag, strings.Join(names, ", "))
		}
		return flagTag, nil
	}
	if len(names) == 1 {
		return names[0], nil
	}
	if has("main") {
		return "main", nil
	}
	return "", fmt.Errorf("no --tag given and no %q tag (available: %s)", "main", strings.Join(names, ", "))
}

// Fencing reads the store-wide fencing counter.
func (d *DB) Fencing() (int64, error) {
	var token int64
	err := d.deferred(func(t *tx) error {
		return t.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key='fencing'`).Scan(&token)
	})
	return token, err
}

// Feed replays events after since, reporting truncation when since is below
// the retention floor (SPEC §8.4).
func (d *DB) Feed(since int64) (*FeedResult, error) {
	result := &FeedResult{Events: []Event{}}
	err := d.deferred(func(t *tx) error {
		if err := t.QueryRow(`SELECT COALESCE(MIN(seq), 0) FROM events`).Scan(&result.OldestRetainedSeq); err != nil {
			return fmt.Errorf("read feed floor: %w", err)
		}
		if since > 0 && since < result.OldestRetainedSeq {
			result.Truncated = true
			return nil
		}
		rows, err := t.Query(`SELECT seq, tag, task_id, kind, status, owner, revision, count, action, extra, created_at
			FROM events WHERE seq > ? ORDER BY seq`, since)
		if err != nil {
			return fmt.Errorf("read change feed: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e Event
			var tag, task, status, owner, action, extra sql.NullString
			var revision, count sql.NullInt64
			if err := rows.Scan(&e.Seq, &tag, &task, &e.Kind, &status, &owner, &revision, &count, &action, &extra, &e.At); err != nil {
				return err
			}
			e.Tag, e.Task, e.Status, e.Owner, e.Action = tag.String, task.String, status.String, owner.String, action.String
			e.Revision, e.Count = int(revision.Int64), int(count.Int64)
			if err := decodeExtra(extra, &e.Extra); err != nil {
				return fmt.Errorf("decode event %d unknown fields: %w", e.Seq, err)
			}
			result.Events = append(result.Events, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// --- mutations ---

var validStatuses = map[string]bool{
	"pending": true, "in-progress": true, "review": true, "done": true,
	"deferred": true, "cancelled": true, "blocked": true,
}

func validateTaskForWrite(task *Task) error {
	if task.ID == "" {
		return errors.New("task id is empty")
	}
	if !validStatuses[task.Status] {
		return fmt.Errorf("invalid status %q", task.Status)
	}
	if task.Priority != "" && task.Priority != "high" && task.Priority != "medium" && task.Priority != "low" {
		return fmt.Errorf("invalid priority %q", task.Priority)
	}
	return nil
}

func (t *tx) insertTask(task *Task) error {
	deps := task.Dependencies
	if deps == nil {
		deps = []string{}
	}
	depsRaw, err := jsonMarshal(deps)
	if err != nil {
		return err
	}
	subtasks := task.Subtasks
	if len(subtasks) == 0 {
		subtasks = json.RawMessage(`[]`)
	}
	extra, err := encodeExtraChecked(task.Extra)
	if err != nil {
		return err
	}
	recordExtra, err := encodeExtraChecked(task.OuterExtra)
	if err != nil {
		return err
	}
	claimOwner, claimAcquired, claimExpires, claimExtra, claimToken, claimTTL := task.claimArgs()
	wtPath, wtBoundary, wtBranch, wtCreated, wtExtra, wtKeep := task.bindingArgs()
	_, err = t.Exec(`INSERT INTO tasks(tag, id, title, description, details, status, priority, milestone,
		dependencies, subtasks, extra, record_extra, revision, updated_at,
		claim_owner, claim_token, claim_ttl, claim_acquired_at, claim_expires_at, claim_extra,
		wt_path, wt_boundary, wt_branch, wt_created_at, wt_keep, wt_extra)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		task.Tag, task.ID, task.Title, task.Description, task.Details, task.Status,
		nullString(task.Priority), nullString(task.Milestone),
		string(depsRaw), string(subtasks), extra, recordExtra, task.Revision, task.UpdatedAt,
		claimOwner, claimToken, claimTTL, claimAcquired, claimExpires, claimExtra,
		wtPath, wtBoundary, wtBranch, wtCreated, wtKeep, wtExtra)
	if err != nil {
		return fmt.Errorf("insert task %s: %w", task.ID, err)
	}
	return nil
}

// AddTask creates one task, drawing the id from the tag row's next_id inside
// the same immediate transaction (SPEC §8.2: collision-free across processes
// and worktrees without any document-wide scan). A milestone assignment is
// validated against the registry in the same transaction.
func (d *DB) AddTask(tag string, task *Task) (string, error) {
	var id string
	err := d.immediate(func(t *tx) error {
		info, err := t.ensureTag(tag)
		if err != nil {
			return err
		}
		id = strconv.Itoa(info.NextID)
		if err := t.setNextID(tag, info.NextID+1); err != nil {
			return err
		}
		at := nowUTC()
		task.Tag, task.ID, task.UpdatedAt, task.Revision = tag, id, at, 1
		if task.Status == "" {
			task.Status = "pending"
		}
		if task.Dependencies == nil {
			task.Dependencies = []string{}
		}
		if err := validateTaskForWrite(task); err != nil {
			return err
		}
		for _, dep := range task.Dependencies {
			depTask, err := t.getTask(tag, dep)
			if err != nil {
				return err
			}
			if depTask == nil {
				return fmt.Errorf("dependency %s does not exist in tag %q", dep, tag)
			}
		}
		if task.Milestone != "" {
			if err := t.checkMilestoneKnown(tag, task.Milestone); err != nil {
				return err
			}
		}
		if err := t.insertTask(task); err != nil {
			return err
		}
		if err := t.recountTag(tag); err != nil {
			return err
		}
		return t.appendEvent(Event{Task: id, Tag: tag, Kind: "add", Revision: 1, At: at})
	})
	return id, err
}

// AddTasks appends a whole generated batch with contiguous ids and one
// summary add event carrying count (SPEC §8.4).
func (d *DB) AddTasks(tag string, tasks []*Task) ([]string, error) {
	ids := []string{}
	if len(tasks) == 0 {
		return ids, nil
	}
	err := d.immediate(func(t *tx) error {
		info, err := t.ensureTag(tag)
		if err != nil {
			return err
		}
		at := nowUTC()
		for i, task := range tasks {
			id := strconv.Itoa(info.NextID + i)
			ids = append(ids, id)
			task.Tag, task.ID, task.UpdatedAt, task.Revision = tag, id, at, 1
			if task.Status == "" {
				task.Status = "pending"
			}
			if task.Dependencies == nil {
				task.Dependencies = []string{}
			}
			if err := validateTaskForWrite(task); err != nil {
				return err
			}
			if err := t.insertTask(task); err != nil {
				return err
			}
		}
		if err := t.setNextID(tag, info.NextID+len(tasks)); err != nil {
			return err
		}
		if err := t.recountTag(tag); err != nil {
			return err
		}
		return t.appendEvent(Event{Tag: tag, Kind: "add", Count: len(tasks), At: at})
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// staleConflict builds the typed conflict from the current row state.
func (t *tx) staleConflict(tag, id string, supplied int) error {
	current, err := t.getTask(tag, id)
	if err != nil {
		return err
	}
	if current == nil {
		return &TaskNotFound{Tag: tag, ID: id}
	}
	expireClaim(current, time.Now())
	conflict := &StaleWriteConflict{
		Tag:      tag,
		ID:       id,
		Supplied: supplied,
		Current:  current.Revision,
		Status:   current.Status,
	}
	if current.Claim != nil {
		conflict.ClaimOwner = current.Claim.Owner
		conflict.ClaimExpiry = current.Claim.ExpiresAt
	}
	return conflict
}

// UpdateTask applies fn to one task under CAS: the stored revision must equal
// revision, else a typed StaleWriteConflict. fn mutates the in-memory task
// and every proposed value is validated before the claim fence, so an
// independently invalid mutation reports its validation error before any
// claim conflict and nothing persists before the gate (SPEC §8.3). An
// unexpired claim is then gated by its owner and token — the same gate as
// set-status — and the write persists the task with revision+1 and one
// update event.
func (d *DB) UpdateTask(tag, id string, revision int, gate *ClaimGate, fn func(task *Task) error) error {
	if err := ValidateTaskRunGate(gate); err != nil {
		return err
	}
	return d.immediate(func(t *tx) error {
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
		// Claim and binding state are coordination authority, not §1 fields
		// exposed to the proposal callback. Preserve deep copies so a callback
		// cannot clear or rewrite a live fence (including by mutating the
		// pointed-to value) before the gate is evaluated.
		identityTag, identityID, identityRevision := task.Tag, task.ID, task.Revision
		claim := cloneClaim(task.Claim)
		binding := cloneBinding(task.Binding)
		outerExtra := cloneRawMessages(task.OuterExtra)
		// Apply the proposal in memory and validate it first: validation is
		// side-effect free and precedes the fence, and no persistence
		// happens before the gate (SPEC §8.3).
		if err := fn(task); err != nil {
			return err
		}
		task.Tag, task.ID, task.Revision = identityTag, identityID, identityRevision
		task.Claim, task.Binding, task.OuterExtra = claim, binding, outerExtra
		if err := validateTaskForWrite(task); err != nil {
			return err
		}
		fence, err := t.taskMutationRunFence(task, gate, now, "update")
		if err != nil {
			return err
		}
		task.UpdatedAt = now.Format(time.RFC3339)
		oldRevision := task.Revision
		task.Revision++
		if err := taskMutationFaultHook(t, "before-write"); err != nil {
			return err
		}
		n, err := t.writeTaskWithRunFence(task, oldRevision, gate, claim, fence, task.UpdatedAt)
		if err != nil {
			return err
		}
		if n == 0 {
			return t.taskMutationWriteConflict(fence, claim, gate, "update", tag, id, revision)
		}
		if err := t.recountTag(tag); err != nil {
			return err
		}
		return t.appendEvent(Event{Task: id, Tag: tag, Kind: "update", Revision: task.Revision, At: task.UpdatedAt})
	})
}

// SetStatus applies a top-level or subtask status transition with CAS
// revision and claim gating (SPEC §8.2/§8.3). A top-level transition to done
// on a claimed task releases the claim (the durable done floor).
func (d *DB) SetStatus(tag, raw, status string, revision int, gate *ClaimGate) (string, error) {
	if !validStatuses[status] {
		return "", fmt.Errorf("invalid status %q", status)
	}
	parentID, subtaskID, err := backlog.ParseTaskReference(raw)
	if err != nil {
		return "", err
	}
	if err := ValidateTaskRunGate(gate); err != nil {
		return "", err
	}
	var previous string
	err = d.immediate(func(t *tx) error {
		task, err := t.getTask(tag, parentID)
		if err != nil {
			return err
		}
		if task == nil {
			return &TaskNotFound{Tag: tag, ID: raw}
		}
		now := time.Now().UTC()
		expired := expireClaim(task, now)
		if task.Revision != revision {
			return t.staleConflict(tag, parentID, revision)
		}
		claim := cloneClaim(task.Claim)
		at := now.Format(time.RFC3339)
		if subtaskID != "" {
			reloaded, prev, err := prepareSubtaskStatus(task, parentID, subtaskID, status)
			if err != nil {
				return err
			}
			fence, err := t.taskMutationRunFence(task, gate, now, "status")
			if err != nil {
				return err
			}
			if prev == status {
				previous = prev
				return nil
			}
			previous = prev
			return t.publishSubtaskStatus(task, reloaded, parentID, subtaskID, status, revision,
				gate, claim, fence, at)
		}
		fence, err := t.taskMutationRunFence(task, gate, now, "status")
		if err != nil {
			return err
		}
		previous = task.Status
		if expired {
			// The expired claim is cleared by the write below; persist the
			// clearing even on a no-op transition.
			n, err := t.writeTaskWithRunFence(task, task.Revision, gate, claim, fence, at)
			if err != nil {
				return err
			}
			if n == 0 {
				return t.taskMutationWriteConflict(fence, claim, gate, "status", tag, parentID, revision)
			}
		}
		if previous == status {
			return nil
		}
		task.Status = status
		task.UpdatedAt = at
		released := false
		if status == "done" && task.Claim != nil {
			task.Claim = nil
			released = true
		}
		oldRevision := task.Revision
		task.Revision++
		if err := taskMutationFaultHook(t, "before-write"); err != nil {
			return err
		}
		n, err := t.writeTaskWithRunFence(task, oldRevision, gate, claim, fence, at)
		if err != nil {
			return err
		}
		if n == 0 {
			return t.taskMutationWriteConflict(fence, claim, gate, "status", tag, parentID, revision)
		}
		if err := t.recountTag(tag); err != nil {
			return err
		}
		// §9.4: a direct top-level transition to done establishes the
		// runless automatic worktree cleanup authority in this same
		// transaction, audited by the status event below.
		if status == "done" {
			if err := t.scheduleWorktreeAutoCleanup(tag, parentID, 0, at); err != nil {
				return err
			}
		}
		if err := t.appendEvent(Event{Task: parentID, Tag: tag, Kind: "status", Status: status, Revision: task.Revision, At: at}); err != nil {
			return err
		}
		if released {
			return t.appendEvent(Event{Task: parentID, Tag: tag, Kind: "release", Revision: task.Revision, At: at})
		}
		return nil
	})
	return previous, err
}

func prepareSubtaskStatus(task *Task, parentID, subtaskID, status string) (*Task, string, error) {
	fields, err := task.Fields()
	if err != nil {
		return nil, "", err
	}
	updated, previous, err := backlog.UpdateSubtaskStatusRaw(fields, parentID, subtaskID, status)
	if err != nil || previous == status {
		return nil, previous, err
	}
	// Subtask status is the only §1 field mutated; the parent task's other
	// fields stay byte-identical (SPEC §1), including its updatedAt value.
	reloaded, err := taskFromFields(task.Tag, updated)
	if err != nil {
		return nil, "", err
	}
	reloaded.Revision = task.Revision + 1
	reloaded.Claim, reloaded.Binding = task.Claim, task.Binding
	reloaded.UpdatedAt = task.UpdatedAt
	return reloaded, previous, nil
}

func (t *tx) publishSubtaskStatus(task, reloaded *Task, parentID, subtaskID, status string,
	revision int, gate *ClaimGate, claim *Claim, fence *taskRunFence, at string) error {
	if err := taskMutationFaultHook(t, "before-write"); err != nil {
		return err
	}
	n, err := t.writeTaskWithRunFence(reloaded, task.Revision, gate, claim, fence, at)
	if err != nil {
		return err
	}
	if n == 0 {
		return t.taskMutationWriteConflict(fence, claim, gate, "status", task.Tag, parentID, revision)
	}
	return t.appendEvent(Event{Task: parentID + "." + subtaskID, Tag: task.Tag, Kind: "status",
		Status: status, Revision: reloaded.Revision, At: at})
}

// taskFromFields decomposes a §1 task object back into a Task.
func taskFromFields(tag string, fields []byte) (*Task, error) {
	parsed, err := backlog.ReadTask(fields)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(fields, &raw); err != nil {
		return nil, err
	}
	for _, k := range []string{"id", "title", "description", "details", "status", "priority", "milestone", "dependencies", "subtasks", "updatedAt"} {
		delete(raw, k)
	}
	task := &Task{
		Tag:          tag,
		ID:           parsed.ID,
		Title:        parsed.Title,
		Description:  parsed.Description,
		Details:      parsed.Details,
		Status:       parsed.Status,
		Priority:     parsed.Priority,
		Milestone:    parsed.Milestone,
		Dependencies: parsed.Dependencies,
		UpdatedAt:    parsed.UpdatedAt,
		Extra:        raw,
	}
	if len(task.Extra) == 0 {
		task.Extra = nil
	}
	subs := json.RawMessage(`[]`)
	if parsed.Subtasks != nil {
		var err error
		subs, err = jsonMarshal(parsed.Subtasks)
		if err != nil {
			return nil, err
		}
	}
	task.Subtasks = subs
	return task, nil
}

// RemoveTask removes a top-level task or one nested subtask (SPEC §4:
// --force is top-level only and strips dependents transactionally).
// Precedence (SPEC §8.3): the stale-revision conflict comes first, then every
// independently invalid structural or dependency condition — hierarchical
// --force, a missing subtask, dependent subtasks, dependents without --force —
// then the claim fences; validation is side-effect free. The removed row — or
// the subtask's parent row — is gated by its owner and token, and every
// dependent a forced removal rewrites is a secondary task: any unexpired
// claim on it refuses the whole transaction with no operator override for
// field mutation (SPEC §8.3).
func (d *DB) RemoveTask(tag, raw string, revision int, force bool, gate *ClaimGate) ([]string, error) {
	parentID, subtaskID, err := backlog.ParseTaskReference(raw)
	if err != nil {
		return nil, err
	}
	if err := ValidateTaskRunGate(gate); err != nil {
		return nil, err
	}
	var stripped []string
	err = d.immediate(func(t *tx) error {
		task, err := t.getTask(tag, parentID)
		if err != nil {
			return err
		}
		if task == nil {
			return &TaskNotFound{Tag: tag, ID: raw}
		}
		now := time.Now().UTC()
		expireClaim(task, now)
		at := now.Format(time.RFC3339)
		if task.Revision != revision {
			return t.staleConflict(tag, parentID, revision)
		}
		if subtaskID != "" {
			// Structural validation first, all side-effect free (SPEC §8.3:
			// an independently invalid request reports its structural or
			// dependency error before any claim conflict).
			if force {
				return fmt.Errorf("--force is not supported for hierarchical task %s", raw)
			}
			fields, err := task.Fields()
			if err != nil {
				return err
			}
			updated, err := backlog.RemoveSubtaskRaw(fields, parentID, subtaskID)
			if err != nil {
				return err
			}
			dependents, err := t.subtaskDependents(tag, parentID, subtaskID)
			if err != nil {
				return err
			}
			if len(dependents) > 0 {
				return fmt.Errorf("task %s is a possible dependency of task(s) %s; remove dependent subtasks first", raw, strings.Join(dependents, ", "))
			}
			claim := cloneClaim(task.Claim)
			fence, err := t.taskMutationRunFence(task, gate, now, "remove")
			if err != nil {
				return err
			}
			reloaded, err := taskFromFields(tag, updated)
			if err != nil {
				return err
			}
			reloaded.Revision = task.Revision + 1
			reloaded.Claim, reloaded.Binding = task.Claim, task.Binding
			reloaded.UpdatedAt = task.UpdatedAt
			if err := taskMutationFaultHook(t, "before-write"); err != nil {
				return err
			}
			n, err := t.writeTaskWithRunFence(reloaded, task.Revision, gate, claim, fence, at)
			if err != nil {
				return err
			}
			if n == 0 {
				return t.taskMutationWriteConflict(fence, claim, gate, "remove", tag, parentID, revision)
			}
			if err := t.recountTag(tag); err != nil {
				return err
			}
			return t.appendEvent(Event{Task: raw, Tag: tag, Kind: "update", Revision: reloaded.Revision, At: at})
		}
		// Top-level dependency validation precedes the fence (SPEC §8.3).
		dependents, err := t.topLevelDependents(tag, task.ID)
		if err != nil {
			return err
		}
		if len(dependents) > 0 && !force {
			ids := make([]string, len(dependents))
			for i, dep := range dependents {
				ids[i] = dep.ID
			}
			sort.Strings(ids)
			return fmt.Errorf("task %s is a dependency of task(s) %s (use --force to remove it and strip the references)", task.ID, strings.Join(ids, ", "))
		}
		hasWorktreeHistory, err := t.hasTaskWorktreeSaga(tag, parentID)
		if err != nil {
			return err
		}
		if hasWorktreeHistory {
			return &WorktreeConflict{Kind: "history", Tag: tag, ID: parentID}
		}
		hasHistory, err := t.hasTaskRun(tag, parentID)
		if err != nil {
			return err
		}
		if hasHistory {
			return taskRunConflict("remove", tag, parentID, nil)
		}
		claim := cloneClaim(task.Claim)
		fence, err := t.taskMutationRunFence(task, gate, now, "remove")
		if err != nil {
			return err
		}
		return t.removeTopLevel(tag, task, revision, dependents, &stripped, gate, claim, fence, now, at)
	})
	if err != nil {
		return nil, err
	}
	return stripped, nil
}

func (t *tx) subtaskDependents(tag, parentID, subtaskID string) ([]string, error) {
	cols, err := t.taskSelectColumns()
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(`SELECT `+cols+` FROM tasks WHERE tag=?`, tag)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	qualified := parentID + "." + subtaskID
	var dependents []string
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		var subs []json.RawMessage
		if err := json.Unmarshal(task.Subtasks, &subs); err != nil {
			return nil, err
		}
		for _, rawSub := range subs {
			sub, err := backlog.DecodeSubtask(task.ID, rawSub)
			if err != nil {
				return nil, err
			}
			if sub.ParentID == parentID && sub.ID == subtaskID {
				continue
			}
			for _, dep := range sub.Dependencies {
				if dep == qualified || (sub.ParentID == parentID && dep == subtaskID) {
					dependents = append(dependents, sub.ParentID+"."+sub.ID)
					break
				}
			}
		}
	}
	return dependents, rows.Err()
}

// topLevelDependents returns every other task in the tag whose dependencies
// reference targetID — a side-effect free read shared by RemoveTask's
// dependency validation and the forced strip.
func (t *tx) topLevelDependents(tag, targetID string) ([]*Task, error) {
	cols, err := t.taskSelectColumns()
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(`SELECT `+cols+` FROM tasks WHERE tag=? AND id<>?`, tag, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var dependents []*Task
	for rows.Next() {
		other, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		for _, dep := range other.Dependencies {
			if dep == targetID {
				dependents = append(dependents, other)
				break
			}
		}
	}
	return dependents, rows.Err()
}

// removeTopLevel strips the reference from every dependent and deletes the
// row. The caller has already refused the unforced dependency case and gated
// the named target. Stripping is a collateral §1 field rewrite, so any
// unexpired claim on a secondary dependent refuses the whole transaction
// unconditionally: the target's gate can never authorize another task's row,
// and no operator override exists for field mutation (SPEC §8.3). Expired
// leases are cleared by the writes below.
func (t *tx) removeTopLevel(tag string, task *Task, revision int, dependents []*Task, stripped *[]string,
	gate *ClaimGate, claim *Claim, fence *taskRunFence, now time.Time, at string) error {
	for _, dep := range dependents {
		expireClaim(dep, now)
		if _, err := t.taskMutationRunFence(dep, nil, now, "update"); err != nil {
			return err
		}
	}
	if err := taskMutationFaultHook(t, "before-write"); err != nil {
		return err
	}
	for _, dep := range dependents {
		deps := make([]string, 0, len(dep.Dependencies))
		for _, d := range dep.Dependencies {
			if d != task.ID {
				deps = append(deps, d)
			}
		}
		dep.Dependencies = deps
		dep.UpdatedAt = at
		old := dep.Revision
		dep.Revision++
		n, err := t.writeTaskWithRunFence(dep, old, nil, nil, nil, at)
		if err != nil {
			return err
		}
		if n == 0 {
			return &ClaimConflict{Tag: tag, ID: dep.ID, Kind: "update"}
		}
		*stripped = append(*stripped, dep.ID)
		if err := t.appendEvent(Event{Task: dep.ID, Tag: tag, Kind: "update", Revision: dep.Revision, At: at}); err != nil {
			return err
		}
	}
	query := `DELETE FROM tasks WHERE tag=? AND id=? AND revision=?`
	args := []any{tag, task.ID, revision}
	if claim == nil {
		query += ` AND (claim_owner IS NULL OR claim_expires_at<=?)`
		args = append(args, at)
	} else {
		query += ` AND claim_owner=? AND claim_token=? AND claim_expires_at>? AND claim_extra IS ?`
		args = append(args, claim.Owner, claim.Token, at, encodeExtra(claim.Extra))
	}
	query += ` AND NOT EXISTS(SELECT 1 FROM runs WHERE tag=tasks.tag AND task_id=tasks.id)
		AND NOT EXISTS(SELECT 1 FROM worktree_sagas WHERE tag=tasks.tag AND task_id=tasks.id)`
	result, err := t.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("remove task %s: %w", task.ID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		hasWorktreeHistory, err := t.hasTaskWorktreeSaga(tag, task.ID)
		if err != nil {
			return err
		}
		if hasWorktreeHistory {
			return &WorktreeConflict{Kind: "history", Tag: tag, ID: task.ID}
		}
		hasHistory, err := t.hasTaskRun(tag, task.ID)
		if err != nil {
			return err
		}
		if hasHistory {
			return taskRunConflict("remove", tag, task.ID, nil)
		}
		current, err := t.getTask(tag, task.ID)
		if err != nil {
			return err
		}
		if current != nil {
			expireClaim(current, now)
			if err := taskClaimGate(current, gate, "update"); err != nil {
				return err
			}
		}
		return t.taskMutationWriteConflict(fence, claim, gate, "remove", tag, task.ID, revision)
	}
	if err := t.recountTag(tag); err != nil {
		return err
	}
	return t.appendEvent(Event{Task: task.ID, Tag: tag, Kind: "remove", At: at})
}

// AppendDetails appends a timestamped info block to a task's details under
// CAS and the §8.3 claim gate, returning the timestamp used.
func (d *DB) AppendDetails(tag, id, text string, revision int, gate *ClaimGate) (string, error) {
	var stamp string
	err := d.UpdateTask(tag, id, revision, gate, func(task *Task) error {
		stamp = nowUTC()
		task.Details += fmt.Sprintf("\n<info added %s>\n%s\n</info>", stamp, text)
		return nil
	})
	return stamp, err
}

// --- claims (SPEC §8.3) ---

// AcquireClaim fences a claim when no unexpired claim exists, recording its
// immutable §8.4 claim receipt in the same immediate transaction. The fencing
// counter increments inside that transaction; the token is returned only to
// the acquirer or an idempotent replay of the same request, which re-reads
// the recorded acquisition without repeating any effect. The returned
// revision is the task's, read inside the same transaction; replayed reports
// a §8.4 replay of the originally recorded acquisition.
func (d *DB) AcquireClaim(tag, id, owner string, ttl int, requestID string) (*Claim, int, bool, error) {
	if err := ValidateOwnerLabel(owner); err != nil {
		return nil, 0, false, fmt.Errorf("claim owner %v", err)
	}
	if err := validateClaimRequestID(requestID); err != nil {
		return nil, 0, false, err
	}
	if ttl <= 0 {
		ttl = DefaultClaimTTL
	}
	r := claimRequest{operation: "claim", requestID: requestID, tag: tag, task: id, owner: owner, ttl: ttl}
	var claim *Claim
	revision := 0
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
			revision, claim, replayed = recordedRevision, task.Claim, true
			return nil
		}
		task, err = t.getTask(tag, id)
		if err != nil {
			return err
		}
		if task == nil {
			return &TaskNotFound{Tag: tag, ID: id}
		}
		revision = task.Revision
		expireClaim(task, now)
		if task.Claim != nil {
			return &ClaimConflict{Tag: tag, ID: id, Kind: "claim",
				Owner: task.Claim.Owner, Expiry: task.Claim.ExpiresAt, FencingToken: task.Claim.Token}
		}
		nonterminal, err := t.hasNonterminalRun(tag, id)
		if err != nil {
			return err
		}
		if nonterminal {
			// Claim expiry never abandons a run. Keep the ordinary claim
			// conflict shape, but do not disclose the stale acquisition that
			// originally started the still-live run (SPEC §8.3).
			return &ClaimConflict{Tag: tag, ID: id, Kind: "claim"}
		}
		claim, err = writeClaimAcquisition(t, r, task, now)
		return err
	})
	if err != nil {
		return nil, 0, false, err
	}
	return claim, revision, replayed, nil
}

// RenewClaim renews a claim by CAS token; expiresAt = now + ttl. The
// returned revision is the task's, read inside the same transaction.
func (d *DB) RenewClaim(tag, id, owner string, token int64, ttl int) (*Claim, int, error) {
	if err := ValidateOwnerLabel(owner); err != nil {
		return nil, 0, fmt.Errorf("claim owner %v", err)
	}
	var claim *Claim
	revision := 0
	err := d.immediate(func(t *tx) error {
		now := time.Now().UTC()
		task, err := t.getTask(tag, id)
		if err != nil {
			return err
		}
		if task == nil {
			return &TaskNotFound{Tag: tag, ID: id}
		}
		revision = task.Revision
		expireClaim(task, now)
		if task.Claim == nil || task.Claim.Token != token || task.Claim.Owner != owner {
			conflict := &ClaimConflict{Tag: tag, ID: id, Kind: "renew"}
			if task.Claim != nil {
				conflict.Owner = task.Claim.Owner
				conflict.Expiry = task.Claim.ExpiresAt
				conflict.FencingToken = task.Claim.Token
			}
			return conflict
		}
		nonterminal, err := t.hasNonterminalRun(tag, id)
		if err != nil {
			return err
		}
		if nonterminal {
			return &ClaimConflict{Tag: tag, ID: id, Kind: "renew"}
		}
		if ttl <= 0 {
			ttl = task.Claim.TTL
		}
		newToken, err := t.nextFence()
		if err != nil {
			return err
		}
		expires := now.Add(time.Duration(ttl) * time.Second).Format(time.RFC3339)
		res, err := t.Exec(`UPDATE tasks SET claim_token=?, claim_ttl=?, claim_expires_at=?
			WHERE tag=? AND id=? AND claim_owner=? AND claim_token=?`,
			newToken, ttl, expires, tag, id, owner, token)
		if err != nil {
			return fmt.Errorf("renew claim on task %s: %w", id, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return &ClaimConflict{Tag: tag, ID: id, Kind: "renew", Owner: task.Claim.Owner,
				Expiry: task.Claim.ExpiresAt, FencingToken: task.Claim.Token}
		}
		claim = &Claim{Owner: owner, Token: newToken, TTL: ttl,
			AcquiredAt: task.Claim.AcquiredAt, ExpiresAt: expires, Extra: task.Claim.Extra}
		return t.appendEvent(Event{Task: id, Tag: tag, Kind: "claim", Owner: owner, Revision: task.Revision, At: now.Format(time.RFC3339)})
	})
	if err != nil {
		return nil, 0, err
	}
	return claim, revision, nil
}

// ReleaseClaim drops a claim by CAS token, or by force (any owner). A forced
// release is recorded on the feed; a no-op force release records nothing.
func (d *DB) ReleaseClaim(tag, id, owner string, token int64, force bool) error {
	if err := ValidateOwnerLabel(owner); err != nil {
		return fmt.Errorf("claim owner %v", err)
	}
	return d.immediate(func(t *tx) error {
		now := time.Now().UTC()
		task, err := t.getTask(tag, id)
		if err != nil {
			return err
		}
		if task == nil {
			return &TaskNotFound{Tag: tag, ID: id}
		}
		physicalClaim := task.Claim
		expireClaim(task, now)
		if !force && (task.Claim == nil || task.Claim.Token != token || task.Claim.Owner != owner) {
			conflict := &ClaimConflict{Tag: tag, ID: id, Kind: "release"}
			if task.Claim != nil {
				conflict.Owner = task.Claim.Owner
				conflict.Expiry = task.Claim.ExpiresAt
				conflict.FencingToken = task.Claim.Token
			}
			return conflict
		}
		run, err := t.nonterminalRunForTask(tag, id)
		if err != nil {
			return err
		}
		if !force && run != nil {
			return &ClaimConflict{Tag: tag, ID: id, Kind: "release"}
		}
		if physicalClaim == nil {
			if run != nil {
				return fmt.Errorf("task %s/%s has a nonterminal run without a physical claim", tag, id)
			}
			return nil
		}
		if force {
			if err := t.forceCancelRunForRelease(run, tag, id, now.Format(time.RFC3339)); err != nil {
				return err
			}
		}
		if !force && (physicalClaim.Token != token || physicalClaim.Owner != owner) {
			return &ClaimConflict{Tag: tag, ID: id, Kind: "release",
				Owner: physicalClaim.Owner, Expiry: physicalClaim.ExpiresAt, FencingToken: physicalClaim.Token}
		}
		owner = physicalClaim.Owner
		res, err := t.Exec(`UPDATE tasks SET claim_owner=NULL, claim_token=NULL, claim_ttl=NULL,
			claim_acquired_at=NULL, claim_expires_at=NULL, claim_extra=NULL
			WHERE tag=? AND id=? AND claim_owner=? AND claim_token=?`,
			tag, id, physicalClaim.Owner, physicalClaim.Token)
		if err != nil {
			return fmt.Errorf("release claim on task %s: %w", id, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return &ClaimConflict{Tag: tag, ID: id, Kind: "release",
				Owner: physicalClaim.Owner, Expiry: physicalClaim.ExpiresAt, FencingToken: physicalClaim.Token}
		}
		return t.appendEvent(Event{Task: id, Tag: tag, Kind: "release", Owner: owner, Revision: task.Revision, At: now.Format(time.RFC3339)})
	})
}

// --- worktree bindings (SPEC §9.1; no revision touch per §8.2) ---

// SetBinding records the task's worktree binding without touching the
// revision, appending one worktree event with action "created".
func (d *DB) SetBinding(tag, id string, binding *WorktreeBinding) error {
	return d.immediate(func(t *tx) error {
		task, err := t.getTask(tag, id)
		if err != nil {
			return err
		}
		if task == nil {
			return &TaskNotFound{Tag: tag, ID: id}
		}
		task.Binding = binding
		if _, err := t.writeTask(task, -1); err != nil {
			return err
		}
		return t.appendEvent(Event{Task: id, Tag: tag, Kind: "worktree", Action: "created", Revision: task.Revision, At: nowUTC()})
	})
}

// ClearBinding drops the task's worktree binding without touching the
// revision, appending one worktree event with action "removed".
func (d *DB) ClearBinding(tag, id string) error {
	return d.immediate(func(t *tx) error {
		task, err := t.getTask(tag, id)
		if err != nil {
			return err
		}
		if task == nil {
			return &TaskNotFound{Tag: tag, ID: id}
		}
		if task.Binding == nil {
			return nil
		}
		task.Binding = nil
		if _, err := t.writeTask(task, -1); err != nil {
			return err
		}
		return t.appendEvent(Event{Task: id, Tag: tag, Kind: "worktree", Action: "removed", Revision: task.Revision, At: nowUTC()})
	})
}

// --- milestone registry (SPEC §12.3) ---

// Milestones returns the tag's §12 registry value byte-faithfully, or nil.
func (d *DB) Milestones(tag string) (json.RawMessage, error) {
	info, err := d.Tag(tag)
	if err != nil || info == nil {
		return nil, err
	}
	return info.Milestones, nil
}

// SetMilestones writes the tag's registry value and appends one content-free
// milestone event (SPEC §12.3).
func (d *DB) SetMilestones(tag string, raw json.RawMessage) error {
	return d.immediate(func(t *tx) error {
		if _, err := t.ensureTag(tag); err != nil {
			return err
		}
		var val sql.NullString
		if len(raw) > 0 {
			val = sql.NullString{String: string(raw), Valid: true}
		}
		if _, err := t.Exec(`UPDATE tags SET milestones=? WHERE name=?`, val, tag); err != nil {
			return fmt.Errorf("write tag %q milestone registry: %w", tag, err)
		}
		return t.appendEvent(Event{Tag: tag, Kind: "milestone", Count: 1, At: nowUTC()})
	})
}

// EnsureTag creates the tag row when absent, without appending any feed
// event. Used by the bootstrap commands (init/setup) to create an empty tag.
func (d *DB) EnsureTag(tag string) error {
	return d.immediate(func(t *tx) error {
		_, err := t.ensureTag(tag)
		return err
	})
}

// AddBatch appends a generated batch atomically (SPEC §3): ids come from the
// tag row's next_id inside the immediate transaction, batchDeps carry each
// draft's 1-based batch-relative dependencies and are remapped to the
// assigned ids, and one summary add event carries the batch count.
func (d *DB) AddBatch(tag string, tasks []*Task, batchDeps [][]int) (ids []string, depIDs [][]string, err error) {
	ids = []string{}
	if len(tasks) == 0 {
		return ids, nil, nil
	}
	depIDs = make([][]string, len(tasks))
	err = d.immediate(func(t *tx) error {
		info, err := t.ensureTag(tag)
		if err != nil {
			return err
		}
		at := nowUTC()
		for i, task := range tasks {
			id := strconv.Itoa(info.NextID + i)
			ids = append(ids, id)
			deps := make([]string, len(batchDeps[i]))
			for j, idx := range batchDeps[i] {
				deps[j] = strconv.Itoa(info.NextID + idx - 1)
			}
			if len(deps) > 0 {
				depIDs[i] = deps
			}
			task.Tag, task.ID, task.UpdatedAt, task.Revision = tag, id, at, 1
			task.Dependencies = deps
			if task.Status == "" {
				task.Status = "pending"
			}
			if err := validateTaskForWrite(task); err != nil {
				return err
			}
			if err := t.insertTask(task); err != nil {
				return err
			}
		}
		if err := t.setNextID(tag, info.NextID+len(tasks)); err != nil {
			return err
		}
		if err := t.recountTag(tag); err != nil {
			return err
		}
		return t.appendEvent(Event{Tag: tag, Kind: "add", Count: len(tasks), At: at})
	})
	if err != nil {
		return nil, nil, err
	}
	return ids, depIDs, nil
}

// WithImmediateLock runs fn while holding an immediate transaction on the
// database, serializing the caller against every other writer of this store
// (SPEC §2.3 setup re-observation runs under it).
func (d *DB) WithImmediateLock(fn func() error) error {
	return d.immediate(func(t *tx) error { return fn() })
}
