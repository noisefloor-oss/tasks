// Package conform mechanically asserts the SPEC §5.3 plugin surface
// contract against a built noise-<noun> plugin binary. It is a public
// package so sibling repositories in the plugin family can run the same
// probe against their own plugins.
//
// The probe is black-box: it asserts the listed-to-dispatched direction of
// the §5.3 command-list invariant by invoking every listed command's help.
// The other direction — a dispatched command omitted from the list — cannot
// be observed from outside the binary; §5.3 assigns it to the plugin's own
// command table being the single source for both dispatch and help, verified
// by the plugin's own tests and review.
package conform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// execTimeout bounds every plugin invocation the probe makes. §5.3 requires
// the help and version forms to return promptly; a plugin that does not
// answer within this window fails conformance.
const execTimeout = 10 * time.Second

var (
	usageLine   = regexp.MustCompile(`^usage: \S+ <command> \[options\]$`)
	commandLine = regexp.MustCompile(`^  (\S+)  +(\S.*)$`)
	// usageLinePrefix is the §5.3 per-command usage line: first word
	// "usage" in any letter case ("usage: list ...", "Usage of list:").
	usageLinePrefix = regexp.MustCompile(`(?i)^\s*usage\b`)
	// helpPointerLine is the §5.3 pointer at a contracted help form: the
	// word "help" as a standalone token, not a substring of another word.
	helpPointerLine = regexp.MustCompile(`(?i)(^|[\s'"])-{0,2}help([\s'".]|$)`)
	// semver is the SemVer 2.0 grammar (semver.org): no leading-zero core
	// numbers, no empty dot-separated identifiers.
	semverIdent = `(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)`
	semverCore  = `(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)`
	semver      = semverCore + `(?:-` + semverIdent + `(?:\.` + semverIdent + `)*)?` +
		`(?:\+[0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*)?`
	// versionValue is the §5.3 version field: strict SemVer 2.0 or the
	// literal development build version (§5.5).
	versionValue = regexp.MustCompile(`^(?:dev|` + semver + `)$`)
)

// TestPlugin asserts the SPEC §5.3 plugin surface contract against the
// plugin binary at binary. Every violation found is reported as a separate
// test error.
func TestPlugin(t *testing.T, binary string) {
	t.Helper()
	for _, err := range Check(binary) {
		t.Error(err)
	}
}

// Check runs the §5.3 contract checks against the plugin binary at binary
// and returns every violation found. A nil slice means the binary conforms.
func Check(binary string) []error {
	probe := &pluginProbe{binary: binary, name: filepath.Base(binary)}
	var failures []error
	fail := func(format string, args ...any) {
		failures = append(failures, fmt.Errorf(format, args...))
	}

	// The four top-level help forms, each from a bare working directory.
	var commands []string
	for _, args := range [][]string{{}, {"help"}, {"--help"}, {"-h"}} {
		stdout, stderr, code, err := probe.runInBareDir(args...)
		form := strings.Join(append([]string{probe.name}, args...), " ")
		if err != nil {
			fail("%s: %v", form, err)
			continue
		}
		if code != 0 {
			fail("%s: exit %d, want 0 (stderr %q)", form, code, stderr)
			continue
		}
		if stderr != "" {
			fail("%s: stderr %q, want empty", form, stderr)
		}
		parsed, err := parseTopHelp(stdout)
		if err != nil {
			fail("%s: malformed top-level help: %v", form, err)
			continue
		}
		if commands == nil {
			commands = parsed
		}
	}
	if commands == nil {
		return failures
	}

	// version is a contracted subcommand and must be listed.
	if !contains(commands, "version") {
		fail("top-level help does not list the version command")
	}

	// Every listed command dispatches and provides per-command help naming
	// the command.
	for _, command := range commands {
		if !probe.perCommandHelpWorks(command) {
			fail("%s %s help: one of -h, -help, --help did not exit 0 with a usage line on stdout",
				probe.name, command)
		}
	}

	// The version subcommand, held to the same context independence and
	// side-effect-free bar as the help forms. §5.3 contracts the complete
	// output: one "<name> <version>" line, nothing else.
	stdout, stderr, code, err := probe.runInBareDir("version")
	switch {
	case err != nil:
		fail("%s version: %v", probe.name, err)
	case code != 0:
		fail("%s version: exit %d, want 0 (stderr %q)", probe.name, code, stderr)
	case !validVersionOutput(probe.name, stdout):
		fail("%s version: stdout %q, want exactly %q", probe.name, stdout, probe.name+" <version>")
	case stderr != "":
		fail("%s version: stderr %q, want empty", probe.name, stderr)
	}

	// An unknown command exits nonzero with nothing on stdout and a stderr
	// diagnostic that names the rejected command, shows a usage line, and
	// points at help.
	stdout, stderr, code, err = probe.run("conform-probe-no-such-command")
	switch {
	case err != nil:
		fail("%s <unknown>: %v", probe.name, err)
	default:
		if code == 0 {
			fail("%s <unknown>: exit 0, want nonzero", probe.name)
		}
		if stdout != "" {
			fail("%s <unknown>: stdout %q, want empty", probe.name, stdout)
		}
		if problem := unknownCommandStderrProblem(stderr, "conform-probe-no-such-command"); problem != "" {
			fail("%s <unknown>: stderr %q, %s", probe.name, stderr, problem)
		}
	}
	return failures
}

type pluginProbe struct {
	binary string
	name   string
}

