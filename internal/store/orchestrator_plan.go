package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/noisefloor-oss/tasks/internal/backlog"
)

const (
	OrchestratorPlanSchemaVersion = 1
	OrchestratorPlanMaxBytes      = 8 << 20
	orchestratorPlanDigestDomain  = "noise-tasks-plan-v1\x00"
	orchestratorResultDomain      = "noise-tasks-plan-result-v1\x00"
	ReceiptKindPlan               = "plan"
)

// OrchestratorPlanError is a strict parsing or validation refusal.
type OrchestratorPlanError struct{ Reason string }

func (e *OrchestratorPlanError) Error() string { return "invalid orchestrator plan: " + e.Reason }

func orchestratorPlanErrorf(format string, args ...any) *OrchestratorPlanError {
	return &OrchestratorPlanError{Reason: fmt.Sprintf(format, args...)}
}

// PlanIDConflict reports store-wide reuse of a plan id by a non-identical
// application. It is a typed conflict (CLI exit 3), parallel to ReplayConflict.
type PlanIDConflict struct{ PlanID string }

func (e *PlanIDConflict) Error() string {
	return fmt.Sprintf("plan id %q was already applied with different inputs", e.PlanID)
}

type OrchestratorMilestone struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type PlanMilestoneRef struct {
	Key  string `json:"key,omitempty"`
	Name string `json:"name,omitempty"`
}

type PlanTaskRef struct {
	Key string `json:"key,omitempty"`
	ID  string `json:"id,omitempty"`
}

type OrchestratorTask struct {
	Key          string
	Title        string
	Description  string
	Details      string
	Priority     string
	Milestone    *PlanMilestoneRef
	Dependencies []PlanTaskRef
}

type OrchestratorAssignment struct {
	Key   string
	Task  PlanTaskRef
	Owner string
}

// OrchestratorPlan is the parsed strict §9.2 document. canonical is retained
// as the durable collision proof and digest input.
type OrchestratorPlan struct {
	SchemaVersion int
	PlanID        string
	Tag           string
	Milestones    []OrchestratorMilestone
	Tasks         []OrchestratorTask
	Assignments   []OrchestratorAssignment
	Digest        string
	canonical     []byte
}

type PlanInventory struct {
	Milestones   int `json:"milestones"`
	Tasks        int `json:"tasks"`
	Assignments  int `json:"assignments"`
	Dependencies int `json:"dependencies"`
}

type OrchestratorPlanValidation struct {
	SchemaVersion int           `json:"schemaVersion"`
	PlanID        string        `json:"planId"`
	Tag           string        `json:"tag"`
	PlanDigest    string        `json:"planDigest"`
	Offline       bool          `json:"offline"`
	Inventory     PlanInventory `json:"inventory"`
}

type PlanTaskMapping struct {
	Key string `json:"key"`
	ID  string `json:"id"`
}

type OrchestratorPlanApplyResult struct {
	Receipt  map[string]any `json:"receipt"`
	Replayed bool           `json:"replayed"`
}

// orchestratorPlanFaultHook is a deterministic rollback/crash-injection
// boundary for tests. Returning an error from "before-commit" rolls the
// entire publication back.
var orchestratorPlanFaultHook = func(string) error { return nil }

