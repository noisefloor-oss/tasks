//go:build unix

package conform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures are /bin/sh plugins so the probe exercises real processes.
// conformingFixture satisfies every §5.3 clause; each broken fixture
// violates exactly one.

const conformingFixture = `#!/bin/sh
usage() {
cat <<'EOF'
usage: noise-fake <command> [options]

commands:
  alpha    do alpha things
  beta     do beta things
  version  print version

run 'noise-fake <command> -help' for per-command flags.
EOF
}
case "${1:-}" in
""|help|-h|--help)
	usage
	;;
alpha|beta)
	case "${2:-}" in
	-h|-help|--help)
		echo "Usage of $1:"
		echo "  -json"
		echo "    	machine-readable output"
		;;
	*)
		echo "$1 ran"
		;;
	esac
	;;
version)
	case "${2:-}" in
	-h|-help|--help)
		echo "Usage of version:"
		;;
	*)
		echo "noise-fake 1.0.0"
		;;
	esac
	;;
*)
	echo "noise-fake: unknown command: $1" >&2
	usage >&2
	exit 2
	;;
esac
`

func TestConformingPluginPasses(t *testing.T) {
	binary := writeFixture(t, "noise-fake", conformingFixture)
	if failures := Check(binary); len(failures) != 0 {
		t.Fatalf("conforming fixture failed: %v", failures)
	}
	TestPlugin(t, binary)
}

func TestHelpOnStderrFails(t *testing.T) {
	// no-args and the help word must print to stdout and exit 0; sending
	// usage to stderr with exit 2 violates the contract per form.
	fixture := replace(t, conformingFixture,
		"\"\"|help|-h|--help)\n\tusage\n\t;;",
		"\"\"|help)\n\tusage >&2\n\texit 2\n\t;;")
	binary := writeFixture(t, "noise-fake", fixture)
	failures := Check(binary)
	wantSubstrings(t, failures, []string{
		"noise-fake: exit 2, want 0",
		"noise-fake help: exit 2, want 0",
	})
}

func TestPhantomListedCommandFails(t *testing.T) {
	fixture := replace(t, conformingFixture,
		"  beta     do beta things\n",
		"  beta     do beta things\n  ghost    not dispatched\n")
	binary := writeFixture(t, "noise-fake", fixture)
	failures := Check(binary)
	wantSubstrings(t, failures, []string{"noise-fake ghost help: one of -h, -help, --help"})
}

func TestMissingVersionCommandFails(t *testing.T) {
	fixture := replace(t, conformingFixture, "  version  print version\n", "")
	fixture = replace(t, fixture, `version)
	case "${2:-}" in
	-h|-help|--help)
		echo "Usage of version:"
		;;
	*)
		echo "noise-fake 1.0.0"
		;;
	esac
	;;
`, "")
	binary := writeFixture(t, "noise-fake", fixture)
	failures := Check(binary)
	wantSubstrings(t, failures, []string{
		"top-level help does not list the version command",
		"noise-fake version: exit 2, want 0",
	})
}

func TestMalformedTopHelpFails(t *testing.T) {
	fixture := replace(t, conformingFixture,
		"  alpha    do alpha things\n",
		"  alpha do alpha things\n")
	binary := writeFixture(t, "noise-fake", fixture)
	failures := Check(binary)
	wantSubstrings(t, failures, []string{"malformed top-level help"})
}

func TestUnknownCommandSuccessFails(t *testing.T) {
	fixture := replace(t, conformingFixture, "\texit 2\n\t;;\nesac", "\texit 0\n\t;;\nesac")
	binary := writeFixture(t, "noise-fake", fixture)
	failures := Check(binary)
	wantSubstrings(t, failures, []string{"noise-fake <unknown>: exit 0, want nonzero"})
}

func TestSideEffectingHelpFails(t *testing.T) {
	fixture := replace(t, conformingFixture,
		"\"\"|help|-h|--help)\n\tusage\n\t;;",
		"\"\"|help|-h|--help)\n\tusage\n\ttouch state.db\n\t;;")
	binary := writeFixture(t, "noise-fake", fixture)
	failures := Check(binary)
	wantSubstrings(t, failures, []string{"left 1 file(s) behind"})
}

