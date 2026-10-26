package main

import (
	"bytes"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// helpCommandPaths enumerates the CLI command tree once: every top-level
// command and every group/subcommand pair. Every path must answer -h,
// -help, and --help with usage on stdout and exit 0 (SPEC §4). The
// coverage tests below pin this table to the live command listings, so a
// new command or subcommand that forgets help fails here.
var helpCommandPaths = [][]string{
	{"setup"},
	{"init"},
	{"list"},
	{"next"},
	{"show"},
	{"add"},
	{"ensure"},
	{"set-status"},
	{"set-milestone"},
	{"set-link"},
	{"append"},
	{"remove"},
	{"milestone"},
	{"milestone", "add"},
	{"milestone", "list"},
	{"milestone", "close"},
	{"milestone", "reopen"},
	{"milestone", "remove"},
	{"hygiene"},
	{"backup"},
	{"backup", "create"},
	{"backup", "verify"},
	{"backup", "restore"},
	{"migrate"},
	{"migrate", "plan"},
	{"migrate", "apply"},
	{"migrate", "adopt"},
	{"migrate", "adopt", "plan"},
	{"migrate", "adopt", "apply"},
	{"plan"},
	{"plan", "validate"},
	{"plan", "apply"},
	{"doctor"},
	{"run"},
	{"run", "start"},
	{"run", "retry"},
	{"run", "renew"},
	{"run", "finish"},
	{"run", "cancel"},
	{"run", "show"},
	{"run", "list"},
	{"verify"},
	{"context"},
	{"generate"},
	{"parse-prd"},
	{"claim"},
	{"claim-next"},
	{"release"},
	{"live"},
	{"live", "feed"},
	{"worktree"},
	{"worktree", "provision"},
	{"worktree", "list"},
	{"worktree", "remove"},
	{"worktree", "recover"},
	{"worktree", "prune"},
	{"dispatch"},
	{"dispatch", "prepare"},
	{"dispatch", "reconcile"},
	{"version"},
}

// helpSpellings are the flag forms every command path must accept.
var helpSpellings = []string{"-h", "-help", "--help"}

// runHelpSubprocess executes the test binary as the CLI with args via the
// helper process, returning stdout, stderr, and the exit code.
func runHelpSubprocess(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestHelpFlagsHelperProcess$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "NOISE_TASKS_TEST_HELP_HELPER=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("helper process: %v", err)
		}
		code = exitErr.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

// TestHelpFlagsHelperProcess re-executes the test binary as noise-tasks.
func TestHelpFlagsHelperProcess(t *testing.T) {
	if os.Getenv("NOISE_TASKS_TEST_HELP_HELPER") != "1" {
		return
	}
	args := []string{}
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	os.Args = append([]string{"noise-tasks"}, args...)
	main()
	os.Exit(0)
}

// TestHelpEveryCommandPath asserts that every enumerated command path
// answers all three help spellings with a usage line on stdout, an empty
// stderr, and exit 0.
func TestHelpEveryCommandPath(t *testing.T) {
	for _, path := range helpCommandPaths {
		for _, spelling := range helpSpellings {
			name := strings.Join(append(append([]string{}, path...), spelling), " ")
			t.Run(name, func(t *testing.T) {
				stdout, stderr, code := runHelpSubprocess(t, append(append([]string{}, path...), spelling)...)
				if code != 0 {
					t.Fatalf("exit %d, want 0; stderr = %q", code, stderr)
				}
				if !strings.Contains(stdout, "usage:") {
					t.Fatalf("stdout = %q, want a usage line", stdout)
				}
				if stderr != "" {
					t.Fatalf("stderr = %q, want empty", stderr)
				}
			})
		}
	}
}

// TestHelpGroupWordForm asserts the bare `help` word answers group usage
// like the flag spellings, matching the existing run/dispatch convention.
func TestHelpGroupWordForm(t *testing.T) {
	for _, group := range [][]string{
		{"run"}, {"worktree"}, {"live"}, {"milestone"}, {"backup"},
		{"migrate"}, {"migrate", "adopt"}, {"plan"}, {"dispatch"},
	} {
		name := strings.Join(group, " ")
		t.Run(name, func(t *testing.T) {
			args := append(append([]string(nil), group...), "help")
			stdout, stderr, code := runHelpSubprocess(t, args...)
			if code != 0 {
				t.Fatalf("exit %d, want 0; stderr = %q", code, stderr)
			}
			if !strings.Contains(stdout, "usage:") {
				t.Fatalf("stdout = %q, want a usage line", stdout)
			}
			if stderr != "" {
				t.Fatalf("stderr = %q, want empty", stderr)
			}
		})
	}
}

// TestTopLevelHelpForms asserts the bare host-level spellings.
func TestTopLevelHelpForms(t *testing.T) {
	for _, spelling := range append([]string{"help"}, helpSpellings...) {
		if spelling == "help" {
			// The bare `help` word is not a top-level command; only the
			// flag spellings are contracted here.
			continue
		}
		t.Run(spelling, func(t *testing.T) {
			stdout, stderr, code := runHelpSubprocess(t, spelling)
			if code != 0 {
				t.Fatalf("exit %d, want 0; stderr = %q", code, stderr)
			}
			if !strings.Contains(stdout, "usage: noise-tasks <command> [options]") {
				t.Fatalf("stdout = %q, want top-level usage", stdout)
			}
			if stderr != "" {
				t.Fatalf("stderr = %q, want empty", stderr)
			}
		})
	}
}

// TestBareInvocationStillUsageError pins the unchanged bare behavior.
func TestBareInvocationStillUsageError(t *testing.T) {
	stdout, stderr, code := runHelpSubprocess(t)
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "usage: noise-tasks <command> [options]") {
		t.Fatalf("stderr = %q, want usage", stderr)
	}
}

