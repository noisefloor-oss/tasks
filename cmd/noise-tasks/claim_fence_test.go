package main

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/store"
)

func TestClaimFenceCLIMapsTaskMutationConflicts(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "target")
	code, stdout, stderr := runCmd(cmdClaim, "--json", "--owner", "agent-a", "--request-id", "req-fence-cli-target", "1")
	if code != 0 {
		t.Fatalf("claim: %s", stderr)
	}
	var claimed struct {
		Token int64 `json:"token"`
	}
	if err := json.Unmarshal([]byte(stdout), &claimed); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := runCmd(cmdAppend, "--json", "--revision", "1", "1", "note"); code != 3 || stdout != "" || !strings.Contains(stderr, `"kind": "update"`) {
		t.Fatalf("ungated append: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	gate := []string{"--owner", "agent-a", "--token", strconv.FormatInt(claimed.Token, 10)}
	args := append([]string{"--revision", "1"}, gate...)
	args = append(args, "1", "note")
	if code, _, stderr := runCmd(cmdAppend, args...); code != 0 {
		t.Fatalf("gated append: %s", stderr)
	}
	if code, stdout, stderr := runCmd(cmdRemove, "--json", "--revision", "2", "1"); code != 3 || stdout != "" || !strings.Contains(stderr, `"kind": "update"`) {
		t.Fatalf("ungated remove: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestClaimFenceCLIMapsForcedMilestoneClear(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "milestoned")
	if code, _, stderr := runCmd(cmdMilestone, "add", "m"); code != 0 {
		t.Fatal(stderr)
	}
	if code, _, stderr := runCmd(cmdSetMilestone, "--revision", "1", "1", "m"); code != 0 {
		t.Fatal(stderr)
	}
	if code, _, stderr := runCmd(cmdClaim, "--owner", "agent-a", "--request-id", "req-fence-cli-milestone", "1"); code != 0 {
		t.Fatal(stderr)
	}
	if code, stdout, stderr := runCmd(cmdMilestone, "remove", "--json", "--force", "m"); code != 3 || stdout != "" || !strings.Contains(stderr, `"kind": "update"`) {
		t.Fatalf("forced milestone clear: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if code, stdout, _ := runCmd(cmdSetMilestone, "--revision", "2", "1", " padded "); code != 2 || stdout != "" {
		t.Fatalf("invalid milestone precedence: exit %d stdout %q", code, stdout)
	}
}

// TestClaimFenceCLIForcedRemoveRefusesClaimedDependentGate pins the CLI
// surface of §8.3 collateral fencing: a forced remove cannot strip a claimed
// dependent's reference even when that dependent's own owner and token gate
// the command, and the refusal covers the whole transaction.
func TestClaimFenceCLIForcedRemoveRefusesClaimedDependentGate(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "source")
	if code, _, stderr := runCmd(cmdAdd, "--title", "dependent", "--dependencies", "1"); code != 0 {
		t.Fatal(stderr)
	}
	code, stdout, stderr := runCmd(cmdClaim, "--json", "--owner", "agent-a", "--request-id", "req-cli-collateral", "2")
	if code != 0 {
		t.Fatalf("claim: %s", stderr)
	}
	var claimed struct {
		Token int64 `json:"token"`
	}
	if err := json.Unmarshal([]byte(stdout), &claimed); err != nil {
		t.Fatal(err)
	}
	args := []string{"--json", "--force", "--revision", "1",
		"--owner", "agent-a", "--token", strconv.FormatInt(claimed.Token, 10), "1"}
	if code, stdout, stderr := runCmd(cmdRemove, args...); code != 3 || stdout != "" || !strings.Contains(stderr, `"kind": "update"`) {
		t.Fatalf("forced remove with dependent's gate: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// Neither row changed: the source is still present and the dependent
	// still references it.
	if code, _, stderr := runCmd(cmdShow, "--json", "1"); code != 0 {
		t.Fatalf("source removed by refused transaction: %s", stderr)
	}
	if code, stdout, stderr := runCmd(cmdRemove, "--revision", "1", "1"); code != 1 || !strings.Contains(stderr, "is a dependency of task(s) 2") {
		t.Fatalf("dependent lost its reference: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// TestClaimFenceCLIRemoveValidationPrecedesFencing pins the CLI surface of
// the §8.3 precedence: the dependency refusal is reported (exit 1) before
// the claimed target's fence (exit 3).
func TestClaimFenceCLIRemoveValidationPrecedesFencing(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "target")
	if code, _, stderr := runCmd(cmdAdd, "--title", "dependent", "--dependencies", "1"); code != 0 {
		t.Fatal(stderr)
	}
	if code, _, stderr := runCmd(cmdClaim, "--owner", "agent-a", "--request-id", "req-cli-remove", "1"); code != 0 {
		t.Fatal(stderr)
	}
	if code, stdout, stderr := runCmd(cmdRemove, "--revision", "1", "1"); code != 1 || stdout != "" || !strings.Contains(stderr, "is a dependency of") {
		t.Fatalf("unforced dependent remove: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if code, stdout, stderr := runCmd(cmdRemove, "--json", "--force", "--revision", "1", "1"); code != 3 || stdout != "" || !strings.Contains(stderr, `"kind": "update"`) {
		t.Fatalf("forced claimed remove: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// TestClaimFenceCLIRemoveSubtaskValidationPrecedesFencing pins the CLI
// surface of the hierarchical refusals: --force and a missing subtask are
// reported (exit 1) before the claimed parent's fence (exit 3).
func TestClaimFenceCLIRemoveSubtaskValidationPrecedesFencing(t *testing.T) {
	dir := chdirGitDir(t)
	db, err := store.CreateWithOwner(projectStorePath(dir), dir)
	if err != nil {
		t.Fatal(err)
	}
	subs := json.RawMessage(`[{"id":1,"title":"leaf","status":"pending","dependencies":[],"parentTaskId":"1"}]`)
	if _, err := db.AddTask("main", &store.Task{Title: "parent", Subtasks: subs}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCmd(cmdClaim, "--owner", "agent-a", "--request-id", "req-cli-subtask", "1"); code != 0 {
		t.Fatal(stderr)
	}
	if code, stdout, stderr := runCmd(cmdRemove, "--force", "--revision", "1", "1.1"); code != 2 || stdout != "" || !strings.Contains(stderr, "--force is not supported") {
		t.Fatalf("hierarchical force: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if code, stdout, stderr := runCmd(cmdRemove, "--revision", "1", "1.9"); code != 1 || stdout != "" || !strings.Contains(stderr, "not found") {
		t.Fatalf("missing subtask: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if code, stdout, stderr := runCmd(cmdRemove, "--json", "--revision", "1", "1.1"); code != 3 || stdout != "" || !strings.Contains(stderr, `"kind": "update"`) {
		t.Fatalf("ungated subtask remove: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestRemoveHierarchicalForceIsUsageBeforeResolution(t *testing.T) {
	chdirGitDir(t) // no authoritative store exists
	if code, stdout, stderr := runCmd(cmdRemove, "--force", "--revision", "1", "1.1"); code != 2 || stdout != "" || !strings.Contains(stderr, "--force is not supported") {
		t.Fatalf("store-independent hierarchical force: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	missing := filepath.Join(t.TempDir(), "missing.json")
	if code, stdout, stderr := runCmd(cmdRemove, "--file", missing, "--force", "1.1"); code != 2 || stdout != "" || !strings.Contains(stderr, "--force is not supported") {
		t.Fatalf("fixture-independent hierarchical force: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// TestClaimFenceCLIAppendStaleRevisionPrecedesFence pins the CLI surface of
// the update precedence: the stale revision is reported before the claim
// conflict (both exit 3; only the conflict kind distinguishes them).
func TestClaimFenceCLIAppendStaleRevisionPrecedesFence(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "target")
	if code, _, stderr := runCmd(cmdClaim, "--owner", "agent-a", "--request-id", "req-cli-append", "1"); code != 0 {
		t.Fatal(stderr)
	}
	if code, stdout, stderr := runCmd(cmdAppend, "--json", "--revision", "99", "1", "note"); code != 3 || stdout != "" || !strings.Contains(stderr, `"kind": "stale-write"`) {
		t.Fatalf("stale append: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}