func ReadOrchestratorPlan(path string) (*OrchestratorPlan, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, orchestratorPlanErrorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() {
		return nil, orchestratorPlanErrorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, OrchestratorPlanMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > OrchestratorPlanMaxBytes {
		return nil, orchestratorPlanErrorf("plan exceeds %d bytes", OrchestratorPlanMaxBytes)
	}
	return ParseOrchestratorPlan(data)
}

func ParseOrchestratorPlan(data []byte) (*OrchestratorPlan, error) {
	if len(data) > OrchestratorPlanMaxBytes {
		return nil, orchestratorPlanErrorf("plan exceeds %d bytes", OrchestratorPlanMaxBytes)
	}
	if !utf8.Valid(data) {
		return nil, orchestratorPlanErrorf("plan is not valid UTF-8")
	}
	value, err := decodeJSON(data)
	if err != nil {
		return nil, orchestratorPlanErrorf("strict JSON: %v", err)
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, orchestratorPlanErrorf("top level is not an object")
	}
	if err := exactPlanKeys(obj, "plan", "schemaVersion", "planId", "tag", "milestones", "tasks", "assignments"); err != nil {
		return nil, err
	}
	p := &OrchestratorPlan{}
	version, err := planInt(obj, "schemaVersion", "plan")
	if err != nil {
		return nil, err
	}
	if version != OrchestratorPlanSchemaVersion {
		return nil, orchestratorPlanErrorf("unsupported schemaVersion %d", version)
	}
	p.SchemaVersion = version
	if p.PlanID, err = planString(obj, "planId", "plan"); err != nil {
		return nil, err
	}
	if !ValidLocalKey(p.PlanID) {
		return nil, orchestratorPlanErrorf("planId %q does not match %s", p.PlanID, localKeyGrammar)
	}
	if p.Tag, err = planString(obj, "tag", "plan"); err != nil {
		return nil, err
	}
	if p.Tag == "" || strings.TrimSpace(p.Tag) != p.Tag || !utf8.ValidString(p.Tag) {
		return nil, orchestratorPlanErrorf("tag must be nonempty, trimmed UTF-8")
	}
	if p.Milestones, err = parsePlanMilestones(obj["milestones"]); err != nil {
		return nil, err
	}
	if p.Tasks, err = parsePlanTasks(obj["tasks"]); err != nil {
		return nil, err
	}
	if p.Assignments, err = parsePlanAssignments(obj["assignments"]); err != nil {
		return nil, err
	}
	if err := validatePlanLocal(p); err != nil {
		return nil, err
	}
	p.canonical, err = canonicalJSON(value)
	if err != nil {
		return nil, orchestratorPlanErrorf("canonical JSON: %v", err)
	}
	sum := sha256.Sum256(append([]byte(orchestratorPlanDigestDomain), p.canonical...))
	p.Digest = "sha256:" + hex.EncodeToString(sum[:])
	return p, nil
}

func exactPlanKeys(obj map[string]any, context string, required ...string) error {
	want := make(map[string]bool, len(required))
	for _, key := range required {
		want[key] = true
	}
	for key := range obj {
		if !want[key] {
			return orchestratorPlanErrorf("%s has unknown key %q", context, key)
		}
	}
	for _, key := range required {
		if _, ok := obj[key]; !ok {
			return orchestratorPlanErrorf("%s is missing key %q", context, key)
		}
	}
	return nil
}

func planString(obj map[string]any, key, context string) (string, error) {
	v, ok := obj[key].(string)
	if !ok {
		return "", orchestratorPlanErrorf("%s key %q is not a string", context, key)
	}
	return v, nil
}

func planInt(obj map[string]any, key, context string) (int, error) {
	n, ok := obj[key].(json.Number)
	if !ok {
		return 0, orchestratorPlanErrorf("%s key %q is not an integer", context, key)
	}
	v, err := strconv.Atoi(n.String())
	if err != nil {
		return 0, orchestratorPlanErrorf("%s key %q is outside integer range", context, key)
	}
	return v, nil
}

func planArray(v any, context string) ([]any, error) {
	a, ok := v.([]any)
	if !ok {
		return nil, orchestratorPlanErrorf("%s is not an array", context)
	}
	return a, nil
}

func parsePlanMilestones(v any) ([]OrchestratorMilestone, error) {
	a, err := planArray(v, "milestones")
	if err != nil {
		return nil, err
	}
	out := make([]OrchestratorMilestone, 0, len(a))
	for i, raw := range a {
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil, orchestratorPlanErrorf("milestones[%d] is not an object", i)
		}
		ctx := fmt.Sprintf("milestones[%d]", i)
		if err := exactPlanKeys(obj, ctx, "key", "name", "status"); err != nil {
			return nil, err
		}
		key, err := planString(obj, "key", ctx)
		if err != nil {
			return nil, err
		}
		name, err := planString(obj, "name", ctx)
		if err != nil {
			return nil, err
		}
		status, err := planString(obj, "status", ctx)
		if err != nil {
			return nil, err
		}
		if !ValidLocalKey(key) {
			return nil, orchestratorPlanErrorf("%s key %q does not match %s", ctx, key, localKeyGrammar)
		}
		if err := backlog.ValidateMilestoneName(name); err != nil {
			return nil, orchestratorPlanErrorf("%s: %v", ctx, err)
		}
		if !backlog.ValidMilestoneStatuses[status] {
			return nil, orchestratorPlanErrorf("%s has invalid status %q", ctx, status)
		}
		out = append(out, OrchestratorMilestone{Key: key, Name: name, Status: status})
	}
	return out, nil
}