// runInBareDir runs the plugin from a fresh empty working directory and
// verifies the run left no trace: §5.3 help forms are side-effect-free and
// need no project context.
func (p *pluginProbe) runInBareDir(args ...string) (stdout, stderr string, code int, err error) {
	dir, err := os.MkdirTemp("", "conform-probe")
	if err != nil {
		return "", "", -1, err
	}
	defer os.RemoveAll(dir)
	stdout, stderr, code, err = p.runFrom(dir, args...)
	if err != nil {
		return "", "", -1, err
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		return "", "", -1, readErr
	}
	if len(entries) != 0 {
		return "", "", -1, fmt.Errorf("left %d file(s) behind in a bare working directory", len(entries))
	}
	return stdout, stderr, code, nil
}

func (p *pluginProbe) run(args ...string) (stdout, stderr string, code int, err error) {
	return p.runFrom("", args...)
}

func (p *pluginProbe) runFrom(dir string, args ...string) (stdout, stderr string, code int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.binary, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var outBuf, errBuf strings.Builder
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return "", "", -1, fmt.Errorf("did not exit within %s", execTimeout)
	}
	if runErr == nil {
		return outBuf.String(), errBuf.String(), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return outBuf.String(), errBuf.String(), exitErr.ExitCode(), nil
	}
	return "", "", -1, runErr
}

// perCommandHelpWorks reports whether every contracted per-command help
// form (-h, -help, --help) exits 0 with a usage line naming the command
// on stdout and an empty stderr: a line whose first word is "usage" (any
// case) and that contains the command name. §5.3 requires all three
// forms; help output goes to stdout only.
func (p *pluginProbe) perCommandHelpWorks(command string) bool {
	for _, flag := range []string{"-h", "-help", "--help"} {
		stdout, stderr, code, err := p.run(command, flag)
		if err != nil || code != 0 {
			return false
		}
		if stderr != "" || !hasUsageLineFor(stdout, command) {
			return false
		}
	}
	return true
}

// hasUsageLineFor reports whether output carries a §5.3 usage line naming
// the command.
func hasUsageLineFor(output, command string) bool {
	for _, line := range strings.Split(output, "\n") {
		if usageLinePrefix.MatchString(line) &&
			strings.Contains(strings.ToLower(line), strings.ToLower(command)) {
			return true
		}
	}
	return false
}

// validVersionOutput reports whether stdout is exactly the §5.3 version
// line: "<name> <version>\n" and nothing else.
func validVersionOutput(name, stdout string) bool {
	line, ok := strings.CutSuffix(stdout, "\n")
	if !ok || strings.Contains(line, "\n") {
		return false
	}
	plugin, version, ok := strings.Cut(line, " ")
	return ok && plugin == name && versionValue.MatchString(version)
}

// unknownCommandStderrProblem validates the §5.3 unknown-command stderr as
// three separately parsed elements: a diagnostic line naming the rejected
// command (not the usage line itself), a usage line, and a line pointing at
// a contracted help form. It returns "" when the output conforms, else the
// first missing element.
func unknownCommandStderrProblem(stderr, command string) string {
	diagnostic, usage, helpPointer := false, false, false
	for _, line := range strings.Split(stderr, "\n") {
		if usageLinePrefix.MatchString(line) {
			usage = true
			continue
		}
		if strings.Contains(line, command) {
			diagnostic = true
		}
		if helpPointerLine.MatchString(line) {
			helpPointer = true
		}
	}
	if !diagnostic {
		return "want a diagnostic line naming the rejected command"
	}
	if !usage {
		return "want a usage line"
	}
	if !helpPointer {
		return "want a line pointing at a help form"
	}
	return ""
}

// parseTopHelp validates the §5.3 top-level help shape and returns the
// listed command names in order.
func parseTopHelp(stdout string) ([]string, error) {
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) == 0 || !usageLine.MatchString(lines[0]) {
		return nil, fmt.Errorf("first line %q, want %q", firstLine(lines), "usage: <name> <command> [options]")
	}
	i := 1
	if i >= len(lines) || lines[i] != "" {
		return nil, fmt.Errorf("line 2 %q, want blank", nth(lines, i))
	}
	i++
	if i >= len(lines) || lines[i] != "commands:" {
		return nil, fmt.Errorf("line 3 %q, want %q", nth(lines, i), "commands:")
	}
	i++
	var commands []string
	for i < len(lines) && lines[i] != "" {
		match := commandLine.FindStringSubmatch(lines[i])
		if match == nil {
			return nil, fmt.Errorf("command line %q, want two-space indent, name, 2+ spaces, description", lines[i])
		}
		if contains(commands, match[1]) {
			return nil, fmt.Errorf("command %q listed twice", match[1])
		}
		commands = append(commands, match[1])
		i++
	}
	if len(commands) == 0 {
		return nil, errors.New("commands section is empty")
	}
	i++
	if i != len(lines)-1 || lines[i] == "" || !strings.Contains(lines[i], "<command>") || !strings.Contains(lines[i], "help") {
		return nil, fmt.Errorf("footer %q, want one line naming how to reach per-command flags", nth(lines, i))
	}
	return commands, nil
}

func firstLine(lines []string) string { return nth(lines, 0) }

func nth(lines []string, i int) string {
	if i < 0 || i >= len(lines) {
		return ""
	}
	return lines[i]
}

func contains(list []string, item string) bool {
	for _, entry := range list {
		if entry == item {
			return true
		}
	}
	return false
}
