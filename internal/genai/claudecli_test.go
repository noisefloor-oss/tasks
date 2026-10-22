package genai

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// No test here runs a real claude CLI: the driver is exercised only
// against stub shell scripts written into a temp dir.

// stubClaude writes an executable shell script and returns its path.
func stubClaude(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-stub")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// recordingStubClaude writes a stub that records its argv and stdin
// into dir and replies with the contents of dir/reply.
func recordingStubClaude(t *testing.T, reply string) (exe, dir string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "reply"), []byte(reply), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `printf '<%s>\n' "$@" > "` + dir + `/args"
pwd > "` + dir + `/cwd"
cat > "` + dir + `/stdin"
cat "` + dir + `/reply"
`
	return stubClaude(t, script), dir
}

func TestNewClaudeCLIConfiguredIgnoresSelectorEnvironment(t *testing.T) {
	stub := stubClaude(t, "exit 0")
	t.Setenv("NOISE_TASKS_MODEL", "ambient-model")
	t.Setenv("NOISETASK_MODEL", "conflicting-model")
	t.Setenv("NOISE_TASKS_CLAUDE_EXECUTABLE", filepath.Join(t.TempDir(), "wrong"))
	cli, err := NewClaudeCLIConfigured(stub, "configured-model", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cli.Model != "configured-model" || cli.Executable != stub || cli.Timeout != defaultCLITimeout {
		t.Fatalf("configured CLI = %+v", cli)
	}
}

func TestNewClaudeCLIConfiguredRefusesMissingExecutable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-claude")
	_, err := NewClaudeCLIConfigured(missing, "configured-model", t.TempDir())
	if err == nil {
		t.Fatal("configured Claude adapter with a missing executable should refuse")
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("error = %v", err)
	}
}

func TestClaudeCLIGenerateTasksHappyPath(t *testing.T) {
	// Markdown fences around the JSON must be tolerated, same as the
	// API driver.
	exe, dir := recordingStubClaude(t, "```json\n"+validDraftsJSON+"\n```")
	c := &ClaudeCLI{Executable: exe, Model: "claude-test", WorkingDirectory: dir, Timeout: time.Minute}

	result, err := c.GenerateTasks(context.Background(), "the PRD text", 3)
	if err != nil {
		t.Fatalf("GenerateTasks: %v", err)
	}
	drafts := result.Drafts

	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := "<-p>\n<--output-format>\n<text>\n<--model>\n<claude-test>\n<--no-session-persistence>\n<--safe-mode>\n<--tools>\n<>"
	if got := strings.TrimSpace(string(args)); got != wantArgs {
		t.Errorf("argv = %q, want %q", got, wantArgs)
	}
	cwd, err := os.ReadFile(filepath.Join(dir, "cwd"))
	if err != nil || strings.TrimSpace(string(cwd)) != dir {
		t.Errorf("cwd = %q, %v; want %q", cwd, err, dir)
	}

	stdin, err := os.ReadFile(filepath.Join(dir, "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(stdin)
	if !strings.Contains(prompt, "STRICT JSON") {
		t.Errorf("stdin prompt should demand strict JSON: %q", prompt)
	}
	if !strings.Contains(prompt, "at most 3 tasks") {
		t.Errorf("stdin prompt should carry the requested count: %q", prompt)
	}
	if !strings.Contains(prompt, "the PRD text") {
		t.Errorf("stdin prompt should carry the PRD: %q", prompt)
	}

	if len(drafts) != 3 {
		t.Fatalf("got %d drafts, want 3", len(drafts))
	}
	if drafts[0].Title != "Set up scaffolding" || drafts[0].Priority != "high" {
		t.Errorf("draft 1 = %+v", drafts[0])
	}
	if len(drafts[2].Dependencies) != 2 || drafts[2].Dependencies[0] != 1 || drafts[2].Dependencies[1] != 2 {
		t.Errorf("draft 3 dependencies = %v, want [1 2]", drafts[2].Dependencies)
	}
}

func TestClaudeCLIGenerateTasksCapsSurplusDrafts(t *testing.T) {
	exe, _ := recordingStubClaude(t, validDraftsJSON)
	c := &ClaudeCLI{Executable: exe, Model: "claude-test", Timeout: time.Minute}
	result, err := c.GenerateTasks(context.Background(), "prd", 2)
	if err != nil {
		t.Fatalf("GenerateTasks: %v", err)
	}
	drafts := result.Drafts
	if len(drafts) != 2 {
		t.Errorf("got %d drafts, want the requested cap of 2", len(drafts))
	}
}

func TestClaudeCLIGenerateTasksRejectsGarbage(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		want  string
	}{
		{"prose instead of JSON", "Here are some great tasks for you!", "JSON"},
		{"broken JSON array", `[{"title": "a", `, "JSON"},
		{"unknown fields", `[{"title": "a", "id": "7"}]`, "JSON"},
		{"empty array", `[]`, "no actionable"},
		{"empty title", `[{"title": "  "}]`, "empty title"},
		{"self dep", `[{"title": "a", "dependencies": [1]}]`, "depends on itself"},
	}
	for _, tc := range cases {
		exe, _ := recordingStubClaude(t, tc.reply)
		c := &ClaudeCLI{Executable: exe, Model: "claude-test", Timeout: time.Minute}
		_, err := c.GenerateTasks(context.Background(), "prd", 5)
		if err == nil {
			t.Errorf("%s: want an error, got none", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %q, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestClaudeCLIGenerateTasksSanitizesStderrOnFailure(t *testing.T) {
	exe := stubClaude(t, `echo "please run /login first" >&2
exit 3`)
	c := &ClaudeCLI{Executable: exe, Model: "claude-test", Timeout: time.Minute}
	_, err := c.GenerateTasks(context.Background(), "prd", 5)
	if err == nil {
		t.Fatal("nonzero exit should be an error")
	}
	if strings.Contains(err.Error(), "please run /login first") {
		t.Errorf("error leaked CLI stderr: %v", err)
	}
	if !strings.Contains(err.Error(), "exit code 3") {
		t.Errorf("error should carry the exit status: %v", err)
	}
}

func TestClaudeCLIGenerateTasksNonzeroExitWithoutStderr(t *testing.T) {
	exe := stubClaude(t, "exit 1")
	c := &ClaudeCLI{Executable: exe, Model: "claude-test", Timeout: time.Minute}
	_, err := c.GenerateTasks(context.Background(), "prd", 5)
	if err == nil || !strings.Contains(err.Error(), "exit code 1") {
		t.Errorf("silent failure should still report the exit status, got: %v", err)
	}
}

func TestClaudeCLIGenerateTasksTimesOut(t *testing.T) {
	exe := stubClaude(t, "exec sleep 30")
	c := &ClaudeCLI{Executable: exe, Model: "claude-test", Timeout: 100 * time.Millisecond}
	start := time.Now()
	_, err := c.GenerateTasks(context.Background(), "prd", 5)
	if err == nil {
		t.Fatal("a hung CLI should time out")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout took %v, the context should have killed the process", elapsed)
	}
}