func parseMilestoneRef(v any, context string) (*PlanMilestoneRef, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, orchestratorPlanErrorf("%s is not an object", context)
	}
	if len(obj) != 1 {
		return nil, orchestratorPlanErrorf("%s must contain exactly one of key or name", context)
	}
	if _, ok := obj["key"]; ok {
		key, err := planString(obj, "key", context)
		if err != nil || !ValidLocalKey(key) {
			if err != nil {
				return nil, err
			}
			return nil, orchestratorPlanErrorf("%s key %q does not match %s", context, key, localKeyGrammar)
		}
		return &PlanMilestoneRef{Key: key}, nil
	}
	if _, ok := obj["name"]; ok {
		name, err := planString(obj, "name", context)
		if err != nil {
			return nil, err
		}
		if err := backlog.ValidateMilestoneName(name); err != nil {
			return nil, orchestratorPlanErrorf("%s: %v", context, err)
		}
		return &PlanMilestoneRef{Name: name}, nil
	}
	return nil, orchestratorPlanErrorf("%s has unknown reference key", context)
}

func parseTaskRef(v any, context string) (PlanTaskRef, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return PlanTaskRef{}, orchestratorPlanErrorf("%s is not an object", context)
	}
	if len(obj) != 1 {
		return PlanTaskRef{}, orchestratorPlanErrorf("%s must contain exactly one of key or id", context)
	}
	if _, ok := obj["key"]; ok {
		key, err := planString(obj, "key", context)
		if err != nil {
			return PlanTaskRef{}, err
		}
		if !ValidLocalKey(key) {
			return PlanTaskRef{}, orchestratorPlanErrorf("%s key %q does not match %s", context, key, localKeyGrammar)
		}
		return PlanTaskRef{Key: key}, nil
	}
	if _, ok := obj["id"]; ok {
		id, err := planString(obj, "id", context)
		if err != nil {
			return PlanTaskRef{}, err
		}
		if !positivePlanID(id) {
			return PlanTaskRef{}, orchestratorPlanErrorf("%s id %q is not a positive integer string", context, id)
		}
		return PlanTaskRef{ID: id}, nil
	}
	return PlanTaskRef{}, orchestratorPlanErrorf("%s has unknown reference key", context)
}

