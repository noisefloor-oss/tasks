package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// claimToFile claims task 1 and writes the fencing token to a private
// regular file, returning the path.
func claimToFile(t *testing.T, requestID string) string {
	t.Helper()
	code, stdout, stderr := runCmd(cmdClaim, "--json", "--owner", "file-probe",
		"--request-id", requestID, "1")
	if code != 0 || stderr != "" {
		t.Fatalf("claim = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	var out struct {
		Token int64 `json:"token"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || out.Token <= 0 {
		t.Fatalf("claim output = %q, %v", stdout, err)
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", out.Token)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReleaseAcceptsTokenFile(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "token file release")
	tokenFile := claimToFile(t, "tokenfile.release")

	code, stdout, stderr := runCmd(cmdRelease, "--json", "--owner", "file-probe",
		"--token-file", tokenFile, "1")
	if code != 0 || stdout == "" || stderr != "" {
		t.Fatalf("release --token-file = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestRunLifecycleAcceptsTokenFiles(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "token file run")
	claimToken := claimTokenForRun(t)
	claimFile := filepath.Join(t.TempDir(), "claim")
	if err := os.WriteFile(claimFile, []byte(fmt.Sprintf("%d\n", claimToken)), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCmd(cmdRun, "start", "1", "--json", "--owner", "agent-a",
		"--claim-token-file", claimFile, "--request-id", "tokenfile.run.start", "--ttl", "300")
	if code != 0 || stderr != "" {
		t.Fatalf("run start --claim-token-file = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	var start runStartCLIOut
	if err := json.Unmarshal([]byte(stdout), &start); err != nil || start.RunID <= 0 {
		t.Fatalf("run start output = %q, %v", stdout, err)
	}

	attemptFile := filepath.Join(t.TempDir(), "attempt")
	if err := os.WriteFile(attemptFile, []byte(fmt.Sprintf("  %d  ", start.AttemptToken)), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runCmd(cmdRun, "finish", fmt.Sprint(start.RunID), "--json", "--owner", "agent-a",
		"--claim-token-file", claimFile, "--attempt", fmt.Sprint(start.Attempt),
		"--attempt-token-file", attemptFile, "--outcome", "succeeded",
		"--request-id", "tokenfile.run.finish")
	if code != 0 || stderr != "" || !containsJSONField(stdout, "runStatus") {
		t.Fatalf("run finish via token files = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func containsJSONField(stdout, field string) bool {
	var obj map[string]any
	return json.Unmarshal([]byte(stdout), &obj) == nil && obj[field] != nil
}

func TestTokenFileFlagRefusals(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "token file refusals")
	tokenFile := claimToFile(t, "tokenfile.refusals")

	bad := t.TempDir()
	garbage := filepath.Join(bad, "garbage")
	if err := os.WriteFile(garbage, []byte("not-a-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(bad, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(bad, "symlink")
	if err := os.Symlink(tokenFile, symlink); err != nil {
		t.Fatal(err)
	}
	oversize := filepath.Join(bad, "oversize")
	payload := append([]byte("1234567890"), make([]byte, 100)...)
	if err := os.WriteFile(oversize, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"both forms", []string{"--json", "--owner", "file-probe", "--token", "1", "--token-file", tokenFile, "1"}},
		{"garbage content", []string{"--json", "--owner", "file-probe", "--token-file", garbage, "1"}},
		{"empty file", []string{"--json", "--owner", "file-probe", "--token-file", empty, "1"}},
		{"symlink", []string{"--json", "--owner", "file-probe", "--token-file", symlink, "1"}},
		{"missing file", []string{"--json", "--owner", "file-probe", "--token-file", filepath.Join(bad, "absent"), "1"}},
		{"oversize file", []string{"--json", "--owner", "file-probe", "--token-file", oversize, "1"}},
	}
	for _, tc := range cases {
		code, stdout, stderr := runCmd(cmdRelease, tc.args...)
		if code != 2 || stdout != "" || stderr == "" {
			t.Errorf("%s = code %d stdout %q stderr %q", tc.name, code, stdout, stderr)
		}
	}
}