func TestPerCommandHelpWithoutUsageLineFails(t *testing.T) {
	// Output that mentions "usage" and the command name without an actual
	// usage line naming the command is not the contracted per-command
	// help.
	fixture := replace(t, conformingFixture,
		"\t\techo \"Usage of $1:\"\n",
		"\t\techo \"$1 is available; no usage information\"\n")
	binary := writeFixture(t, "noise-fake", fixture)
	failures := Check(binary)
	wantSubstrings(t, failures, []string{
		"noise-fake alpha help: one of -h, -help, --help did not exit 0 with a usage line on stdout",
		"noise-fake beta help: one of -h, -help, --help did not exit 0 with a usage line on stdout",
	})
}

func TestEmptyVersionOutputFails(t *testing.T) {
	fixture := replace(t, conformingFixture,
		"\t\techo \"noise-fake 1.0.0\"\n",
		"\t\t:\n")
	binary := writeFixture(t, "noise-fake", fixture)
	failures := Check(binary)
	wantSubstrings(t, failures, []string{"noise-fake version: stdout \"\", want exactly \"noise-fake <version>\""})
}

func TestVersionOnStderrFails(t *testing.T) {
	fixture := replace(t, conformingFixture,
		"\t\techo \"noise-fake 1.0.0\"\n",
		"\t\techo \"noise-fake 1.0.0\" >&2\n")
	binary := writeFixture(t, "noise-fake", fixture)
	failures := Check(binary)
	wantSubstrings(t, failures, []string{"noise-fake version: stdout \"\", want exactly \"noise-fake <version>\""})
}

func TestVersionWithoutNameOrWithGarbageFails(t *testing.T) {
	// §5.3 contracts the complete line: plugin name, one space, version.
	for name, line := range map[string]string{
		"version-only":    "1.2.3",
		"garbage-name":    "garbage 1.2.3",
		"missing-version": "noise-fake",
		"extra-line":      "noise-fake 1.0.0\nextra",
	} {
		t.Run(name, func(t *testing.T) {
			fixture := replace(t, conformingFixture,
				"\t\techo \"noise-fake 1.0.0\"\n",
				"\t\tprintf '%s\\n' \""+line+"\"\n")
			binary := writeFixture(t, "noise-fake", fixture)
			failures := Check(binary)
			wantSubstrings(t, failures, []string{"want exactly \"noise-fake <version>\""})
		})
	}
}

func TestVersionValueSemVerGrammar(t *testing.T) {
	// The SemVer 2.0 boundary cases: both prerelease and build metadata
	// are valid; leading-zero core numbers and empty identifiers are not.
	for _, valid := range []string{
		"dev", "0.0.0", "1.2.3", "10.20.30", "1.2.3-rc.1+build.5",
		"1.2.3-alpha", "1.2.3-alpha.1", "1.2.3+build.5", "1.0.0-x-y-z.-",
	} {
		if !versionValue.MatchString(valid) {
			t.Errorf("versionValue rejected valid %q", valid)
		}
	}
	for _, invalid := range []string{
		"", "1.2", "1.2.3.4", "01.2.3", "1.02.3", "1.2.03", "1.2.3-",
		"1.2.3-.", "1.2.3+", "1.2.3+.", "v1.2.3", "1.2.3 ",
	} {
		if versionValue.MatchString(invalid) {
			t.Errorf("versionValue accepted invalid %q", invalid)
		}
	}
}

func TestCwdDependentVersionFails(t *testing.T) {
	// version must work from a bare working directory; consulting a
	// project file violates the same context-independence bar as help.
	fixture := replace(t, conformingFixture,
		"\t\techo \"noise-fake 1.0.0\"\n",
		"\t\tcat version.txt || exit 1\n")
	binary := writeFixture(t, "noise-fake", fixture)
	failures := Check(binary)
	wantSubstrings(t, failures, []string{"noise-fake version: exit 1, want 0"})
}

func TestUnknownCommandNotPointingAtHelpFails(t *testing.T) {
	fixture := replace(t, conformingFixture, "\tusage >&2\n", "")
	binary := writeFixture(t, "noise-fake", fixture)
	failures := Check(binary)
	wantSubstrings(t, failures, []string{"want a usage line"})
}

