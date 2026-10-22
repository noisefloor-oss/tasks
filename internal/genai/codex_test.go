package genai

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func stubDraftCLI(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	executable, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return executable
}

const codexAnswerArgument = `
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--output-last-message" ]; then
    shift
    answer="$1"
  fi
  shift
done
`

func recordedCLIFile(t *testing.T, dir, name string) string {
	t.Helper()
	value, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}

func assertTemporaryWorkingDirectory(t *testing.T, records string) string {
	t.Helper()
	cwd := strings.TrimSpace(recordedCLIFile(t, records, "cwd"))
	if cwd == "" || !filepath.IsAbs(cwd) {
		t.Fatalf("working directory = %q", cwd)
	}
	if entries := recordedCLIFile(t, records, "entries"); entries != "" {
		t.Fatalf("working directory was not empty: %q", entries)
	}
	if _, err := os.Stat(cwd); !os.IsNotExist(err) {
		t.Fatalf("temporary working directory remains: %q, %v", cwd, err)
	}
	return cwd
}

func TestCodexGenerateTasksProtocol(t *testing.T) {
	for _, model := range []string{"", "gpt-5.3-codex"} {
		t.Run("model="+model, func(t *testing.T) {
			records := t.TempDir()
			executable := stubDraftCLI(t, "codex", `
printf '<%s>\n' "$@" > "`+records+`/args"
pwd > "`+records+`/cwd"
ls -A > "`+records+`/entries"
cat > "`+records+`/stdin"
`+codexAnswerArgument+`
printf '%s' '`+"```json\n"+validDraftsJSON+"\n```"+`' > "$answer"
printf 'progress that is not JSON\n'
`)
			provider, err := NewCodex(executable, model)
			if err != nil {
				t.Fatal(err)
			}
			if provider.Timeout != defaultCLITimeout {
				t.Fatalf("timeout = %s", provider.Timeout)
			}
			result, err := provider.GenerateTasks(context.Background(), "the PRD text", 2)
			if err != nil || len(result.Drafts) != 2 || result.ResolvedModel != "" {
				t.Fatalf("result = %+v, %v", result, err)
			}
			cwd := assertTemporaryWorkingDirectory(t, records)
			answer := filepath.Join(filepath.Dir(cwd), "answer")
			args := []string{"exec", "-", "--sandbox", "read-only", "--skip-git-repo-check",
				"--ephemeral", "--color", "never", "-C", cwd, "--output-last-message", answer,
				"--ignore-user-config", "--ignore-rules",
				"--disable", "shell_tool", "--disable", "apps", "--disable", "plugins",
				"--disable", "multi_agent", "--disable", "image_generation", "--disable", "view_image",
				"-c", `web_search="disabled"`}
			if model != "" {
				args = append(args, "-m", model)
			}
			wantArgs := "<" + strings.Join(args, ">\n<") + ">\n"
			if got := recordedCLIFile(t, records, "args"); got != wantArgs {
				t.Fatalf("argv = %q, want %q", got, wantArgs)
			}
			if got := recordedCLIFile(t, records, "stdin"); got != draftSystemPrompt+"\n\n"+draftUserPrompt(2, "the PRD text") {
				t.Fatalf("stdin = %q", got)
			}
			if _, err := os.Stat(filepath.Dir(cwd)); !os.IsNotExist(err) {
				t.Fatalf("temporary output directory remains: %v", err)
			}
		})
	}
}

func TestCodexFinalAnswerFailures(t *testing.T) {
	for _, test := range []struct{ name, script, want string }{
		{"missing", "printf '%s' '" + validDraftsJSON + "'", "unavailable"},
		{"invalid", `printf 'not JSON' > "$answer"`, "JSON"},
		{"empty", `printf '[]' > "$answer"`, "no actionable"},
		{"oversized", `head -c 8388609 /dev/zero > "$answer"`, "final answer exceeded"},
		{"symlink", `ln -s /dev/zero "$answer"`, "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			executable := stubDraftCLI(t, "codex", codexAnswerArgument+test.script)
			provider, err := NewCodex(executable, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.GenerateTasks(context.Background(), "prd", 3); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestIsolatedCLIFailuresAndModelValidation(t *testing.T) {
	for _, adapter := range []string{"codex", "kimi"} {
		newProvider := func(executable, model string, timeout time.Duration) (Provider, error) {
			if adapter == "codex" {
				p, err := NewCodex(executable, model)
				if err == nil {
					p.Timeout = timeout
				}
				return p, err
			}
			p, err := NewKimi(executable, model)
			if err == nil {
				p.Timeout = timeout
			}
			return p, err
		}
		t.Run(adapter, func(t *testing.T) {
			for _, test := range []struct{ name, script, want string }{
				{"exit", "echo credential-shaped-secret >&2\nexit 7", "exit code 7"},
				{"timeout", "exec sleep 30", "timed out"},
				{"stdout overflow", "head -c 8388609 /dev/zero", "stdout exceeded"},
				{"stderr overflow", "head -c 65537 /dev/zero >&2", "stderr exceeded"},
			} {
				t.Run(test.name, func(t *testing.T) {
					records := t.TempDir()
					executable := stubDraftCLI(t, adapter, `pwd > "`+records+`/cwd"
ls -A > "`+records+`/entries"
`+test.script)
					timeout := time.Minute
					if test.name == "timeout" {
						timeout = 100 * time.Millisecond
					}
					provider, err := newProvider(executable, "provider/model", timeout)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := provider.GenerateTasks(context.Background(), "prd", 1); err == nil ||
						!strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "credential-shaped-secret") {
						t.Fatalf("error = %v, want %q", err, test.want)
					}
					assertTemporaryWorkingDirectory(t, records)
				})
			}
			executable := stubDraftCLI(t, adapter, "exit 0")
			for _, model := range []string{" model ", "--danger", "line\nbreak", "\xff", strings.Repeat("m", maxModelBytes+1)} {
				if _, err := newProvider(executable, model, time.Minute); err == nil {
					t.Errorf("model %q was accepted", model)
				}
			}
			if _, err := newProvider(filepath.Join(t.TempDir(), "missing"), "", time.Minute); err == nil {
				t.Fatal("missing executable accepted")
			}
			provider, err := newProvider(executable, "", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := provider.GenerateTasks(ctx, "prd", 1); err == nil || !strings.Contains(err.Error(), "cancelled") {
				t.Fatalf("cancellation error = %v", err)
			}
		})
	}
}
