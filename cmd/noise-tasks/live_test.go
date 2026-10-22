package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/store"
)

// seedProjectStore creates the §8.1 database for the project rooted at dir
// (a plain .git-dir checkout) with one tag and one task per title.
func seedProjectStore(t *testing.T, dir, tag string, titles ...string) {
	t.Helper()
	db, err := store.CreateWithOwner(projectStorePath(dir), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.EnsureTag(tag); err != nil {
		t.Fatal(err)
	}
	for _, title := range titles {
		if _, err := db.AddTask(tag, &store.Task{Title: title, Status: "pending"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLiveListAndShowJSONExposeRevisionAndClaim(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "seed")

	code, stdout, stderr := runCmd(cmdList, "--json")
	if code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, `"revision"`) {
		t.Errorf("list --json missing revision")
	}

	code, stdout, stderr = runCmd(cmdShow, "--json", "1")
	if code != 0 {
		t.Fatalf("show exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, `"revision"`) {
		t.Errorf("show --json missing revision")
	}

	// Acquire a claim and verify the token is not surfaced.
	code, stdout, stderr = runCmd(cmdClaim, "--owner", "a", "--request-id", "req-live-1", "1")
	if code != 0 {
		t.Fatalf("claim exit %d, stderr: %s", code, stderr)
	}
	code, stdout, stderr = runCmd(cmdShow, "--json", "1")
	if code != 0 {
		t.Fatalf("show after claim exit %d, stderr: %s", code, stderr)
	}
	for _, forbidden := range []string{"\"token\"", "fencingToken"} {
		if strings.Contains(stdout, forbidden) {
			t.Fatalf("show leaked token field %q: %s", forbidden, stdout)
		}
	}
	if !strings.Contains(stdout, `"owner"`) {
		t.Errorf("show missing claim owner")
	}
}

func TestLiveMutationRequiresRevision(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "seed")

	code, stdout, stderr := runCmd(cmdSetStatus, "1", "done")
	if code != 2 {
		t.Fatalf("set-status without --revision exit = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("usage error must not write stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "--revision") {
		t.Fatalf("stderr = %q, want --revision usage message", stderr)
	}
}

func TestStaleRevisionJSONConflict(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "seed")

	if code, _, stderr := runCmd(cmdSetStatus, "--revision", "1", "1", "in-progress"); code != 0 {
		t.Fatalf("first set-status exit %d, stderr: %s", code, stderr)
	}

	code, stdout, stderr := runCmd(cmdSetStatus, "--json", "--revision", "1", "1", "done")
	if code != 3 {
		t.Fatalf("stale revision exit = %d, want 3", code)
	}
	if stdout != "" {
		t.Fatalf("conflict must leave stdout empty, got %q", stdout)
	}
	var conflict map[string]any
	if err := json.Unmarshal([]byte(stderr), &conflict); err != nil {
		t.Fatalf("stderr is not JSON: %v\n%s", err, stderr)
	}
	if conflict["kind"] != "stale-write" || conflict["current"] != float64(2) || conflict["status"] != "in-progress" {
		t.Fatalf("conflict JSON = %+v", conflict)
	}
}

func TestClaimConflictJSONRedactsToken(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "seed")

	if code, _, stderr := runCmd(cmdClaim, "--owner", "alice", "--request-id", "req-redact-1", "1"); code != 0 {
		t.Fatalf("claim exit %d, stderr: %s", code, stderr)
	}

	code, stdout, stderr := runCmd(cmdClaim, "--json", "--owner", "bob", "--request-id", "req-redact-2", "1")
	if code != 3 {
		t.Fatalf("claim conflict exit = %d, want 3", code)
	}
	if stdout != "" {
		t.Fatalf("conflict must leave stdout empty, got %q", stdout)
	}
	var conflict map[string]any
	if err := json.Unmarshal([]byte(stderr), &conflict); err != nil {
		t.Fatalf("stderr is not JSON: %v\n%s", err, stderr)
	}
	if conflict["kind"] != "claim" {
		t.Fatalf("conflict kind = %q", conflict["kind"])
	}
	for _, forbidden := range []string{"token", "fencing"} {
		if strings.Contains(stderr, forbidden) {
			t.Fatalf("claim conflict leaked token (%q): %s", forbidden, stderr)
		}
	}
}

func TestLiveFeedTruncatedJSON(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "seed")

	// Generate enough events in-process to truncate sequence 1.
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < store.MaxRetainedEvents+2; i++ {
		status := "in-progress"
		if i%2 == 1 {
			status = "pending"
		}
		if _, err := db.SetStatus("main", "1", status, 1+i, nil); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	code, stdout, stderr := runCmd(cmdLiveFeed, "--json", "--since", "1")
	if code != 0 {
		t.Fatalf("feed exit %d, stderr: %s", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("truncated JSON feed must not write stderr, got %q", stderr)
	}
	var feed struct {
		Events            []map[string]any `json:"events"`
		OldestRetainedSeq int64            `json:"oldestRetainedSeq"`
		Truncated         bool             `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(stdout), &feed); err != nil {
		t.Fatalf("feed stdout not JSON: %v\n%s", err, stdout)
	}
	if !feed.Truncated {
		t.Fatal("feed did not report truncated")
	}
	if len(feed.Events) != 0 {
		t.Fatalf("truncated feed returned %d events", len(feed.Events))
	}
	if feed.OldestRetainedSeq <= 1 {
		t.Fatalf("oldestRetainedSeq = %d", feed.OldestRetainedSeq)
	}
}

func TestLiveFeedSeqStartsAtOne(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "seed")

	code, stdout, stderr := runCmd(cmdLiveFeed, "--json")
	if code != 0 {
		t.Fatalf("feed exit %d, stderr: %s", code, stderr)
	}
	var feed struct {
		Events []struct {
			Seq  int64  `json:"seq"`
			Kind string `json:"kind"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(stdout), &feed); err != nil {
		t.Fatalf("feed stdout not JSON: %v\n%s", err, stdout)
	}
	if len(feed.Events) == 0 {
		t.Fatal("feed after add is empty; the add event must be visible")
	}
	if feed.Events[0].Seq != 1 {
		t.Fatalf("first event seq = %d, want 1", feed.Events[0].Seq)
	}
}

// TestLegacyJSONStoreRefusalAcrossCommands pins the §2/§8.6 cutover refusal:
// with a legacy JSON live store and no database, every normal command exits 1
// with the exact diagnostic and an empty stdout.
func TestLegacyJSONStoreRefusalAcrossCommands(t *testing.T) {
	dir := chdirGitDir(t)
	legacy := filepath.Join(dir, ".git", "noise-tasks", "live.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	want := "noise-tasks: legacy live store found; create a verified backup and run noise tasks migrate plan\n"
	runs := map[string]func() (int, string, string){
		"list":       func() (int, string, string) { return runCmd(cmdList, "--json") },
		"next":       func() (int, string, string) { return runCmd(cmdNext) },
		"show":       func() (int, string, string) { return runCmd(cmdShow, "1") },
		"add":        func() (int, string, string) { return runCmd(cmdAdd, "--title", "x") },
		"set-status": func() (int, string, string) { return runCmd(cmdSetStatus, "--revision", "1", "1", "done") },
		"append":     func() (int, string, string) { return runCmd(cmdAppend, "--revision", "1", "1", "x") },
		"remove":     func() (int, string, string) { return runCmd(cmdRemove, "--revision", "1", "1") },
		"verify":     func() (int, string, string) { return runCmd(verifyMain) },
		"claim": func() (int, string, string) {
			return runCmd(cmdClaim, "--owner", "a", "--request-id", "req-refusal-1", "1")
		},
		"release":   func() (int, string, string) { return runCmd(cmdRelease, "--owner", "a", "1") },
		"milestone": func() (int, string, string) { return runCmd(cmdMilestone, "list") },
		"worktree":  func() (int, string, string) { return runCmd(cmdWorktree, "list") },
		"live feed": func() (int, string, string) { return runCmd(cmdLiveFeed) },
	}
	// The migration window is closed: live feed no longer accepts a
	// legacy-only family; it gets the same refusal as every other command.
	for name, run := range runs {
		code, stdout, stderr := run()
		if code != 1 || stdout != "" || stderr != want {
			t.Errorf("%s = code %d stdout %q stderr %q", name, code, stdout, stderr)
		}
	}
}

// TestFileFixtureNeverTouchesStore pins §8.1: --file operations are
// standalone and neither require nor create the project database.
func TestFileFixtureNeverTouchesStore(t *testing.T) {
	dir := chdirGitDir(t)
	path := backlogFile(t, "main", taskLit("1", "fixture", "pending", ""))

	if code, _, stderr := runCmd(cmdAdd, "--file", path, "--title", "two"); code != 0 {
		t.Fatalf("fixture add exit %d, stderr: %s", code, stderr)
	}
	if code, _, stderr := runCmd(cmdList, "--file", path, "--json"); code != 0 {
		t.Fatalf("fixture list exit %d, stderr: %s", code, stderr)
	}
	if code, _, stderr := runCmd(cmdSetStatus, "--file", path, "1", "done"); code != 0 {
		t.Fatalf("fixture set-status exit %d, stderr: %s", code, stderr)
	}
	if _, err := os.Lstat(projectStorePath(dir)); !os.IsNotExist(err) {
		t.Fatalf("fixture operations created the project store: %v", err)
	}
}