func positivePlanID(id string) bool {
	if id == "" || id == "0" || len(id) > 1 && id[0] == '0' {
		return false
	}
	for _, c := range []byte(id) {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parsePlanTasks(v any) ([]OrchestratorTask, error) {
	a, err := planArray(v, "tasks")
	if err != nil {
		return nil, err
	}
	out := make([]OrchestratorTask, 0, len(a))
	for i, raw := range a {
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil, orchestratorPlanErrorf("tasks[%d] is not an object", i)
		}
		ctx := fmt.Sprintf("tasks[%d]", i)
		required := []string{"key", "title", "description", "details", "priority", "dependencies"}
		allowed := append(append([]string{}, required...), "milestone")
		for key := range obj {
			found := false
			for _, k := range allowed {
				found = found || key == k
			}
			if !found {
				return nil, orchestratorPlanErrorf("%s has unknown key %q", ctx, key)
			}
		}
		for _, key := range required {
			if _, ok := obj[key]; !ok {
				return nil, orchestratorPlanErrorf("%s is missing key %q", ctx, key)
			}
		}
		var task OrchestratorTask
		if task.Key, err = planString(obj, "key", ctx); err != nil {
			return nil, err
		}
		if !ValidLocalKey(task.Key) {
			return nil, orchestratorPlanErrorf("%s key %q does not match %s", ctx, task.Key, localKeyGrammar)
		}
		if task.Title, err = planString(obj, "title", ctx); err != nil {
			return nil, err
		}
		if task.Title == "" {
			return nil, orchestratorPlanErrorf("%s has an empty title", ctx)
		}
		if task.Description, err = planString(obj, "description", ctx); err != nil {
			return nil, err
		}
		if task.Details, err = planString(obj, "details", ctx); err != nil {
			return nil, err
		}
		if task.Priority, err = planString(obj, "priority", ctx); err != nil {
			return nil, err
		}
		if !backlog.ValidPriorities[task.Priority] {
			return nil, orchestratorPlanErrorf("%s has invalid priority %q", ctx, task.Priority)
		}
		if rawMilestone, ok := obj["milestone"]; ok {
			if task.Milestone, err = parseMilestoneRef(rawMilestone, ctx+".milestone"); err != nil {
				return nil, err
			}
		}
		deps, err := planArray(obj["dependencies"], ctx+".dependencies")
		if err != nil {
			return nil, err
		}
		for j, dep := range deps {
			ref, err := parseTaskRef(dep, fmt.Sprintf("%s.dependencies[%d]", ctx, j))
			if err != nil {
				return nil, err
			}
			task.Dependencies = append(task.Dependencies, ref)
		}
		if task.Dependencies == nil {
			task.Dependencies = []PlanTaskRef{}
		}
		out = append(out, task)
	}
	return out, nil
}

func parsePlanAssignments(v any) ([]OrchestratorAssignment, error) {
	a, err := planArray(v, "assignments")
	if err != nil {
		return nil, err
	}
	out := make([]OrchestratorAssignment, 0, len(a))
	for i, raw := range a {
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil, orchestratorPlanErrorf("assignments[%d] is not an object", i)
		}
		ctx := fmt.Sprintf("assignments[%d]", i)
		if err := exactPlanKeys(obj, ctx, "key", "task", "owner"); err != nil {
			return nil, err
		}
		key, err := planString(obj, "key", ctx)
		if err != nil {
			return nil, err
		}
		if !ValidLocalKey(key) {
			return nil, orchestratorPlanErrorf("%s key %q does not match %s", ctx, key, localKeyGrammar)
		}
		ref, err := parseTaskRef(obj["task"], ctx+".task")
		if err != nil {
			return nil, err
		}
		if ref.Key == "" {
			return nil, orchestratorPlanErrorf("%s.task must reference a local task key", ctx)
		}
		owner, err := planString(obj, "owner", ctx)
		if err != nil {
			return nil, err
		}
		if err := ValidateOwnerLabel(owner); err != nil {
			return nil, orchestratorPlanErrorf("%s.owner: %v", ctx, err)
		}
		out = append(out, OrchestratorAssignment{Key: key, Task: ref, Owner: owner})
	}
	return out, nil
}

// ValidateOwnerLabel enforces the one owner-label grammar shared by plan
// assignment owners (§9.2), claim/claim-next owners, and claim-next's
// --assignment filter (§8.3): a trimmed, nonempty UTF-8 string of at most
// 256 bytes containing no ASCII control character.
func ValidateOwnerLabel(owner string) error {
	if owner == "" || strings.TrimSpace(owner) != owner || !utf8.ValidString(owner) || len(owner) > 256 {
		return errors.New("must be trimmed, nonempty UTF-8 of at most 256 bytes")
	}
	for _, r := range owner {
		if r < 0x20 || r == 0x7f {
			return errors.New("contains an ASCII control character")
		}
	}
	return nil
}