// TestUnknownSubcommandStillUsageError pins the unchanged refusal.
func TestUnknownSubcommandStillUsageError(t *testing.T) {
	for _, args := range [][]string{
		{"live", "frobnicate"},
		{"worktree", "frobnicate"},
		{"run", "frobnicate"},
		{"milestone", "frobnicate"},
		{"backup", "frobnicate"},
		{"migrate", "frobnicate"},
		{"plan", "frobnicate"},
		{"dispatch", "frobnicate"},
		{"frobnicate"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			stdout, _, code := runHelpSubprocess(t, args...)
			if code == 0 {
				t.Fatalf("exit 0, want nonzero")
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want empty", stdout)
			}
		})
	}
}

// helpGroupUsageLine runs one group's in-process help and returns its
// usage line.
func helpGroupUsageLine(t *testing.T, group string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	var code int
	switch group {
	case "run":
		code = cmdRun([]string{"-h"}, &stdout, &stderr)
	case "worktree":
		code = cmdWorktree([]string{"-h"}, &stdout, &stderr)
	case "live":
		code = cmdLive([]string{"-h"}, &stdout, &stderr)
	case "milestone":
		code = cmdMilestone([]string{"-h"}, &stdout, &stderr)
	case "backup":
		code = cmdBackup([]string{"-h"}, &stdout, &stderr)
	case "migrate":
		code = cmdMigrate([]string{"-h"}, &stdout, &stderr)
	case "migrate adopt":
		code = cmdMigrateAdopt([]string{"-h"}, &stdout, &stderr)
	case "plan":
		code = cmdPlan([]string{"-h"}, &stdout, &stderr)
	case "dispatch":
		code = cmdDispatch([]string{"-h"}, &stdout, &stderr)
	default:
		t.Fatalf("unknown group %q", group)
	}
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("%s -h: exit %d, stderr %q", group, code, stderr.String())
	}
	return stdout.String()
}

// subcommandAlternatives parses the `<a|b|c>` list from a usage line.
func subcommandAlternatives(t *testing.T, usage string) []string {
	t.Helper()
	m := regexp.MustCompile(`<([^<>|]+(?:\|[^<>|]+)*)>`).FindStringSubmatch(usage)
	if m == nil {
		t.Fatalf("usage %q has no <a|b|c> subcommand list", usage)
	}
	return strings.Split(m[1], "|")
}

// TestHelpTableCoversTopLevelListing derives the top-level command list
// from printUsage — the single dispatch-adjacent listing — and asserts the
// help table covers every listed command, so a new command cannot ship
// without help coverage.
func TestHelpTableCoversTopLevelListing(t *testing.T) {
	var out bytes.Buffer
	printUsage(&out)
	covered := map[string]bool{}
	for _, path := range helpCommandPaths {
		covered[path[0]] = true
	}
	inCommands := false
	for _, line := range strings.Split(out.String(), "\n") {
		if line == "commands:" {
			inCommands = true
			continue
		}
		if !inCommands || line == "" {
			continue
		}
		if strings.HasPrefix(line, "run '") {
			break
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if !covered[name] {
			t.Errorf("command %q is listed in top-level usage but missing from helpCommandPaths", name)
		}
	}
}

// TestHelpTableCoversGroupSubcommands derives each group's subcommand list
// from its own help output and asserts the help table covers every
// subcommand, so a new subcommand cannot ship without help coverage.
func TestHelpTableCoversGroupSubcommands(t *testing.T) {
	covered := map[string]bool{}
	for _, path := range helpCommandPaths {
		covered[strings.Join(path, " ")] = true
	}
	for _, group := range []string{"run", "worktree", "live", "milestone", "backup", "migrate", "migrate adopt", "plan", "dispatch"} {
		usage := helpGroupUsageLine(t, group)
		for _, sub := range subcommandAlternatives(t, usage) {
			if !covered[group+" "+sub] {
				t.Errorf("subcommand %q of group %q is listed in group help but missing from helpCommandPaths", sub, group)
			}
		}
	}
}
