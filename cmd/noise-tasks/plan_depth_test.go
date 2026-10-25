package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanValidateNestingLimitSubprocess(t *testing.T) {
	for _, container := range []struct {
		name        string
		open, close string
	}{
		{name: "array", open: "[", close: "]"},
		{name: "object", open: `{"x":`, close: "}"},
	} {
		t.Run(container.name, func(t *testing.T) {
			raw := strings.Repeat(container.open, 10001) + "null" + strings.Repeat(container.close, 10001)
			path := filepath.Join(t.TempDir(), "plan.json")
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(os.Args[0], "-test.run=^TestPlanValidateCLIHelperProcess$", "--", "plan", "validate", "--offline", "--json", path)
			cmd.Env = append(os.Environ(), "NOISE_TASKS_TEST_CLI_HELPER=1")
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			exitErr, ok := err.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 1 {
				t.Fatalf("exit error = %v, want normal exit 1; stderr: %s", err, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			diagnostic := stderr.String()
			if !strings.Contains(diagnostic, "noise-tasks:") || !strings.Contains(diagnostic, "nesting depth exceeds 10000") {
				t.Fatalf("stderr = %q, want nesting-depth diagnostic", diagnostic)
			}
			if strings.Contains(diagnostic, "fatal error") || strings.Contains(diagnostic, "stack overflow") {
				t.Fatalf("stderr contains a runtime crash: %q", diagnostic)
			}
		})
	}
}

func TestPlanValidateCLIHelperProcess(t *testing.T) {
	if os.Getenv("NOISE_TASKS_TEST_CLI_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"noise-tasks"}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
	os.Exit(2)
}