func validatePlanLocal(p *OrchestratorPlan) error {
	milestones := map[string]bool{}
	names := map[string]bool{}
	for _, m := range p.Milestones {
		if milestones[m.Key] {
			return orchestratorPlanErrorf("duplicate milestone key %q", m.Key)
		}
		if names[m.Name] {
			return orchestratorPlanErrorf("duplicate milestone name %q", m.Name)
		}
		milestones[m.Key], names[m.Name] = true, true
	}
	tasks := map[string]bool{}
	for _, task := range p.Tasks {
		if tasks[task.Key] {
			return orchestratorPlanErrorf("duplicate task key %q", task.Key)
		}
		tasks[task.Key] = true
	}
	graph := map[string][]string{}
	for _, task := range p.Tasks {
		seenDeps := map[string]bool{}
		for _, dep := range task.Dependencies {
			if dep.Key != "" {
				if !tasks[dep.Key] {
					return orchestratorPlanErrorf("task %q references unknown local task key %q", task.Key, dep.Key)
				}
				if dep.Key == task.Key {
					return orchestratorPlanErrorf("task %q depends on itself", task.Key)
				}
				if seenDeps["k:"+dep.Key] {
					return orchestratorPlanErrorf("task %q has duplicate dependency key %q", task.Key, dep.Key)
				}
				seenDeps["k:"+dep.Key] = true
				graph["l:"+task.Key] = append(graph["l:"+task.Key], "l:"+dep.Key)
			} else if seenDeps["i:"+dep.ID] {
				return orchestratorPlanErrorf("task %q has duplicate dependency id %q", task.Key, dep.ID)
			} else {
				seenDeps["i:"+dep.ID] = true
			}
		}
		if task.Milestone != nil && task.Milestone.Key != "" && !milestones[task.Milestone.Key] {
			return orchestratorPlanErrorf("task %q references unknown milestone key %q", task.Key, task.Milestone.Key)
		}
	}
	assignmentKeys := map[string]bool{}
	assignedTasks := map[string]bool{}
	for _, assignment := range p.Assignments {
		if assignmentKeys[assignment.Key] {
			return orchestratorPlanErrorf("duplicate assignment key %q", assignment.Key)
		}
		if !tasks[assignment.Task.Key] {
			return orchestratorPlanErrorf("assignment %q references unknown task key %q", assignment.Key, assignment.Task.Key)
		}
		if assignedTasks[assignment.Task.Key] {
			return orchestratorPlanErrorf("task key %q has more than one assignment", assignment.Task.Key)
		}
		assignmentKeys[assignment.Key], assignedTasks[assignment.Task.Key] = true, true
	}
	if cycle := graphCycle(graph); cycle != "" {
		return orchestratorPlanErrorf("local dependency graph contains a cycle at task %q", strings.TrimPrefix(cycle, "l:"))
	}
	return nil
}

func graphCycle(graph map[string][]string) string {
	state := map[string]uint8{}
	var visit func(string) string
	visit = func(node string) string {
		if state[node] == 1 {
			return node
		}
		if state[node] == 2 {
			return ""
		}
		state[node] = 1
		for _, dep := range graph[node] {
			if found := visit(dep); found != "" {
				return found
			}
		}
		state[node] = 2
		return ""
	}
	for node := range graph {
		if found := visit(node); found != "" {
			return found
		}
	}
	return ""
}

func (p *OrchestratorPlan) Inventory() PlanInventory {
	inv := PlanInventory{Milestones: len(p.Milestones), Tasks: len(p.Tasks), Assignments: len(p.Assignments)}
	for _, task := range p.Tasks {
		inv.Dependencies += len(task.Dependencies)
	}
	return inv
}

func (p *OrchestratorPlan) validation(offline bool) *OrchestratorPlanValidation {
	return &OrchestratorPlanValidation{
		SchemaVersion: p.SchemaVersion, PlanID: p.PlanID, Tag: p.Tag,
		PlanDigest: p.Digest, Offline: offline, Inventory: p.Inventory(),
	}
}

func ValidateOrchestratorPlanOffline(p *OrchestratorPlan) *OrchestratorPlanValidation {
	return p.validation(true)
}

func (d *DB) ValidateOrchestratorPlan(p *OrchestratorPlan) (*OrchestratorPlanValidation, error) {
	err := d.deferred(func(t *tx) error { return validateOrchestratorPlanOnline(t, p) })
	if err != nil {
		return nil, err
	}
	return p.validation(false), nil
}

