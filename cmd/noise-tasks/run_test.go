package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

func claimTokenForRun(t *testing.T) int64 {
	t.Helper()
	code, stdout, stderr := runCmd(cmdClaim, "--json", "--owner", "agent-a", "--request-id", "claim.run", "1")
	if code != 0 {
		t.Fatalf("claim exit %d: %s", code, stderr)
	}
	var out struct {
		Token int64 `json:"token"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || out.Token <= 0 {
		t.Fatalf("claim output = %q, %v", stdout, err)
	}
	return out.Token
}

func TestRunStartCLIOutputReplayAndConflict(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "run")
	token := claimTokenForRun(t)
	args := []string{"1", "--json", "--owner", "agent-a", "--claim-token", fmt.Sprint(token), "--request-id", "run.cli", "--ttl", "300"}
	code, stdout, stderr := runCmd(cmdRunStart, args...)
	if code != 0 || stderr != "" {
		t.Fatalf("start exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(stdout), &first); err != nil {
		t.Fatal(err)
	}
	if len(first) != 5 || first["attempt"] != float64(1) || first["leaseGeneration"] != float64(1) || first["attemptToken"].(float64) <= 0 {
		t.Fatalf("start output = %v", first)
	}
	replayArgs := []string{"--json", "--owner", "agent-a", "--claim-token", fmt.Sprint(token + 99), "--request-id", "run.cli", "--ttl", "300", "1"}
	code, replay, stderr := runCmd(cmdRunStart, replayArgs...)
	if code != 0 || stderr != "" || replay != stdout {
		t.Fatalf("replay exit %d stdout=%q stderr=%q; want %q", code, replay, stderr, stdout)
	}
	code, stdout, stderr = runCmd(cmdRunStart, "--json", "--owner", "agent-a", "--claim-token", fmt.Sprint(token), "--request-id", "run.other", "1")
	if code != 3 || stdout != "" || strings.Contains(stderr, "token") || strings.Contains(stderr, "owner") || strings.Contains(stderr, "expiry") || strings.Contains(stderr, "status") {
		t.Fatalf("conflict exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var conflict map[string]any
	if err := json.Unmarshal([]byte(stderr), &conflict); err != nil || conflict["kind"] != "run-conflict" || conflict["task"] != "1" {
		t.Fatalf("conflict = %v, %v", conflict, err)
	}
}

func TestRunStartCLILocalUsage(t *testing.T) {
	chdirGitDir(t)
	cases := map[string][]string{
		"missing task":  {"--owner", "a", "--claim-token", "1", "--request-id", "r"},
		"missing owner": {"1", "--claim-token", "1", "--request-id", "r"},
		"bad owner":     {"1", "--owner", " bad", "--claim-token", "1", "--request-id", "r"},
		"bad token":     {"1", "--owner", "a", "--claim-token", "0", "--request-id", "r"},
		"missing req":   {"1", "--owner", "a", "--claim-token", "1"},
		"bad req":       {"1", "--owner", "a", "--claim-token", "1", "--request-id", "-bad"},
		"bad ttl":       {"1", "--owner", "a", "--claim-token", "1", "--request-id", "r", "--ttl", "86401"},
		"zero ttl":      {"1", "--owner", "a", "--claim-token", "1", "--request-id", "r", "--ttl", "0"},
		"bad task":      {"nope", "--owner", "a", "--claim-token", "1", "--request-id", "r"},
		"subtask":       {"1.1", "--owner", "a", "--claim-token", "1", "--request-id", "r"},
		"fixture":       {"1", "--owner", "a", "--claim-token", "1", "--request-id", "r", "--file", "fixture.json"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, stdout, _ := runCmd(cmdRunStart, args...)
			if code != 2 || stdout != "" {
				t.Fatalf("exit %d stdout=%q", code, stdout)
			}
		})
	}
	if code, stdout, stderr := runCmd(cmdRun); code != 2 || stdout != "" || !strings.Contains(stderr, "retry") {
		t.Fatalf("empty run = exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if code, stdout, _ := runCmd(cmdRun, "wat"); code != 2 || stdout != "" {
		t.Fatalf("unknown run = exit %d stdout=%q", code, stdout)
	}
}

type runStartCLIOut struct {
	RunID           int64  `json:"runId"`
	Attempt         int64  `json:"attempt"`
	LeaseGeneration int64  `json:"leaseGeneration"`
	AttemptToken    int64  `json:"attemptToken"`
	ExpiresAt       string `json:"expiresAt"`
}

func startRunCLI(t *testing.T, claimToken int64, requestID string) runStartCLIOut {
	t.Helper()
	code, stdout, stderr := runCmd(cmdRun, "start", "1", "--json", "--owner", "agent-a",
		"--claim-token", fmt.Sprint(claimToken), "--request-id", requestID, "--ttl", "300")
	if code != 0 || stderr != "" {
		t.Fatalf("run start exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var out runStartCLIOut
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || out.RunID <= 0 || out.AttemptToken <= 0 {
		t.Fatalf("run start output = %q, %v", stdout, err)
	}
	return out
}

func TestRunCLIRenewFinishRetryCancelAndReads(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "run lifecycle")
	claimToken := claimTokenForRun(t)
	start := startRunCLI(t, claimToken, "run.lifecycle")

	code, stdout, stderr := runCmd(cmdRun, "renew", fmt.Sprint(start.RunID), "--json", "--owner", "agent-a",
		"--claim-token", fmt.Sprint(claimToken), "--attempt", "1", "--attempt-token", fmt.Sprint(start.AttemptToken),
		"--request-id", "renew.lifecycle", "--ttl", "600")
	if code != 0 || stderr != "" {
		t.Fatalf("renew exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var renewed struct {
		RunID           int64 `json:"runId"`
		Attempt         int64 `json:"attempt"`
		LeaseGeneration int64 `json:"leaseGeneration"`
		ClaimToken      int64 `json:"claimToken"`
		AttemptToken    int64 `json:"attemptToken"`
	}
	if err := json.Unmarshal([]byte(stdout), &renewed); err != nil || renewed.LeaseGeneration != 2 ||
		renewed.ClaimToken <= claimToken || renewed.AttemptToken <= start.AttemptToken {
		t.Fatalf("renew output = %q, %v", stdout, err)
	}

	code, stdout, stderr = runCmd(cmdRun, "finish", fmt.Sprint(start.RunID), "--json", "--owner", "agent-a",
		"--claim-token", fmt.Sprint(renewed.ClaimToken), "--attempt", "1", "--attempt-token", fmt.Sprint(renewed.AttemptToken),
		"--outcome", "failed", "--request-id", "finish.lifecycle.failed")
	if code != 0 || stderr != "" || !strings.Contains(stdout, `"runStatus": "retryable"`) {
		t.Fatalf("failed finish exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}

	code, stdout, stderr = runCmd(cmdRun, "retry", fmt.Sprint(start.RunID), "--json", "--owner", "agent-a",
		"--claim-token", fmt.Sprint(renewed.ClaimToken), "--attempt", "1", "--attempt-token", fmt.Sprint(renewed.AttemptToken),
		"--request-id", "retry.lifecycle", "--ttl", "300")
	if code != 0 || stderr != "" {
		t.Fatalf("retry exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var retry runStartCLIOut
	if err := json.Unmarshal([]byte(stdout), &retry); err != nil || retry.Attempt != 2 || retry.AttemptToken <= 0 {
		t.Fatalf("retry output = %q, %v", stdout, err)
	}

	code, stdout, stderr = runCmd(cmdRun, "show", fmt.Sprint(start.RunID), "--json")
	if code != 0 || stderr != "" || strings.Contains(stdout, "attemptToken") || !strings.Contains(stdout, `"receipts"`) {
		t.Fatalf("show exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, stderr = runCmd(cmdRun, "show", fmt.Sprint(start.RunID))
	if code != 0 || stderr != "" || strings.Contains(stdout, "attemptToken") ||
		!strings.Contains(stdout, "terminalAt=null terminalResultDigest=null attempts=[{") ||
		!strings.Contains(stdout, `receipts=[{"operation":"start"`) {
		t.Fatalf("human show exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	wantOrder := []string{"runId=", " tag=", " task=", " owner=", " status=", " currentAttempt=", " startedAt=", " updatedAt=", " terminalAt=", " terminalResultDigest=", " attempts=", " receipts="}
	last := -1
	for _, field := range wantOrder {
		at := strings.Index(stdout, field)
		if at <= last {
			t.Fatalf("human show field %q out of order in %q", field, stdout)
		}
		last = at
	}
	code, stdout, stderr = runCmd(cmdRun, "list", "--json", "--task", "1", "--status", "running", "--owner", "agent-a")
	if code != 0 || stderr != "" || strings.Contains(stdout, "Token") || !strings.Contains(stdout, `"runId"`) {
		t.Fatalf("list exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}

	code, stdout, stderr = runCmd(cmdRun, "cancel", fmt.Sprint(start.RunID), "--json", "--owner", "agent-a",
		"--claim-token", fmt.Sprint(renewed.ClaimToken), "--attempt", "2", "--attempt-token", fmt.Sprint(retry.AttemptToken),
		"--request-id", "cancel.lifecycle")
	if code != 0 || stderr != "" || !strings.Contains(stdout, `"runStatus": "cancelled"`) || !strings.Contains(stdout, `"forced": false`) {
		t.Fatalf("cancel exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestRunCLIFinishSuccessAndForceCancel(t *testing.T) {
	t.Run("finish success with task publication", func(t *testing.T) {
		dir := chdirGitDir(t)
		seedProjectStore(t, dir, "main", "finish success")
		claimToken := claimTokenForRun(t)
		start := startRunCLI(t, claimToken, "run.finish-success")
		code, stdout, stderr := runCmd(cmdRun, "finish", fmt.Sprint(start.RunID), "--json", "--owner", "agent-a",
			"--claim-token", fmt.Sprint(claimToken), "--attempt", "1", "--attempt-token", fmt.Sprint(start.AttemptToken),
			"--outcome", "succeeded", "--request-id", "finish.success", "--task-status", "done", "--revision", "1")
		if code != 0 || stderr != "" || !strings.Contains(stdout, `"runStatus": "succeeded"`) ||
			!strings.Contains(stdout, `"status": "done"`) || !strings.Contains(stdout, `"revision": 2`) {
			t.Fatalf("successful finish exit %d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})

	t.Run("force cancel", func(t *testing.T) {
		dir := chdirGitDir(t)
		seedProjectStore(t, dir, "main", "force cancel")
		claimToken := claimTokenForRun(t)
		start := startRunCLI(t, claimToken, "run.force-cancel")
		code, stdout, stderr := runCmd(cmdRun, "cancel", fmt.Sprint(start.RunID), "--json", "--force",
			"--request-id", "cancel.force")
		if code != 0 || stderr != "" || !strings.Contains(stdout, `"forced": true`) {
			t.Fatalf("force cancel exit %d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if code, stdout, _ := runCmd(cmdRun, "cancel", fmt.Sprint(start.RunID), "--force", "--owner", "agent-a",
			"--request-id", "cancel.bad"); code != 2 || stdout != "" {
			t.Fatalf("force capability mix exit %d stdout=%q", code, stdout)
		}
	})
}

func TestRunCLILocalGrammarForAllVerbs(t *testing.T) {
	chdirGitDir(t)
	cases := []struct {
		name string
		args []string
	}{
		{"retry missing capabilities", []string{"retry", "1", "--request-id", "r"}},
		{"renew bad ttl", []string{"renew", "1", "--owner", "a", "--claim-token", "1", "--attempt", "1", "--attempt-token", "2", "--request-id", "r", "--ttl", "0"}},
		{"finish bad outcome", []string{"finish", "1", "--owner", "a", "--claim-token", "1", "--attempt", "1", "--attempt-token", "2", "--request-id", "r", "--outcome", "maybe"}},
		{"finish partial task publication", []string{"finish", "1", "--owner", "a", "--claim-token", "1", "--attempt", "1", "--attempt-token", "2", "--request-id", "r", "--outcome", "succeeded", "--task-status", "done"}},
		{"cancel missing force or capabilities", []string{"cancel", "1", "--request-id", "r"}},
		{"show fixture", []string{"show", "1", "--file", "fixture.json"}},
		{"list subtask", []string{"list", "--task", "1.2"}},
		{"list bad status", []string{"list", "--status", "pending"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, _ := runCmd(cmdRun, tt.args...)
			if code != 2 || stdout != "" {
				t.Fatalf("exit %d stdout=%q", code, stdout)
			}
		})
	}
}

func TestRunCLIHelpAndStrictDecimalGrammar(t *testing.T) {
	chdirGitDir(t)
	if code, stdout, stderr := runCmd(cmdRun, "--help"); code != 0 || stderr != "" || !strings.Contains(stdout, "run <start|retry") {
		t.Fatalf("run help exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, tt := range []struct {
		verb    string
		wantTag bool
	}{
		{"start", true}, {"retry", false}, {"renew", false}, {"finish", false},
		{"cancel", false}, {"show", false}, {"list", false},
	} {
		code, stdout, stderr := runCmd(cmdRun, tt.verb, "-help")
		if code != 0 || stderr != "" || !strings.Contains(stdout, "usage: noise-tasks run "+tt.verb) || strings.Contains(stdout, "\n  -file ") || strings.Contains(stdout, "-tag") != tt.wantTag {
			t.Errorf("%s help exit %d stdout=%q stderr=%q", tt.verb, code, stdout, stderr)
		}
	}

	cases := []struct {
		name string
		args []string
	}{
		{"start hexadecimal claim token", []string{"start", "1", "--owner", "a", "--claim-token", "0x1", "--request-id", "r"}},
		{"retry underscored attempt", []string{"retry", "1", "--owner", "a", "--claim-token", "1", "--attempt", "1_0", "--attempt-token", "2", "--request-id", "r"}},
		{"finish hexadecimal revision", []string{"finish", "1", "--owner", "a", "--claim-token", "1", "--attempt", "1", "--attempt-token", "2", "--request-id", "r", "--outcome", "succeeded", "--task-status", "done", "--revision", "0x1"}},
		{"hexadecimal run id", []string{"show", "0x1"}},
		{"empty fixture spelling", []string{"show", "1", "--file="}},
		{"tag on non-start verb", []string{"show", "1", "--tag", "main"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, _ := runCmd(cmdRun, tt.args...)
			if code != 2 || stdout != "" {
				t.Fatalf("exit %d stdout=%q", code, stdout)
			}
		})
	}
	var ttl int
	var parseErr strings.Builder
	fs := newCommandFlagSet("decimal", &parseErr)
	decimalIntVar(fs, &ttl, "ttl", "")
	if code, ok := parseFlags(fs, io.Discard, []string{"--ttl", "077"}); !ok || code != 0 || ttl != 77 {
		t.Fatalf("base-10 leading-zero TTL parsed as %d, exit %d, stderr=%q", ttl, code, parseErr.String())
	}
}