func TestUnknownCommandWithoutDiagnosticFails(t *testing.T) {
	// stderr that contains the words "usage" and "help" without naming the
	// rejected command carries no diagnostic.
	fixture := replace(t, conformingFixture,
		"\techo \"noise-fake: unknown command: $1\" >&2\n\tusage >&2\n",
		"\techo \"usage help\" >&2\n")
	binary := writeFixture(t, "noise-fake", fixture)
	failures := Check(binary)
	wantSubstrings(t, failures, []string{"want a diagnostic line naming the rejected command"})
}

func TestUnknownCommandNamedOnlyInUsageFails(t *testing.T) {
	// The rejected name appearing only inside the usage line is not a
	// diagnostic, and "helpful" is not a pointer at a help form.
	fixture := replace(t, conformingFixture,
		"\techo \"noise-fake: unknown command: $1\" >&2\n\tusage >&2\n",
		"\techo \"error occurred\" >&2\n\techo \"usage: noise-fake $1 [options]\" >&2\n\techo \"this is helpful\" >&2\n")
	binary := writeFixture(t, "noise-fake", fixture)
	failures := Check(binary)
	wantSubstrings(t, failures, []string{"want a diagnostic line naming the rejected command"})
}

func TestUnknownCommandHelpSubstringIsNotPointerFails(t *testing.T) {
	// A diagnostic names the command and a usage line is present, but no
	// line points at a contracted help form.
	fixture := replace(t, conformingFixture,
		"\tusage >&2\n",
		"\techo \"usage: noise-fake <command> [options]\" >&2\n\techo \"this is helpful\" >&2\n")
	binary := writeFixture(t, "noise-fake", fixture)
	failures := Check(binary)
	wantSubstrings(t, failures, []string{"want a line pointing at a help form"})
}

func TestParseTopHelpShape(t *testing.T) {
	good := "usage: noise-x <command> [options]\n\ncommands:\n  a  one\n\nrun 'noise-x <command> -help' for per-command flags.\n"
	commands, err := parseTopHelp(good)
	if err != nil || len(commands) != 1 || commands[0] != "a" {
		t.Fatalf("parseTopHelp(good) = %v, %v", commands, err)
	}
	for name, bad := range map[string]string{
		"no-usage-line":  "help text\n",
		"missing-blank":  "usage: noise-x <command> [options]\ncommands:\n  a  one\n\nrun 'noise-x <command> -help' for per-command flags.\n",
		"one-space-gap":  "usage: noise-x <command> [options]\n\ncommands:\n  a one\n\nrun 'noise-x <command> -help' for per-command flags.\n",
		"no-description": "usage: noise-x <command> [options]\n\ncommands:\n  a  \n\nrun 'noise-x <command> -help' for per-command flags.\n",
		"empty-commands": "usage: noise-x <command> [options]\n\ncommands:\n\nrun 'noise-x <command> -help' for per-command flags.\n",
		"duplicate":      "usage: noise-x <command> [options]\n\ncommands:\n  a  one\n  a  again\n\nrun 'noise-x <command> -help' for per-command flags.\n",
		"missing-footer": "usage: noise-x <command> [options]\n\ncommands:\n  a  one\n\n",
		"footer-no-help": "usage: noise-x <command> [options]\n\ncommands:\n  a  one\n\nnothing else.\n",
		"trailing-lines": "usage: noise-x <command> [options]\n\ncommands:\n  a  one\n\nrun 'noise-x <command> -help' for per-command flags.\nextra\n",
	} {
		if _, err := parseTopHelp(bad); err == nil {
			t.Errorf("parseTopHelp(%s) accepted malformed help", name)
		}
	}
}

func writeFixture(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func replace(t *testing.T, haystack, old, new string) string {
	t.Helper()
	if !strings.Contains(haystack, old) {
		t.Fatalf("fixture does not contain %q", old)
	}
	return strings.Replace(haystack, old, new, 1)
}

func wantSubstrings(t *testing.T, failures []error, wants []string) {
	t.Helper()
	var rendered []string
	for _, err := range failures {
		rendered = append(rendered, err.Error())
	}
	joined := strings.Join(rendered, "\n")
	for _, want := range wants {
		if !strings.Contains(joined, want) {
			t.Errorf("failures missing %q; got:\n%s", want, joined)
		}
	}
}