func validateOrchestratorPlanOnline(t *tx, p *OrchestratorPlan) error {
	info, err := t.tagInfo(p.Tag)
	if err != nil {
		return err
	}
	if info == nil {
		return orchestratorPlanErrorf("tag %q does not exist", p.Tag)
	}
	// Validation remains read-only on early v2 databases which predate these
	// tables, while current stores reject an already-published plan id.
	var hasPlans int
	if err := t.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='plans'`).Scan(&hasPlans); err != nil {
		return err
	}
	if hasPlans != 0 {
		var one int
		err := t.QueryRow(`SELECT 1 FROM plans WHERE plan_id=?`, p.PlanID).Scan(&one)
		if err == nil {
			return orchestratorPlanErrorf("plan id %q is already applied", p.PlanID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check plan id %q: %w", p.PlanID, err)
		}
	}
	existingMilestones := map[string]bool{}
	if len(info.Milestones) > 0 {
		entries, err := backlog.ParseMilestones(p.Tag, info.Milestones)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			existingMilestones[entry.Name] = true
		}
	}
	for _, milestone := range p.Milestones {
		if existingMilestones[milestone.Name] {
			return orchestratorPlanErrorf("milestone %q already exists in tag %q", milestone.Name, p.Tag)
		}
	}
	localMilestones := map[string]string{}
	for _, milestone := range p.Milestones {
		localMilestones[milestone.Key] = milestone.Name
	}
	for _, task := range p.Tasks {
		if task.Milestone == nil {
			continue
		}
		name := task.Milestone.Name
		if task.Milestone.Key != "" {
			name = localMilestones[task.Milestone.Key]
		}
		if task.Milestone.Name != "" && !existingMilestones[name] {
			return orchestratorPlanErrorf("task %q references unknown milestone %q", task.Key, name)
		}
	}
	existing, err := t.listTasks(p.Tag)
	if err != nil {
		return err
	}
	existingIDs := map[string]bool{}
	graph := map[string][]string{}
	for _, task := range existing {
		existingIDs[task.ID] = true
	}
	for _, task := range existing {
		for _, dep := range task.Dependencies {
			if !existingIDs[dep] {
				return orchestratorPlanErrorf("existing task %q references missing dependency %q", task.ID, dep)
			}
			graph["e:"+task.ID] = append(graph["e:"+task.ID], "e:"+dep)
		}
	}
	for _, task := range p.Tasks {
		for _, dep := range task.Dependencies {
			if dep.ID != "" {
				if !existingIDs[dep.ID] {
					return orchestratorPlanErrorf("task %q references unknown existing task id %q", task.Key, dep.ID)
				}
				graph["l:"+task.Key] = append(graph["l:"+task.Key], "e:"+dep.ID)
			} else {
				graph["l:"+task.Key] = append(graph["l:"+task.Key], "l:"+dep.Key)
			}
		}
	}
	if cycle := graphCycle(graph); cycle != "" {
		return orchestratorPlanErrorf("combined dependency graph contains a cycle at %q", strings.TrimPrefix(strings.TrimPrefix(cycle, "l:"), "e:"))
	}
	return nil
}

func planInputDigest(requestID string, p *OrchestratorPlan) (string, error) {
	canonical, err := canonicalJSON(map[string]any{"requestId": requestID, "planId": p.PlanID, "planDigest": p.Digest})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func planReceiptID(requestID, planID, digest string) string {
	sum := sha256.Sum256([]byte("noise-tasks-plan-receipt-v1\x00" + requestID + "\x00" + planID + "\x00" + digest))
	return "plan-" + hex.EncodeToString(sum[:8])
}

func mappingValues(mappings []PlanTaskMapping) []any {
	out := make([]any, 0, len(mappings))
	for _, mapping := range mappings {
		out = append(out, map[string]any{"key": mapping.Key, "id": mapping.ID})
	}
	return out
}

func planResultDigest(p *OrchestratorPlan, mappings []PlanTaskMapping) (string, error) {
	inv := p.Inventory()
	canonical, err := canonicalJSON(map[string]any{
		"planId": p.PlanID, "planDigest": p.Digest,
		"inventory": map[string]any{
			"milestones": int64(inv.Milestones), "tasks": int64(inv.Tasks),
			"assignments": int64(inv.Assignments), "dependencies": int64(inv.Dependencies),
		},
		"taskMappings": mappingValues(mappings),
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(orchestratorResultDomain), canonical...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func receiptMappings(receipt map[string]any) ([]PlanTaskMapping, error) {
	raw, ok := receipt["taskMappings"].([]any)
	if !ok {
		return nil, errors.New("stored plan receipt taskMappings is malformed")
	}
	out := make([]PlanTaskMapping, 0, len(raw))
	for _, entry := range raw {
		obj, ok := entry.(map[string]any)
		if !ok {
			return nil, errors.New("stored plan receipt task mapping is malformed")
		}
		key, kok := obj["key"].(string)
		id, iok := obj["id"].(string)
		if !kok || !iok {
			return nil, errors.New("stored plan receipt task mapping is malformed")
		}
		out = append(out, PlanTaskMapping{Key: key, ID: id})
	}
	return out, nil
}

// ApplyOrchestratorPlan revalidates and publishes a complete §9.2 batch in
// one immediate transaction. Identical request replay returns before current
// state validation, because the original publication itself creates the
// duplicate names and ids that validation would otherwise reject.
func (d *DB) ApplyOrchestratorPlan(p *OrchestratorPlan, acceptedDigest, requestID string) (*OrchestratorPlanApplyResult, error) {
	if !digestForm(acceptedDigest) {
		return nil, orchestratorPlanErrorf("accepted digest %q is not a lowercase SHA-256 digest", acceptedDigest)
	}
	if acceptedDigest != p.Digest {
		return nil, orchestratorPlanErrorf("accepted digest %s does not match plan digest %s", acceptedDigest, p.Digest)
	}
	if !ValidLocalKey(requestID) {
		return nil, orchestratorPlanErrorf("request id %q does not match %s", requestID, localKeyGrammar)
	}
	inputDigest, err := planInputDigest(requestID, p)
	if err != nil {
		return nil, err
	}
	result := &OrchestratorPlanApplyResult{}
	err = d.immediate(func(t *tx) error {
		existingReceipt, err := t.findReceipt(ReceiptKindPlan, requestID)
		if err != nil {
			return err
		}
		if existingReceipt != nil {
			if existingReceipt["inputDigest"] != inputDigest || existingReceipt["planId"] != p.PlanID || existingReceipt["planDigest"] != p.Digest {
				return &ReplayConflict{Kind: ReceiptKindPlan, RequestID: requestID}
			}
			var canonicalPlan string
			if err := t.QueryRow(`SELECT canonical_json FROM plans WHERE plan_id=? AND request_id=?`, p.PlanID, requestID).Scan(&canonicalPlan); err != nil {
				return fmt.Errorf("verify replayed plan row: %w", err)
			}
			if canonicalPlan != string(p.canonical) {
				return &ReplayConflict{Kind: ReceiptKindPlan, RequestID: requestID}
			}
			if _, err := receiptMappings(existingReceipt); err != nil {
				return err
			}
			result.Receipt, result.Replayed = existingReceipt, true
			return nil
		}
		var storedDigest, storedRequest, storedCanonical string
		err = t.QueryRow(`SELECT digest, request_id, canonical_json FROM plans WHERE plan_id=?`, p.PlanID).Scan(&storedDigest, &storedRequest, &storedCanonical)
		if err == nil {
			return &PlanIDConflict{PlanID: p.PlanID}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read plan id %q: %w", p.PlanID, err)
		}
		if err := validateOrchestratorPlanOnline(t, p); err != nil {
			return err
		}
		familyID, _, err := t.requireIdentity()
		if err != nil {
			return err
		}
		info, err := t.tagInfo(p.Tag)
		if err != nil {
			return err
		}
		at := nowUTC()
		mappings := make([]PlanTaskMapping, len(p.Tasks))
		idByKey := make(map[string]string, len(p.Tasks))
		for i, task := range p.Tasks {
			id := strconv.Itoa(info.NextID + i)
			mappings[i] = PlanTaskMapping{Key: task.Key, ID: id}
			idByKey[task.Key] = id
		}
		receiptID := planReceiptID(requestID, p.PlanID, p.Digest)
		if _, err := t.Exec(`INSERT INTO plans(plan_id, tag, digest, canonical_json, request_id, receipt_id, applied_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, p.PlanID, p.Tag, p.Digest, string(p.canonical), requestID, receiptID, at); err != nil {
			return fmt.Errorf("record applied plan: %w", err)
		}
		rawMilestones := info.Milestones
		for _, milestone := range p.Milestones {
			entry, err := backlog.MarshalMilestoneEntry(milestone.Name, milestone.Status)
			if err != nil {
				return err
			}
			if len(rawMilestones) == 0 {
				rawMilestones, err = backlog.MarshalMilestones([]backlog.MilestoneEntry{{Name: milestone.Name, Status: milestone.Status, Raw: entry}})
			} else {
				rawMilestones, err = backlog.AppendArrayElement(rawMilestones, entry)
			}
			if err != nil {
				return err
			}
		}
		if len(p.Milestones) > 0 {
			if err := t.writeMilestones(p.Tag, rawMilestones); err != nil {
				return err
			}
		}
		milestoneByKey := map[string]string{}
		for _, milestone := range p.Milestones {
			milestoneByKey[milestone.Key] = milestone.Name
		}
		for i, planned := range p.Tasks {
			deps := make([]string, 0, len(planned.Dependencies))
			for _, ref := range planned.Dependencies {
				if ref.Key != "" {
					deps = append(deps, idByKey[ref.Key])
				} else {
					deps = append(deps, ref.ID)
				}
			}
			milestone := ""
			if planned.Milestone != nil {
				milestone = planned.Milestone.Name
				if planned.Milestone.Key != "" {
					milestone = milestoneByKey[planned.Milestone.Key]
				}
			}
			task := &Task{
				Tag: p.Tag, ID: mappings[i].ID, Title: planned.Title,
				Description: planned.Description, Details: planned.Details,
				Status: "pending", Priority: planned.Priority, Milestone: milestone,
				Dependencies: deps, Subtasks: json.RawMessage(`[]`), Revision: 1, UpdatedAt: at,
			}
			if err := t.insertTask(task); err != nil {
				return err
			}
			if _, err := t.Exec(`INSERT INTO plan_mappings(plan_id, local_key, task_id) VALUES (?, ?, ?)`, p.PlanID, planned.Key, mappings[i].ID); err != nil {
				return fmt.Errorf("record plan task mapping: %w", err)
			}
		}
		for _, assignment := range p.Assignments {
			if _, err := t.Exec(`INSERT INTO assignments(tag, task_id, assignment_key, owner, plan_id) VALUES (?, ?, ?, ?, ?)`,
				p.Tag, idByKey[assignment.Task.Key], assignment.Key, assignment.Owner, p.PlanID); err != nil {
				return fmt.Errorf("record plan assignment: %w", err)
			}
		}
		if err := t.setNextID(p.Tag, info.NextID+len(p.Tasks)); err != nil {
			return err
		}
		if err := t.recountTag(p.Tag); err != nil {
			return err
		}
		resultDigest, err := planResultDigest(p, mappings)
		if err != nil {
			return err
		}
		cd := newClassDigester()
		inv := &ClassInventory{}
		if err := cd.compute(t, inv); err != nil {
			return err
		}
		if inv.Events < MaxRetainedEvents {
			inv.Events++
		}
		inv.Receipts++
		var lastSeq sql.NullInt64
		if err := t.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='events'`).Scan(&lastSeq); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read event sequence: %w", err)
		}
		seq := lastSeq.Int64 + 1
		receipt := map[string]any{
			"schemaVersion": int64(ReceiptSchemaVersion), "kind": ReceiptKindPlan,
			"receiptId": receiptID, "familyId": familyID, "requestId": requestID,
			"inputDigest": inputDigest, "resultDigest": resultDigest,
			"inventory": inventoryRecord(inv), "eventSeq": map[string]any{"first": seq, "last": seq},
			"committedAt": at, "planId": p.PlanID, "planDigest": p.Digest,
			"taskMappings": mappingValues(mappings),
		}
		receiptDigest, err := receiptDigest(receipt)
		if err != nil {
			return err
		}
		extra := map[string]json.RawMessage{
			"receiptId":     json.RawMessage(strconv.Quote(receiptID)),
			"receiptDigest": json.RawMessage(strconv.Quote(receiptDigest)),
		}
		if err := t.appendEvent(Event{Tag: p.Tag, Kind: "plan", Action: "applied", Count: len(p.Tasks), At: at, Extra: extra}); err != nil {
			return err
		}
		var actualSeq int64
		if err := t.QueryRow(`SELECT last_insert_rowid()`).Scan(&actualSeq); err != nil {
			return err
		}
		if actualSeq != seq {
			return fmt.Errorf("plan event sequence changed from %d to %d", seq, actualSeq)
		}
		if _, err := t.insertReceipt(ReceiptKindPlan, receiptID, requestID, receipt, at); err != nil {
			return err
		}
		if err := orchestratorPlanFaultHook("before-commit"); err != nil {
			return err
		}
		result.Receipt = receipt
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
