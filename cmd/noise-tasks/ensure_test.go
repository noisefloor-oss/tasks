package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/genai"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// seedTag registers one extra tag in the existing project store.
func seedTag(t *testing.T, dir, tag string) {
	t.Helper()
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.EnsureTag(tag); err != nil {
		t.Fatal(err)
	}
}

func TestCLIEnsureCreateThenReplay(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "seed")

	code, stdout, stderr := runCmd(cmdEnsure, "--repo", "acme/widgets", "--issue", "214",
		"--title", "Fix crash on import", "--description", "Importing an empty file crashes.")
	if code != 0 || stdout != "created task 2 for github/acme/widgets#214\n" {
		t.Fatalf("create exit %d stdout %q stderr %q", code, stdout, stderr)
	}

	// Replay with different caller text: same task, no second write.
	code, stdout, stderr = runCmd(cmdEnsure, "--repo", "acme/widgets", "--issue", "214",
		"--title", "importer replay title")
	if code != 0 || stdout != "task 2 already tracks github/acme/widgets#214\n" {
		t.Fatalf("replay exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// No counter bump: the next plain add draws id 3.
	code, stdout, stderr = runCmd(cmdAdd, "--title", "unrelated")
	if code != 0 || strings.TrimSpace(stdout) != "3" {
		t.Fatalf("post-replay add exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// The stored task keeps the first caller's text and the link.
	code, stdout, _ = runCmd(cmdShow, "--json", "2")
	if code != 0 || !strings.Contains(stdout, `"title": "Fix crash on import"`) ||
		!strings.Contains(stdout, `"repository": "acme/widgets"`) {
		t.Fatalf("replayed task show = %q", stdout)
	}
}

func TestCLIEnsureJSONShape(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main")

	code, stdout, stderr := runCmd(cmdEnsure, "--json", "--repo", "acme/widgets", "--issue", "7",
		"--title", "First issue")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, stderr)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if out["id"] != "1" || out["tag"] != "main" || out["created"] != true {
		t.Fatalf("create json = %v", out)
	}

	code, stdout, stderr = runCmd(cmdEnsure, "--json", "--repo", "acme/widgets", "--issue", "7",
		"--title", "different caller")
	if code != 0 {
		t.Fatalf("replay exit %d: %s", code, stderr)
	}
	out = nil
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if out["id"] != "1" || out["tag"] != "main" || out["created"] != false {
		t.Fatalf("replay json = %v", out)
	}
}

func TestCLIEnsureCrossTagHolderConflict(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main")
	seedTag(t, dir, "import")

	code, _, stderr := runCmd(cmdEnsure, "--tag", "import", "--repo", "acme/widgets", "--issue", "214",
		"--title", "importer task")
	if code != 0 {
		t.Fatalf("import ensure exit %d: %s", code, stderr)
	}
	code, stdout, stderr := runCmd(cmdEnsure, "--repo", "acme/widgets", "--issue", "214",
		"--title", "agent task")
	if code != 3 || stdout != "" || !strings.Contains(stderr, "import") {
		t.Fatalf("cross-tag exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// The refusal wrote nothing to the default tag.
	code, stdout, _ = runCmd(cmdList, "--json")
	if code != 0 || !strings.Contains(stdout, `"total": 0`) {
		t.Fatalf("main tag after refusal = %q", stdout)
	}
}

func TestCLIEnsureFamilyFlag(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main")

	code, stdout, stderr := runCmd(cmdEnsure, "--family", "gitlab", "--repo", "acme/tools",
		"--issue", "9", "--title", "Gitlab issue")
	if code != 0 || !strings.Contains(stdout, "gitlab/acme/tools#9") {
		t.Fatalf("family ensure exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// The default family is github; a different family is a different
	// triple, so it creates a second task rather than replaying.
	code, stdout, stderr = runCmd(cmdEnsure, "--repo", "acme/tools", "--issue", "9", "--title", "same issue, other family")
	if code != 0 || !strings.Contains(stdout, "created task 2 for github/acme/tools#9") {
		t.Fatalf("default-family ensure exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestCLIEnsureUntrustedTextRefusals(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main")

	long := strings.Repeat("x", 201)
	longDesc := strings.Repeat("d", 501)
	for _, args := range [][]string{
		{"--repo", "acme/widgets", "--issue", "1", "--title", "two\nlines"},
		{"--repo", "acme/widgets", "--issue", "1", "--title", long},
		{"--repo", "acme/widgets", "--issue", "1", "--title", "ok", "--description", "two\nlines"},
		{"--repo", "acme/widgets", "--issue", "1", "--title", "ok", "--description", longDesc},
		{"--repo", "acme/widgets", "--issue", "1"},                   // --title required
		{"--repo", "acme/widgets", "--issue", "1", "--title", "   "}, // whitespace-only title
	} {
		code, stdout, stderr := runCmd(cmdEnsure, args...)
		if code != 2 || stdout != "" {
			t.Fatalf("text refusal %v exit %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
	// The refusals share the generate contract's exact diagnostics.
	if err := genai.ValidateTitle("two\nlines"); err == nil || err.Error() != "title contains a newline" {
		t.Fatalf("genai title parity: %v", err)
	}
	if err := genai.ValidateDescription(longDesc); err == nil || err.Error() != "description exceeds 500 bytes" {
		t.Fatalf("genai description parity: %v", err)
	}
	// Nothing was written by any refusal.
	code, stdout, _ := runCmd(cmdList, "--json")
	if code != 0 || !strings.Contains(stdout, `"total": 0`) {
		t.Fatalf("tag after refusals = %q", stdout)
	}
}

func TestCLIEnsureGrammarRefusals(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main")

	for _, args := range [][]string{
		{"--repo", "acme/widgets", "--issue", "0", "--title", "zero"},
		{"--repo", "acme/widgets", "--issue", "01", "--title", "leading zero"},
		{"--repo", "acme/widgets", "--issue", "x", "--title", "not decimal"},
		{"--repo", "acme/widgets", "--issue", "", "--title", "missing issue"},
		{"--repo", "acme widgets", "--issue", "1", "--title", "repo whitespace"},
		{"--repo", "acme/widgets#2", "--issue", "1", "--title", "repo hash"},
		{"--repo", "", "--issue", "1", "--title", "missing repo"},
		{"--repo", "acme/widgets", "--issue", "1", "--title", "ok", "--family", "-bad"},
	} {
		code, stdout, stderr := runCmd(cmdEnsure, args...)
		if code != 2 || stdout != "" {
			t.Fatalf("grammar refusal %v exit %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
}

func TestCLIEnsureFixtureSurface(t *testing.T) {
	dir := chdirGitDir(t)
	path := dir + "/fixture.json"
	if code, _, _ := runCmd(cmdInit, "--file", path); code != 0 {
		t.Fatal("fixture init failed")
	}
	code, stdout, stderr := runCmd(cmdEnsure, "--file", path, "--repo", "acme/widgets",
		"--issue", "5", "--title", "fixture issue")
	if code != 0 || stdout != "created task 1 for github/acme/widgets#5\n" {
		t.Fatalf("fixture create exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	code, stdout, stderr = runCmd(cmdEnsure, "--file", path, "--repo", "acme/widgets",
		"--issue", "5", "--title", "replay")
	if code != 0 || stdout != "task 1 already tracks github/acme/widgets#5\n" {
		t.Fatalf("fixture replay exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// A second distinct issue draws id 2 — the replay consumed nothing.
	code, stdout, stderr = runCmd(cmdEnsure, "--file", path, "--repo", "acme/widgets",
		"--issue", "6", "--title", "second")
	if code != 0 || !strings.Contains(stdout, "created task 2 ") {
		t.Fatalf("fixture second create exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}
