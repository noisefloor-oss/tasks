package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func taskRunGateArgs(claimToken int64, run runStartCLIOut) []string {
	return []string{"--owner", "agent-a", "--token", fmt.Sprint(claimToken), "--run", fmt.Sprint(run.RunID),
		"--attempt", fmt.Sprint(run.Attempt), "--attempt-token", fmt.Sprint(run.AttemptToken)}
}

func TestTaskMutationCLIAcceptsExactRunGateAndRedactsConflicts(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "run-gated task")
	claimToken := claimTokenForRun(t)
	run := startRunCLI(t, claimToken, "run.task-mutations")

	code, stdout, stderr := runCmd(cmdAppend, "--json", "--revision", "1", "1", "ungated")
	if code != 3 || stdout != "" || strings.Contains(stderr, "token") || strings.Contains(stderr, "owner") ||
		strings.Contains(stderr, "expiry") || !strings.Contains(stderr, `"kind": "run-conflict"`) {
		t.Fatalf("ungated append exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}

	gate := taskRunGateArgs(claimToken, run)
	appendArgs := append([]string{"--json", "--revision", "1"}, gate...)
	appendArgs = append(appendArgs, "1", "published")
	if code, stdout, stderr = runCmd(cmdAppend, appendArgs...); code != 0 || stderr != "" || !strings.Contains(stdout, `"id": "1"`) {
		t.Fatalf("gated append exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if code, _, stderr = runCmd(cmdMilestoneAdd, "M1"); code != 0 {
		t.Fatalf("milestone add exit %d: %s", code, stderr)
	}
	milestoneArgs := append([]string{"--json", "--revision", "2"}, gate...)
	milestoneArgs = append(milestoneArgs, "1", "M1")
	if code, stdout, stderr = runCmd(cmdSetMilestone, milestoneArgs...); code != 0 || stderr != "" ||
		!strings.Contains(stdout, `"milestone": "M1"`) {
		t.Fatalf("gated milestone exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	statusArgs := append([]string{"--json", "--revision", "3"}, gate...)
	statusArgs = append(statusArgs, "1", "review")
	if code, stdout, stderr = runCmd(cmdSetStatus, statusArgs...); code != 0 || stderr != "" ||
		!strings.Contains(stdout, `"status": "review"`) {
		t.Fatalf("gated status exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}

	removeArgs := append([]string{"--json", "--revision", "4", "--force"}, gate...)
	removeArgs = append(removeArgs, "1")
	code, stdout, stderr = runCmd(cmdRemove, removeArgs...)
	if code != 3 || stdout != "" || strings.Contains(stderr, "token") || strings.Contains(stderr, "owner") {
		t.Fatalf("run-history removal exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var conflict map[string]any
	if err := json.Unmarshal([]byte(stderr), &conflict); err != nil || conflict["kind"] != "run-conflict" ||
		conflict["operation"] != "remove" || conflict["task"] != "1" {
		t.Fatalf("run-history conflict = %v, %v", conflict, err)
	}
}

func TestTaskMutationCLIRunGateGrammarAndFixtureRefusal(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "grammar")
	cases := []struct {
		name string
		run  func(...string) (int, string, string)
		args []string
	}{
		{"partial claim", func(args ...string) (int, string, string) { return runCmd(cmdAppend, args...) },
			[]string{"--revision", "1", "--owner", "agent-a", "1", "note"}},
		{"partial run", func(args ...string) (int, string, string) { return runCmd(cmdSetStatus, args...) },
			[]string{"--revision", "1", "--owner", "agent-a", "--token", "1", "--run", "1", "1", "review"}},
		{"zero run token", func(args ...string) (int, string, string) { return runCmd(cmdSetMilestone, args...) },
			[]string{"--revision", "1", "--owner", "agent-a", "--token", "1", "--run", "1", "--attempt", "1", "--attempt-token", "0", "1", "M1"}},
		{"all-zero run gate", func(args ...string) (int, string, string) { return runCmd(cmdAppend, args...) },
			[]string{"--revision", "1", "--owner", "agent-a", "--token", "1", "--run", "0", "--attempt", "0", "--attempt-token", "0", "1", "note"}},
		{"fixture explicit zeros", func(args ...string) (int, string, string) { return runCmd(cmdRemove, args...) },
			[]string{"--file", "missing.json", "--run", "0", "--attempt", "0", "--attempt-token", "0", "1"}},
		{"fixture empty path still refuses run gate", func(args ...string) (int, string, string) { return runCmd(cmdAppend, args...) },
			[]string{"--file=", "--run", "1", "--attempt", "1", "--attempt-token", "1", "1", "note"}},
		{"hexadecimal run id", func(args ...string) (int, string, string) { return runCmd(cmdSetStatus, args...) },
			[]string{"--revision", "1", "--owner", "agent-a", "--token", "1", "--run", "0x1", "--attempt", "1", "--attempt-token", "1", "1", "review"}},
		{"octal attempt", func(args ...string) (int, string, string) { return runCmd(cmdSetMilestone, args...) },
			[]string{"--revision", "1", "--owner", "agent-a", "--token", "1", "--run", "1", "--attempt", "077", "--attempt-token", "1", "1", "M1"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, _ := tt.run(tt.args...)
			if code != 2 || stdout != "" {
				t.Fatalf("exit %d stdout=%q", code, stdout)
			}
		})
	}
}
