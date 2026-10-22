package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/store"
)

// claimNextCLISeq hands every helper claim-next a distinct request id so
// each call performs a real acquisition; replay tests pass explicit ids.
var claimNextCLISeq atomic.Int64

// claimNextJSON runs claim-next --owner <owner> --json and decodes its
// stdout object.
func claimNextJSON(t *testing.T, owner string, extra ...string) map[string]any {
	t.Helper()
	args := append([]string{"--json", "--owner", owner,
		"--request-id", fmt.Sprintf("cn-%d", claimNextCLISeq.Add(1))}, extra...)
	code, stdout, stderr := runCmd(cmdClaimNext, args...)
	if code != 0 {
		t.Fatalf("claim-next exit %d: %s", code, stderr)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("claim-next stdout not JSON: %v\n%s", err, stdout)
	}
	return out
}

func TestClaimNextCLISelectionAndEmptyResult(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "first", "second")

	out := claimNextJSON(t, "agent-a")
	if out["id"] != "1" {
		t.Fatalf("first claim-next = %v", out)
	}
	token, ok := out["token"].(float64)
	if !ok || token <= 0 {
		t.Fatalf("first claim-next token = %v", out["token"])
	}
	if _, ok := out["revision"]; !ok {
		t.Fatalf("first claim-next missing revision: %v", out)
	}

	// Human output names the claimed task and the acquisition values.
	code, stdout, stderr := runCmd(cmdClaimNext, "--owner", "agent-b", "--request-id", "cn-human-1")
	if code != 0 {
		t.Fatalf("human claim-next exit %d: %s", code, stderr)
	}
	if !strings.HasPrefix(stdout, "id=2 token=") || !strings.Contains(stdout, "revision=1") {
		t.Fatalf("human claim-next stdout = %q", stdout)
	}

	// Exhausted store: the successful empty result, JSON null, exit 0.
	code, stdout, stderr = runCmd(cmdClaimNext, "--json", "--owner", "agent-c", "--request-id", "cn-empty-1")
	if code != 0 || strings.TrimSpace(stdout) != "null" || stderr != "" {
		t.Fatalf("empty claim-next: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	code, stdout, stderr = runCmd(cmdClaimNext, "--owner", "agent-c", "--request-id", "cn-empty-2")
	if code != 0 || stdout != "no actionable task\n" {
		t.Fatalf("empty human claim-next: exit %d stdout %q", code, stdout)
	}
}

func TestClaimNextCLIUsageRefusals(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "seed")

	for name, args := range map[string][]string{
		"no owner":       {},
		"no request id":  {"--owner", "a"},
		"bad request id": {"--owner", "a", "--request-id", "-bad"},
		"positional":     {"--owner", "a", "--request-id", "r", "1"},
	} {
		if code, stdout, _ := runCmd(cmdClaimNext, args...); code != 2 || stdout != "" {
			t.Fatalf("%s: exit %d stdout %q, want usage exit 2", name, code, stdout)
		}
	}
	if code, stdout, stderr := runCmd(cmdClaimNext, "--owner", "a", "--request-id", "r", "--file", "f.json"); code != 2 || stdout != "" || !strings.Contains(stderr, "--file") {
		t.Fatalf("--file: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if code, stdout, stderr := runCmd(cmdClaimNext, "--owner", "a", "--request-id", "r", "--milestone", "nope"); code != 2 || stdout != "" || !strings.Contains(stderr, "nope") {
		t.Fatalf("unknown milestone: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestClaimNextCLIMilestoneFilter(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "unassigned")
	if code, _, stderr := runCmd(cmdMilestone, "add", "m1"); code != 0 {
		t.Fatalf("milestone add: %s", stderr)
	}
	if code, _, stderr := runCmd(cmdAdd, "--title", "in-m", "--milestone", "m1"); code != 0 {
		t.Fatalf("add: %s", stderr)
	}

	out := claimNextJSON(t, "agent-a", "--milestone", "m1")
	if out["id"] != "2" {
		t.Fatalf("milestone-filtered claim-next = %v, want task 2", out)
	}

	// The §8.3 assignment filter matches only tasks durably assigned to the
	// owner: this store records no assignments to "orch", so the result is
	// the successful empty one — never a fallback to unassigned work.
	code, stdout, stderr := runCmd(cmdClaimNext, "--json", "--owner", "agent-b", "--assignment", "orch", "--request-id", "cn-as-1")
	if code != 0 || strings.TrimSpace(stdout) != "null" || stderr != "" {
		t.Fatalf("assignment filter: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// TestNextNeverAdvertisesClaimedWork pins §8.3/§4: next skips tasks whose
// claim is unexpired and surfaces the next claimable task instead.
func TestNextNeverAdvertisesClaimedWork(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "held", "free")

	if code, _, stderr := runCmd(cmdClaim, "--owner", "holder", "--request-id", "req-next-1", "1"); code != 0 {
		t.Fatalf("claim: %s", stderr)
	}
	code, stdout, stderr := runCmd(cmdNext, "--json")
	if code != 0 {
		t.Fatalf("next exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, `"id": "2"`) && !strings.Contains(stdout, `"id":"2"`) {
		t.Fatalf("next after claim = %s, want task 2", stdout)
	}
	if code, _, stderr := runCmd(cmdClaim, "--owner", "holder", "--request-id", "req-next-2", "2"); code != 0 {
		t.Fatalf("claim 2: %s", stderr)
	}
	code, stdout, _ = runCmd(cmdNext, "--json")
	if code != 0 || strings.TrimSpace(stdout) != "null" {
		t.Fatalf("next with all claimed = %q", stdout)
	}
}

// TestNextNeverAdvertisesNonterminalRun pins the live-store half of §8.3:
// expiration of the claim that started a run does not make that task
// actionable again while the run remains nonterminal.
func TestNextNeverAdvertisesNonterminalRun(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "running", "free")

	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	claim, _, replayed, err := db.AcquireClaim("main", "1", "run-owner", 3600, "claim.next-running")
	if err != nil || replayed {
		db.Close()
		t.Fatalf("claim = %+v replayed=%v err=%v", claim, replayed, err)
	}
	if _, replayed, err := db.StartRun("main", "1", "run-owner", claim.Token, "run.next-running", 300); err != nil || replayed {
		db.Close()
		t.Fatalf("start run replayed=%v err=%v", replayed, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", "file:"+projectStorePath(dir)+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE tasks SET claim_expires_at='2000-01-01T00:00:00Z' WHERE tag='main' AND id='1'`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCmd(cmdNext, "--json")
	if code != 0 {
		t.Fatalf("next exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, `"id": "2"`) && !strings.Contains(stdout, `"id":"2"`) {
		t.Fatalf("next after run claim expiry = %s, want task 2", stdout)
	}
}

// TestClaimGatedFieldMutationCLI pins §8.3's consistent exclusive claim
// semantics at the CLI: append and set-milestone on an unexpired claim
// require --owner and --token (exit 3 without, success with).
func TestClaimGatedFieldMutationCLI(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "gated")
	var token int64
	{
		code, stdout, stderr := runCmd(cmdClaim, "--json", "--owner", "agent-a", "--request-id", "req-gate-1", "1")
		if code != 0 {
			t.Fatalf("claim: %s", stderr)
		}
		var claim struct {
			Token int64 `json:"token"`
		}
		if err := json.Unmarshal([]byte(stdout), &claim); err != nil {
			t.Fatal(err)
		}
		token = claim.Token
	}
	gate := []string{"--owner", "agent-a", "--token", strconv.FormatInt(token, 10)}

	// append without the gate is a typed claim conflict (exit 3, JSON on
	// stderr, empty stdout); with the gate it applies.
	code, stdout, stderr := runCmd(cmdAppend, "--json", "--revision", "1", "1", "note")
	if code != 3 || stdout != "" || !strings.Contains(stderr, "expiresAt") {
		t.Fatalf("ungated append: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	var conflict map[string]any
	if err := json.Unmarshal([]byte(stderr), &conflict); err != nil || conflict["kind"] != "update" {
		t.Fatalf("append conflict JSON = %v (%v)", conflict, err)
	}
	appendArgs := append([]string{"--revision", "1"}, gate...)
	appendArgs = append(appendArgs, "1", "note")
	if code, _, stderr := runCmd(cmdAppend, appendArgs...); code != 0 {
		t.Fatalf("gated append: %s", stderr)
	}

	// set-milestone is gated identically.
	if code, _, stderr := runCmd(cmdMilestone, "add", "m"); code != 0 {
		t.Fatalf("milestone add: %s", stderr)
	}
	if code, stdout, stderr := runCmd(cmdSetMilestone, "--revision", "2", "1", "m"); code != 3 || stdout != "" {
		t.Fatalf("ungated set-milestone: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	setArgs := append([]string{"--revision", "2"}, gate...)
	setArgs = append(setArgs, "1", "m")
	if code, _, stderr := runCmd(cmdSetMilestone, setArgs...); code != 0 {
		t.Fatalf("gated set-milestone: %s", stderr)
	}
}

// TestNextCountsClaimedCancelledDependency pins the §4/§8.3 dependency
// universe: a pending task depending on a claimed cancelled task is still
// actionable — the claim excludes the holder from candidacy, not its row
// from dependency resolution.
func TestNextCountsClaimedCancelledDependency(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "dep")
	if code, _, stderr := runCmd(cmdAdd, "--title", "worker", "--dependencies", "1"); code != 0 {
		t.Fatalf("add dependent worker: %s", stderr)
	}

	code, stdout, stderr := runCmd(cmdClaim, "--json", "--owner", "holder", "--request-id", "req-next-dep-1", "1")
	if code != 0 {
		t.Fatalf("claim: %s", stderr)
	}
	var claim struct {
		Token int64 `json:"token"`
	}
	if err := json.Unmarshal([]byte(stdout), &claim); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCmd(cmdSetStatus, "--revision", "1", "--owner", "holder",
		"--token", strconv.FormatInt(claim.Token, 10), "1", "cancelled"); code != 0 {
		t.Fatalf("gated cancel: %s", stderr)
	}

	// Task 2 depends on the claimed, cancelled task 1: dependency satisfied,
	// and next must advertise it.
	code, stdout, stderr = runCmd(cmdNext, "--json")
	if code != 0 {
		t.Fatalf("next exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, `"id": "2"`) && !strings.Contains(stdout, `"id":"2"`) {
		t.Fatalf("next after claimed-cancelled dependency = %s, want task 2", stdout)
	}
	// The claimed task itself is never advertised.
	code, stdout, _ = runCmd(cmdAdd, "--title", "third", "--dependencies", "2")
	if code != 0 {
		t.Fatal(stderr)
	}
	if code, _, stderr := runCmd(cmdClaim, "--owner", "holder2", "--request-id", "req-next-dep-2", "3"); code != 0 {
		t.Fatalf("claim 3: %s", stderr)
	}
	// With 2 unclaimed-pending and 3 claimed-pending, next reports 2 only
	// after 2 becomes the sole actionable candidate... 2 depends on nothing
	// further, so it stays the answer.
	code, stdout, _ = runCmd(cmdNext, "--json")
	if code != 0 || !strings.Contains(stdout, `"id": "2"`) && !strings.Contains(stdout, `"id":"2"`) {
		t.Fatalf("next = %q (%v), want task 2", stdout, stderr)
	}
}

// TestClaimOwnerAndAssignmentLabelGrammar pins the shared owner-label
// grammar (§8.3): claim, release, and claim-next owners plus claim-next's --assignment
// reject padding, control bytes, invalid UTF-8, and oversize values as
// usage exit 2 before any store is opened.
func TestClaimOwnerAndAssignmentLabelGrammar(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "labeled")

	bad := []string{
		" padded ",               // whitespace padding
		"\towner",                // control byte
		"\x7fdel",                // DEL
		"bad\xffutf8",            // invalid UTF-8
		strings.Repeat("a", 257), // oversize
	}
	for _, owner := range bad {
		code, stdout, stderr := runCmd(cmdClaim, "--owner", owner, "--request-id", "r", "1")
		if code != 2 || stdout != "" || stderr == "" {
			t.Fatalf("claim --owner %q: exit %d stdout %q stderr %q, want usage exit 2", owner, code, stdout, stderr)
		}
	}
	for _, owner := range bad {
		code, stdout, stderr := runCmd(cmdRelease, "--owner", owner, "--force", "1")
		if code != 2 || stdout != "" || stderr == "" {
			t.Fatalf("release --owner %q: exit %d stdout %q stderr %q, want usage exit 2", owner, code, stdout, stderr)
		}
	}
	for _, owner := range bad {
		code, stdout, stderr := runCmd(cmdClaimNext, "--owner", owner, "--request-id", "r")
		if code != 2 || stdout != "" || stderr == "" {
			t.Fatalf("claim-next --owner %q: exit %d stdout %q stderr %q, want usage exit 2", owner, code, stdout, stderr)
		}
	}
	for _, assignment := range bad {
		code, stdout, stderr := runCmd(cmdClaimNext, "--owner", "fine", "--assignment", assignment, "--request-id", "r")
		if code != 2 || stdout != "" || stderr == "" {
			t.Fatalf("claim-next --assignment %q: exit %d stdout %q stderr %q, want usage exit 2", assignment, code, stdout, stderr)
		}
	}
	// A padded owner never reaches the store: the request stays unrecorded.
	if code, stdout, stderr := runCmd(cmdClaim, "--json", "--owner", " padded ", "--request-id", "req-grammar-1", "1"); code != 2 || stdout != "" {
		t.Fatalf("padded owner reached the store: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// TestClaimFencedRemoveAndMilestoneCLI pins the §8.3 fence at the CLI:
// removing a claimed task or a subtask of a claimed parent, force removal
// that strips a claimed dependent, and force milestone clearing over a
// claimed task are exit-3 typed conflicts; an invalid milestone name on a
// claimed task is still the usage exit 2.
func TestClaimFencedRemoveAndMilestoneCLI(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "target", "dependent")
	var token int64
	{
		code, stdout, stderr := runCmd(cmdClaim, "--json", "--owner", "agent-a", "--request-id", "req-fence-cli-1", "1")
		if code != 0 {
			t.Fatalf("claim: %s", stderr)
		}
		var claim struct {
			Token int64 `json:"token"`
		}
		if err := json.Unmarshal([]byte(stdout), &claim); err != nil {
			t.Fatal(err)
		}
		token = claim.Token
	}

	// Ungated removal of the claimed target: exit 3, JSON conflict on
	// stderr, empty stdout; the gated removal succeeds.
	if code, stdout, stderr := runCmd(cmdRemove, "--revision", "1", "1"); code != 3 || stdout != "" || !strings.Contains(stderr, "claim conflict") {
		t.Fatalf("ungated claimed remove: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if code, _, stderr := runCmd(cmdRemove, "--revision", "1", "--owner", "agent-a",
		"--token", strconv.FormatInt(token, 10), "1"); code != 0 {
		t.Fatalf("gated claimed remove: %s", stderr)
	}

	// Force removal stripping a claimed dependent's reference refuses.
	if code, _, stderr := runCmd(cmdAdd, "--title", "second-source"); code != 0 {
		t.Fatal(stderr)
	}
	if code, _, stderr := runCmd(cmdAdd, "--title", "second-dependent", "--dependencies", "2"); code != 0 {
		t.Fatal(stderr)
	}
	if code, _, stderr := runCmd(cmdClaim, "--owner", "agent-b", "--request-id", "req-fence-cli-2", "4"); code != 0 {
		t.Fatalf("claim 3: %s", stderr)
	}
	if code, stdout, stderr := runCmd(cmdRemove, "--revision", "1", "--force", "2"); code != 3 || stdout != "" || !strings.Contains(stderr, "claim conflict") {
		t.Fatalf("force strip of claimed dependent: exit %d stdout %q stderr %q", code, stdout, stderr)
	}

	// Force milestone clearing over a claimed referencing task refuses;
	// after release it succeeds.
	if code, _, stderr := runCmd(cmdRelease, "--owner", "breaker", "--force", "4"); code != 0 {
		t.Fatalf("force release 4: %s", stderr)
	}
	if code, _, stderr := runCmd(cmdMilestone, "add", "m"); code != 0 {
		t.Fatal(stderr)
	}
	if code, _, stderr := runCmd(cmdSetMilestone, "--revision", "1", "4", "m"); code != 0 {
		t.Fatalf("set-milestone: %s", stderr)
	}
	if code, _, stderr := runCmd(cmdClaim, "--owner", "agent-b", "--request-id", "req-fence-cli-2b", "4"); code != 0 {
		t.Fatalf("reclaim 4: %s", stderr)
	}
	if code, stdout, stderr := runCmd(cmdMilestone, "remove", "--force", "m"); code != 3 || stdout != "" || !strings.Contains(stderr, "claim conflict") {
		t.Fatalf("force milestone clear over claim: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if code, _, stderr := runCmd(cmdRelease, "--owner", "breaker", "--force", "4"); code != 0 {
		t.Fatalf("second force release: %s", stderr)
	}
	if code, _, stderr := runCmd(cmdMilestone, "remove", "--force", "m"); code != 0 {
		t.Fatalf("milestone clear after release: %s", stderr)
	}

	// Validation precedes fencing: an invalid or unknown milestone name on
	// a claimed task is the usage exit 2, not the claim conflict.
	if code, _, stderr := runCmd(cmdClaim, "--owner", "agent-c", "--request-id", "req-fence-cli-3", "3"); code != 0 {
		t.Fatalf("claim 3: %s", stderr)
	}
	if code, stdout, stderr := runCmd(cmdSetMilestone, "--revision", "1", "3", " padded "); code != 2 || stdout != "" {
		t.Fatalf("invalid milestone under claim: exit %d stdout %q stderr %q, want usage exit 2", code, stdout, stderr)
	}
	if code, stdout, stderr := runCmd(cmdSetMilestone, "--revision", "1", "3", "nope"); code != 2 || stdout != "" {
		t.Fatalf("unknown milestone under claim: exit %d stdout %q stderr %q, want usage exit 2", code, stdout, stderr)
	}
}
