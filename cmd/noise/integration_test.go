//go:build unix

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	modulePath  = "github.com/noisefloor-oss/tasks"
	testVersion = "14.test"
	warningLine = "warning: noisetask is deprecated; use noise tasks (or noise-tasks); removal is gated by the published migration plan\n"
)

type builtInstallation struct {
	root       string
	noise      string
	noiseTasks string
	shim       string
}

type probeOutput struct {
	Args  []string `json:"args"`
	Env   []string `json:"env"`
	CWD   string   `json:"cwd"`
	Stdin string   `json:"stdin"`
}

type setupProcessResult struct {
	SchemaVersion          int    `json:"schemaVersion"`
	ProjectBoundary        string `json:"projectBoundary"`
	StorePath              string `json:"storePath"`
	PreferencesPath        string `json:"preferencesPath"`
	StoreAction            string `json:"storeAction"`
	PreferencesAction      string `json:"preferencesAction"`
	AdapterAvailable       bool   `json:"adapterAvailable"`
	AuthenticationVerified bool   `json:"authenticationVerified"`
	Generation             struct {
		Adapter string `json:"adapter"`
		Model   string `json:"model"`
	} `json:"generation"`
}

type setupChildProcess struct {
	cmd        *exec.Cmd
	stdout     bytes.Buffer
	stderr     bytes.Buffer
	model      string
	waitResult chan error
}

func TestRealProcessDispatchTransparency(t *testing.T) {
	installation := buildProbeInstallation(t)
	stdout, stderr, code := collectProcess(exec.Command(installation.noise, "plugins"))
	if code != 0 || stdout != "alpha\ntasks\nzed\n" || stderr != "" {
		t.Fatalf("real discovery = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	for _, noun := range []string{"Tasks", "noise-tasks", "../tasks", "task--tools"} {
		stdout, stderr, code = collectProcess(exec.Command(installation.noise, noun))
		if code != 2 || stdout != "" || !strings.Contains(stderr, "invalid plugin noun") {
			t.Errorf("invalid noun %q = code %d stdout %q stderr %q", noun, code, stdout, stderr)
		}
	}
	cwd := t.TempDir()
	stdin := "input\x00bytes\n"
	args := []string{"dump", "two words", "--literal=ø"}

	cmd := exec.Command(installation.noise, append([]string{"tasks"}, args...)...)
	cmd.Dir = cwd
	cmd.Env = []string{"KEEP=one=two", "NOISE_PROJECT_DIR="}
	cmd.Stdin = strings.NewReader(stdin)
	stdout, stderr, code = collectProcess(cmd)
	if code != 0 || stderr != "probe-stderr\n" {
		t.Fatalf("noise tasks dump exit %d, stderr %q", code, stderr)
	}
	got := decodeProbe(t, stdout)
	if want := append([]string{"noise-tasks"}, args...); !equalStrings(got.Args, want) {
		t.Errorf("argv = %#v, want %#v", got.Args, want)
	}
	if got.CWD != cwd || got.Stdin != stdin {
		t.Errorf("cwd/stdin = %q/%q, want %q/%q", got.CWD, got.Stdin, cwd, stdin)
	}
	if want := []string{"KEEP=one=two", "NOISE_PROJECT_DIR=" + cwd}; !equalStrings(got.Env, want) {
		t.Errorf("environment = %#v, want exact %#v", got.Env, want)
	}

	_, stderr, code = collectProcess(exec.Command(installation.noise, "tasks", "exit", "37"))
	if code != 37 || stderr != "" {
		t.Errorf("host exit passthrough = %d, stderr %q; want 37 and empty", code, stderr)
	}
}

func TestCompatibilityShimTransparency(t *testing.T) {
	installation := buildProbeInstallation(t)
	cwd := t.TempDir()
	cmd := exec.Command(installation.shim, "dump", "two words")
	cmd.Dir = cwd
	cmd.Env = []string{"KEEP=shim", "NOISE_PROJECT_DIR=/preserved"}
	cmd.Stdin = strings.NewReader("input\n")
	stdout, stderr, code := collectProcess(cmd)
	if code != 0 || stderr != warningLine+"probe-stderr\n" {
		t.Fatalf("shim dump exit %d, stderr %q", code, stderr)
	}
	got := decodeProbe(t, stdout)
	if want := []string{"noise-tasks", "dump", "two words"}; !equalStrings(got.Args, want) {
		t.Errorf("shim argv = %#v, want %#v", got.Args, want)
	}
	if want := []string{"KEEP=shim", "NOISE_PROJECT_DIR=/preserved"}; !equalStrings(got.Env, want) {
		t.Errorf("shim environment = %#v, want %#v", got.Env, want)
	}
	if got.CWD != cwd || got.Stdin != "input\n" {
		t.Errorf("shim cwd/stdin = %q/%q", got.CWD, got.Stdin)
	}
	_, stderr, code = collectProcess(exec.Command(installation.shim, "exit", "37"))
	if code != 37 || stderr != warningLine {
		t.Errorf("shim exit passthrough = %d, stderr %q; want 37 and one warning", code, stderr)
	}
}

func TestRealProcessSignalAndPIDTransparency(t *testing.T) {
	installation := buildProbeInstallation(t)
	for _, test := range []struct {
		name   string
		path   string
		args   []string
		stderr string
	}{
		{name: "host", path: installation.noise, args: []string{"tasks", "block"}},
		{name: "shim", path: installation.shim, args: []string{"block"}, stderr: warningLine},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertSignalExec(t, test.path, test.args, test.stderr)
		})
	}
}

func assertSignalExec(t *testing.T, path string, args []string, wantStderr string) {
	t.Helper()
	cmd := exec.Command(path, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	t.Cleanup(func() {
		if !finished {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		want := fmt.Sprintf("ready %d\n", cmd.Process.Pid)
		if line != want {
			t.Fatalf("plugin pid line = %q, want %q", line, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for execed plugin")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	finished = true
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("wait error = %v, want signal exit", err)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Fatalf("wait status = %v, want SIGTERM", exitErr.Sys())
	}
	if stderr.String() != wantStderr {
		t.Fatalf("stderr = %q, want %q", stderr.String(), wantStderr)
	}
}

func TestRealProcessRuntimeRefusalsAndFixedRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(base, "noise")
	buildCommand(t, bin, "./cmd/noise", root, "")

	assertExit(t, exec.Command(bin, "missing"), 127, "noise: plugin not installed: missing\n")
	if err := os.WriteFile(filepath.Join(root, "noise-noexec"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertExitContains(t, exec.Command(bin, "noexec"), 126, "unsafe plugin")
	if err := os.Mkdir(filepath.Join(root, "noise-directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	assertExitContains(t, exec.Command(bin, "directory"), 126, "unsafe plugin")
	outside := filepath.Join(base, "outside")
	writeProbeScript(t, outside, "#!/bin/sh\necho escaped\n")
	if err := os.Symlink(outside, filepath.Join(root, "noise-escape")); err != nil {
		t.Fatal(err)
	}
	assertExitContains(t, exec.Command(bin, "escape"), 126, "outside configured root")
	writeProbeScript(t, filepath.Join(root, "noise-badexec"), "#!/definitely/missing\n")
	assertExitContains(t, exec.Command(bin, "badexec"), 126, "execute badexec")

	malicious := filepath.Join(base, "path")
	if err := os.Mkdir(malicious, 0o755); err != nil {
		t.Fatal(err)
	}
	writeProbeScript(t, filepath.Join(malicious, "noise-pathonly"), "#!/bin/sh\necho planted\n")
	cmd := exec.Command(bin, "pathonly")
	cmd.Dir = malicious
	cmd.Env = []string{"PATH=" + malicious, "NOISE_LIBEXEC_ROOT=" + malicious}
	stdout, stderr, code := collectProcess(cmd)
	if code != 127 || stdout != "" || stderr != "noise: plugin not installed: pathonly\n" {
		t.Fatalf("fixed-root refusal = code %d stdout %q stderr %q", code, stdout, stderr)
	}

	missingRootBin := filepath.Join(base, "noise-missing-root")
	missingRoot := filepath.Join(base, "absent")
	buildCommand(t, missingRootBin, "./cmd/noise", missingRoot, "")
	assertExitContains(t, exec.Command(missingRootBin, "plugins"), 1, missingRoot)
	rootFile := filepath.Join(base, "root-file")
	if err := os.WriteFile(rootFile, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	fileRootBin := filepath.Join(base, "noise-file-root")
	buildCommand(t, fileRootBin, "./cmd/noise", rootFile, "")
	assertExitContains(t, exec.Command(fileRootBin, "plugins"), 1, "not a directory")
}

func TestRealProcessUnsupportedStillProvidesHelpAndVersion(t *testing.T) {
	base := t.TempDir()
	bin := filepath.Join(base, "noise")
	buildCommand(t, bin, "./cmd/noise", filepath.Join(base, "missing"), "plan9")
	stdout, stderr, code := collectProcess(exec.Command(bin, "--help"))
	if code != 0 || !strings.Contains(stdout, "usage: noise") || stderr != "" {
		t.Fatalf("unsupported help = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	stdout, stderr, code = collectProcess(exec.Command(bin, "version"))
	if code != 0 || stdout != "noise "+testVersion+"\n" || stderr != "" {
		t.Fatalf("unsupported version = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	assertExit(t, exec.Command(bin, "plugins"), 1, "noise: unsupported platform: plan9\n")
	shim := filepath.Join(base, "noisetask")
	buildCommand(t, shim, "./cmd/noisetask", filepath.Join(base, "missing"), "plan9")
	assertExit(t, exec.Command(shim, "version"), 1,
		warningLine+"noisetask: unsupported platform: plan9\n")
}

func TestActualTaskPluginInvocationFormsAndProjectContext(t *testing.T) {
	installation := buildTaskInstallation(t)

	directOut, directErr, directCode := collectProcess(exec.Command(installation.noiseTasks, "version"))
	hostOut, hostErr, hostCode := collectProcess(exec.Command(installation.noise, "tasks", "version"))
	shimOut, shimErr, shimCode := collectProcess(exec.Command(installation.shim, "version"))
	if directCode != 0 || hostCode != 0 || shimCode != 0 ||
		directOut != "noise-tasks "+testVersion+"\n" || hostOut != directOut || shimOut != directOut ||
		directErr != "" || hostErr != "" || shimErr != warningLine {
		t.Fatalf("version forms: direct=(%d,%q,%q) host=(%d,%q,%q) shim=(%d,%q,%q)",
			directCode, directOut, directErr, hostCode, hostOut, hostErr, shimCode, shimOut, shimErr)
	}
	directOut, directErr, directCode = collectProcess(exec.Command(installation.noiseTasks, "--help"))
	hostOut, hostErr, hostCode = collectProcess(exec.Command(installation.noise, "tasks", "--help"))
	if directCode != 0 || hostCode != 0 || directOut != hostOut || directErr != "" || hostErr != "" ||
		!strings.Contains(directOut, "usage: noise-tasks") {
		t.Fatalf("plugin help forms: direct=(%d,%q,%q) host=(%d,%q,%q)",
			directCode, directOut, directErr, hostCode, hostOut, hostErr)
	}

	for _, inGit := range []bool{true, false} {
		t.Run(map[bool]string{true: "inside-git", false: "outside-git"}[inGit], func(t *testing.T) {
			project := t.TempDir()
			cwd := project
			if inGit {
				if err := os.Mkdir(filepath.Join(project, ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
				initStoreProcess(t, installation, project)
				cwd = filepath.Join(project, "nested")
				if err := os.Mkdir(cwd, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			direct := exec.Command(installation.noiseTasks, "list", "--json")
			direct.Dir = cwd
			host := exec.Command(installation.noise, "tasks", "list", "--json")
			host.Dir = cwd
			dOut, dErr, dCode := collectProcess(direct)
			hOut, hErr, hCode := collectProcess(host)
			if dOut != hOut || dErr != hErr || dCode != hCode {
				t.Fatalf("context mismatch direct=(%d,%q,%q) host=(%d,%q,%q)",
					dCode, dOut, dErr, hCode, hOut, hErr)
			}
			if inGit && (dCode != 0 || dErr != "") {
				t.Fatalf("initialized list = (%d,%q,%q)", dCode, dOut, dErr)
			}
			if !inGit && (dCode != 1 || !strings.Contains(dErr, "project is not initialized")) {
				t.Fatalf("uninitialized list = (%d,%q,%q)", dCode, dOut, dErr)
			}
		})
	}

	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	initStoreProcess(t, installation, project)
	elsewhere := t.TempDir()
	env := append(os.Environ(), "NOISE_PROJECT_DIR="+project)
	direct := exec.Command(installation.noiseTasks, "list", "--json")
	direct.Dir, direct.Env = elsewhere, env
	host := exec.Command(installation.noise, "tasks", "list", "--json")
	host.Dir, host.Env = elsewhere, env
	dOut, dErr, dCode := collectProcess(direct)
	hOut, hErr, hCode := collectProcess(host)
	if dCode != 0 || hCode != 0 || dOut != hOut || dErr != hErr {
		t.Fatalf("explicit project context mismatch direct=(%d,%q,%q) host=(%d,%q,%q)", dCode, dOut, dErr, hCode, hOut, hErr)
	}
}

func TestRealProcessProjectSetupLifecycle(t *testing.T) {
	installation := buildTaskInstallation(t)
	forms := []struct {
		name string
		argv []string
	}{
		{name: "direct", argv: []string{installation.noiseTasks}},
		{name: "router", argv: []string{installation.noise, "tasks"}},
	}

	for _, form := range forms {
		t.Run(form.name+"-create-rerun", func(t *testing.T) {
			project := setupIntegrationProject(t)
			cwd := filepath.Join(project, "nested")
			if err := os.Mkdir(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			args := []string{"setup", "--adapter", "anthropic-api", "--model", "test/model", "--yes", "--json"}
			cmd := exec.Command(form.argv[0], append(form.argv[1:], args...)...)
			cmd.Dir, cmd.Env = cwd, contextTestEnvironment()
			stdout, stderr, code := collectProcess(cmd)
			if code != 0 || stderr != "" {
				t.Fatalf("create = code %d stdout %q stderr %q", code, stdout, stderr)
			}
			result := decodeSetupProcess(t, stdout)
			wantStorePath := filepath.Join(project, ".git", "noise-tasks", "tasks.sqlite")
			if result.SchemaVersion != 2 || result.ProjectBoundary != project ||
				result.StorePath != wantStorePath || result.PreferencesPath != ".tasks/preferences.json" ||
				result.StoreAction != "created" || result.PreferencesAction != "created" ||
				result.Generation.Adapter != "anthropic-api" || result.Generation.Model != "test/model" ||
				!result.AdapterAvailable || result.AuthenticationVerified {
				t.Fatalf("create result = %+v", result)
			}
			preferencesPath := filepath.Join(project, ".tasks", "preferences.json")
			before, err := os.ReadFile(preferencesPath)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(preferencesPath)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o644 {
				t.Fatalf("preferences mode = %v", info.Mode().Perm())
			}

			cmd = exec.Command(form.argv[0], append(form.argv[1:], "setup", "--json")...)
			cmd.Dir, cmd.Env = cwd, contextTestEnvironment()
			stdout, stderr, code = collectProcess(cmd)
			if code != 0 || stderr != "" {
				t.Fatalf("rerun = code %d stdout %q stderr %q", code, stdout, stderr)
			}
			result = decodeSetupProcess(t, stdout)
			if result.StoreAction != "unchanged" || result.PreferencesAction != "unchanged" {
				t.Fatalf("rerun result = %+v", result)
			}
			after, err := os.ReadFile(preferencesPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("rerun changed preferences: err=%v", err)
			}
		})
	}

	t.Run("pty-prompt-preview-confirm", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("util-linux script invocation is Linux-specific")
		}
		script, err := exec.LookPath("script")
		if err != nil {
			t.Skip("script utility is unavailable")
		}
		project := setupIntegrationProject(t)
		cmd := exec.Command(script, "-q", "-e", "-f", "-c", `"$NOISE_TASKS_TEST_BIN" setup`, os.DevNull)
		cmd.Dir = project
		cmd.Env = append(contextTestEnvironment(), "NOISE_TASKS_TEST_BIN="+installation.noiseTasks)
		cmd.Stdin = strings.NewReader("1\npty-model\nyes\n")
		stdout, stderr, code := collectProcess(cmd)
		if code != 0 || stderr != "" || !strings.Contains(stdout, "available generation adapters:") ||
			!strings.Contains(stdout, "selected generation preferences:") ||
			!strings.Contains(stdout, "write these generation preferences?") ||
			!strings.Contains(stdout, "preferences .tasks/preferences.json: created") {
			t.Fatalf("PTY setup = code %d stdout %q stderr %q", code, stdout, stderr)
		}
		if _, err := os.Stat(filepath.Join(project, ".tasks", "preferences.json")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("repair-preferences-only-half-state", func(t *testing.T) {
		project := setupIntegrationProject(t)
		state := filepath.Join(project, ".tasks")
		if err := os.Mkdir(state, 0o755); err != nil {
			t.Fatal(err)
		}
		preferencesPath := filepath.Join(state, "preferences.json")
		const document = "{\n  \"schemaVersion\": 1,\n  \"generation\": {\n    \"adapter\": \"anthropic-api\",\n    \"model\": \"repair-model\"\n  }\n}\n"
		if err := os.WriteFile(preferencesPath, []byte(document), 0o640); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(installation.noise, "tasks", "setup", "--json")
		cmd.Dir, cmd.Env = project, contextTestEnvironment()
		stdout, stderr, code := collectProcess(cmd)
		if code != 0 || stderr != "" {
			t.Fatalf("repair = code %d stdout %q stderr %q", code, stdout, stderr)
		}
		result := decodeSetupProcess(t, stdout)
		if result.StoreAction != "created" || result.PreferencesAction != "unchanged" {
			t.Fatalf("repair result = %+v", result)
		}
		got, err := os.ReadFile(preferencesPath)
		if err != nil || string(got) != document {
			t.Fatalf("repair rewrote preferences: %q, %v", got, err)
		}
		if _, err := os.Stat(filepath.Join(project, ".git", "noise-tasks", "tasks.sqlite")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("malformed-preferences-fail-closed", func(t *testing.T) {
		project := setupIntegrationProject(t)
		state := filepath.Join(project, ".tasks")
		if err := os.Mkdir(state, 0o755); err != nil {
			t.Fatal(err)
		}
		preferencesPath := filepath.Join(state, "preferences.json")
		const malformed = `{"schemaVersion":2,"generation":{"adapter":"anthropic-api","model":"m"}}`
		if err := os.WriteFile(preferencesPath, []byte(malformed), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(installation.noiseTasks, "setup", "--json")
		cmd.Dir, cmd.Env = project, contextTestEnvironment()
		stdout, stderr, code := collectProcess(cmd)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "invalid preferences") {
			t.Fatalf("refusal = code %d stdout %q stderr %q", code, stdout, stderr)
		}
		got, err := os.ReadFile(preferencesPath)
		if err != nil || string(got) != malformed {
			t.Fatalf("refusal changed preferences: %q, %v", got, err)
		}
		if _, err := os.Lstat(filepath.Join(project, ".git", "noise-tasks", "tasks.sqlite")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refusal created store state: %v", err)
		}
	})
}

func TestRealProcessConcurrentProjectSetup(t *testing.T) {
	installation := buildTaskInstallation(t)
	for _, test := range []struct {
		name   string
		models []string
	}{
		{name: "identical", models: []string{"same", "same"}},
		{name: "conflicting", models: []string{"left", "right"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := setupIntegrationProject(t)

			// The store's own immediate transaction serializes the children;
			// there is no project lock for the parent to hold anymore.
			// Scheduling decides what the losing child observes: when its
			// preview still sees no preferences it is refused as a concurrent
			// update; when it starts after the winner published, its preview
			// already matches the on-disk state and the run is a legitimate
			// rerun, which §2.3 allows to update the preferences. The
			// deterministic barrier variant of this scenario is covered
			// in-process by cmd/noise-tasks' setup tests; here only the
			// scheduling-independent outcome is asserted.
			children := make([]*setupChildProcess, 0, len(test.models))
			for i, model := range test.models {
				argv := []string{installation.noiseTasks, "setup", "--adapter", "anthropic-api", "--model", model, "--yes", "--json"}
				if i%2 == 1 {
					argv = []string{installation.noise, "tasks", "setup", "--adapter", "anthropic-api", "--model", model, "--yes", "--json"}
				}
				entry := &setupChildProcess{cmd: exec.Command(argv[0], argv[1:]...), model: model, waitResult: make(chan error, 1)}
				entry.cmd.Dir, entry.cmd.Env = project, contextTestEnvironment()
				entry.cmd.Stdout, entry.cmd.Stderr = &entry.stdout, &entry.stderr
				if err := entry.cmd.Start(); err != nil {
					t.Fatal(err)
				}
				go func(entry *setupChildProcess) { entry.waitResult <- entry.cmd.Wait() }(entry)
				children = append(children, entry)
			}

			created, unchanged, updated, refused := 0, 0, 0, 0
			for _, entry := range children {
				select {
				case err := <-entry.waitResult:
					code := processExitCode(t, err)
					switch code {
					case 0:
						result := decodeSetupProcess(t, entry.stdout.String())
						switch result.PreferencesAction {
						case "created":
							created++
						case "unchanged":
							unchanged++
						case "updated":
							updated++
						default:
							t.Fatalf("unexpected success action: %+v", result)
						}
						if entry.stderr.Len() != 0 {
							t.Fatalf("success stderr = %q", entry.stderr.String())
						}
					case 1:
						refused++
						if entry.stdout.Len() != 0 || !strings.Contains(entry.stderr.String(), "changed concurrently") {
							t.Fatalf("conflict refusal stdout=%q stderr=%q", entry.stdout.String(), entry.stderr.String())
						}
					default:
						t.Fatalf("child %q exit %d stdout=%q stderr=%q", entry.model, code, entry.stdout.String(), entry.stderr.String())
					}
				case <-time.After(30 * time.Second):
					_ = entry.cmd.Process.Kill()
					t.Fatal("setup child did not exit")
				}
			}
			if test.name == "identical" && (created != 1 || unchanged != 1 || updated != 0 || refused != 0) {
				t.Fatalf("identical actions: created=%d unchanged=%d updated=%d refused=%d", created, unchanged, updated, refused)
			}
			if test.name == "conflicting" && (created != 1 || unchanged != 0 || updated+refused != 1) {
				t.Fatalf("conflicting actions: created=%d unchanged=%d updated=%d refused=%d", created, unchanged, updated, refused)
			}
			published := decodeSetupPreferences(t, project)
			if published != test.models[0] && published != test.models[1] {
				t.Fatalf("published model = %q, want one of %v", published, test.models)
			}
		})
	}
}

func setupIntegrationProject(t *testing.T) string {
	t.Helper()
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return project
}

func decodeSetupProcess(t *testing.T, stdout string) setupProcessResult {
	t.Helper()
	var result setupProcessResult
	decoder := json.NewDecoder(strings.NewReader(stdout))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("decode setup result: %v\n%s", err, stdout)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("trailing setup JSON: %v\n%s", err, stdout)
	}
	return result
}

// decodeSetupPreferences reads the model setup published to the project's
// .tasks/preferences.json.
func decodeSetupPreferences(t *testing.T, project string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(project, ".tasks", "preferences.json"))
	if err != nil {
		t.Fatalf("read published preferences: %v", err)
	}
	var document struct {
		Generation struct {
			Model string `json:"model"`
		} `json:"generation"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode published preferences: %v\n%s", err, raw)
	}
	return document.Generation.Model
}

func processExitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	t.Fatalf("wait for setup child: %v", err)
	return -1
}

func TestNestedSubtaskInvocationForms(t *testing.T) {
	installation := buildTaskInstallation(t)
	forms := []struct {
		name        string
		argv        []string
		wantWarning string
	}{
		{name: "direct", argv: []string{installation.noiseTasks}},
		{name: "host", argv: []string{installation.noise, "tasks"}},
		{name: "shim", argv: []string{installation.shim}, wantWarning: warningLine},
	}
	const content = `{"master":{"tasks":[{"id":"32","title":"Diagnostics","status":"in-progress","dependencies":[],"subtasks":[{"id":1,"title":"Health probes","status":"pending","dependencies":[],"parentTaskId":32,"_custom":true},{"id":"2","title":"Obsolete leaf","status":"pending","dependencies":[],"parentTaskId":"32"}],"updatedAt":"2026-08-02T00:00:00Z"}],"metadata":{"version":"1.0.0","lastModified":"2026-08-02T00:00:00Z","taskCount":1,"completedCount":0,"tags":["master"]}}}`

	for _, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tasks.json")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			showArgs := append(append([]string{}, form.argv...), "show", "--file", path, "--tag", "master", "--json", "32.1")
			stdout, stderr, code := collectProcess(exec.Command(showArgs[0], showArgs[1:]...))
			if code != 0 || stderr != form.wantWarning {
				t.Fatalf("show = code %d stdout %q stderr %q", code, stdout, stderr)
			}
			var shown struct {
				ID           string `json:"id"`
				ParentTaskID string `json:"parentTaskId"`
				Status       string `json:"status"`
			}
			if err := json.Unmarshal([]byte(stdout), &shown); err != nil {
				t.Fatal(err)
			}
			if shown.ID != "32.1" || shown.ParentTaskID != "32" || shown.Status != "pending" {
				t.Fatalf("show output = %+v", shown)
			}

			setArgs := append(append([]string{}, form.argv...), "set-status", "--file", path, "--tag", "master", "--json", "32.1", "done")
			stdout, stderr, code = collectProcess(exec.Command(setArgs[0], setArgs[1:]...))
			if code != 0 || stderr != form.wantWarning || !strings.Contains(stdout, `"previous": "pending"`) {
				t.Fatalf("set-status = code %d stdout %q stderr %q", code, stdout, stderr)
			}

			removeArgs := append(append([]string{}, form.argv...), "remove", "--file", path, "--tag", "master", "--json", "32.2")
			stdout, stderr, code = collectProcess(exec.Command(removeArgs[0], removeArgs[1:]...))
			if code != 0 || stderr != form.wantWarning || !strings.Contains(stdout, `"id": "32.2"`) {
				t.Fatalf("remove = code %d stdout %q stderr %q", code, stdout, stderr)
			}

			var stored struct {
				Master struct {
					Tasks []struct {
						Status    string `json:"status"`
						UpdatedAt string `json:"updatedAt"`
						Subtasks  []struct {
							Status string `json:"status"`
						} `json:"subtasks"`
					} `json:"tasks"`
				} `json:"master"`
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &stored); err != nil {
				t.Fatal(err)
			}
			task := stored.Master.Tasks[0]
			if task.Status != "in-progress" || task.UpdatedAt != "2026-08-02T00:00:00Z" || len(task.Subtasks) != 1 || task.Subtasks[0].Status != "done" {
				t.Fatalf("stored parent/subtask = %+v", task)
			}
		})
	}
}

func TestGenerateInvocationFormsAndAliasAreExact(t *testing.T) {
	installation := buildTaskInstallation(t)
	project := t.TempDir()
	backlogPath := writeBacklog(t, project)
	prd := filepath.Join(project, "PRD.md")
	if err := os.WriteFile(prd, []byte("# Test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	baseArgs := []string{"--file", backlogPath, "--yes", prd}
	forms := []struct {
		name string
		argv []string
		shim bool
	}{
		{"direct", []string{installation.noiseTasks}, false},
		{"host", []string{installation.noise, "tasks"}, false},
		{"shim", []string{installation.shim}, true},
	}
	type result struct {
		stdout, stderr string
		code           int
	}
	environments := []struct {
		name string
		env  []string
	}{
		{"canonical", []string{
			"NOISE_PROJECT_DIR=" + project,
			"NOISE_TASKS_MODEL=preserved",
			"NOISE_TASKS_PROVIDER=invalid",
			"NOISETASK_PROVIDER=",
		}},
		{"compat", []string{
			"NOISE_PROJECT_DIR=" + project,
			"NOISE_TASKS_MODEL=preserved",
			"NOISE_TASKS_PROVIDER=",
			"NOISETASK_PROVIDER=invalid",
		}},
	}
	baselineCmd := exec.Command(installation.noiseTasks, append([]string{"generate"}, baseArgs...)...)
	baselineCmd.Env = environments[0].env
	bOut, bErr, bCode := collectProcess(baselineCmd)
	baseline := result{bOut, bErr, bCode}
	if baseline.code != 1 || baseline.stdout != "" ||
		!strings.Contains(baseline.stderr, `unknown NOISE_TASKS_PROVIDER "invalid"`) {
		t.Fatalf("canonical refusal = %+v", baseline)
	}
	for _, environment := range environments {
		for _, f := range forms {
			for _, command := range []string{"generate", "parse-prd"} {
				argv := append(append([]string{}, f.argv...), command)
				cmd := exec.Command(argv[0], append(argv[1:], baseArgs...)...)
				cmd.Env = environment.env
				out, errOut, code := collectProcess(cmd)
				got := result{out, errOut, code}
				want := baseline
				if f.shim {
					want.stderr = warningLine + baseline.stderr
				}
				if got != want {
					t.Errorf("%s/%s/%s = %+v, want %+v", environment.name, f.name, command, got, want)
				}
			}
		}
	}
	after, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(after, []byte(`"tasks":[]`)) {
		t.Fatal("refused generate forms mutated the backlog")
	}
}

func TestRealRouterGenerateWithConfiguredOMP(t *testing.T) {
	installation := buildTaskInstallation(t)
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	argsPath := filepath.Join(t.TempDir(), "args")
	stdinPath := filepath.Join(t.TempDir(), "stdin")
	cwdPath := filepath.Join(t.TempDir(), "cwd")
	omp := filepath.Join(binDir, "omp")
	script := `#!/bin/sh
printf '<%s>\n' "$@" > "` + argsPath + `"
pwd > "` + cwdPath + `"
cat > "` + stdinPath + `"
printf '%s' '[{"title":"Build widget","description":"Implement the widget.","details":"Follow the PRD.","priority":"high","dependencies":[]}]'
`
	if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runtimeEnv := []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"NOISE_TASKS_PROVIDER=", "NOISETASK_PROVIDER=",
		"NOISE_TASKS_MODEL=", "NOISETASK_MODEL=",
		"NOISE_TASKS_CLAUDE_EXECUTABLE=", "NOISETASK_CLAUDE_EXECUTABLE=",
	}
	setup := exec.Command(installation.noise, "tasks", "setup", "--adapter", "omp", "--model", "zai/glm-5.2", "--yes", "--json")
	setup.Dir = project
	setup.Env = runtimeEnv
	setupOut, setupErr, setupCode := collectProcess(setup)
	if setupCode != 0 || setupErr != "" || !strings.Contains(setupOut, `"preferencesAction": "created"`) {
		t.Fatalf("router setup = code %d stdout %q stderr %q", setupCode, setupOut, setupErr)
	}
	prd := filepath.Join(project, "PRD.md")
	if err := os.WriteFile(prd, []byte("# Toy project\n\nBuild a reliable widget.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(installation.noise, "tasks", "generate", "--json", "--yes", "--num-tasks", "1", prd)
	command.Dir = project
	command.Env = runtimeEnv
	stdout, stderr, code := collectProcess(command)
	if code != 0 || stderr != "" {
		t.Fatalf("router generate = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	var output struct {
		SchemaVersion int `json:"schemaVersion"`
		Generation    struct {
			Adapter        string `json:"adapter"`
			RequestedModel string `json:"requestedModel"`
			ResolvedModel  string `json:"resolvedModel"`
		} `json:"generation"`
		Written bool `json:"written"`
		Drafts  []struct {
			ID string `json:"id"`
		} `json:"drafts"`
	}
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout)
	}
	if output.SchemaVersion != 1 || output.Generation.Adapter != "omp" ||
		output.Generation.RequestedModel != "zai/glm-5.2" || output.Generation.ResolvedModel != "" ||
		!output.Written || len(output.Drafts) != 1 || output.Drafts[0].ID != "1" {
		t.Fatalf("output = %+v", output)
	}
	wantArgs := "<-p>\n<--mode>\n<text>\n<--no-tools>\n<--no-session>\n<--no-skills>\n<--no-rules>\n<--no-extensions>\n<--model>\n<zai/glm-5.2>"
	args, err := os.ReadFile(argsPath)
	if err != nil || strings.TrimSpace(string(args)) != wantArgs {
		t.Fatalf("OMP argv = %q, %v; want %q", args, err, wantArgs)
	}
	stdin, err := os.ReadFile(stdinPath)
	if err != nil || !strings.Contains(string(stdin), "Build a reliable widget") ||
		!strings.Contains(string(stdin), "STRICT JSON") {
		t.Fatalf("OMP stdin = %q, %v", stdin, err)
	}
	cwd, err := os.ReadFile(cwdPath)
	if err != nil || strings.TrimSpace(string(cwd)) != project {
		t.Fatalf("OMP cwd = %q, %v; want %q", cwd, err, project)
	}
	list := exec.Command(installation.noiseTasks, "list", "--json")
	list.Dir = project
	stored, stderr, code := collectProcess(list)
	if code != 0 || stderr != "" || !strings.Contains(stored, `"title": "Build widget"`) {
		t.Fatalf("toy store list = %q, %q, %d", stored, stderr, code)
	}
}

func TestConcurrentTaskProcessesDoNotLoseAdds(t *testing.T) {
	installation := buildTaskInstallation(t)
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// §8.1 gives fixtures atomic writes but no cross-process lock, so the
	// no-lost-adds guarantee lives in the store, which serializes writers.
	initStoreProcess(t, installation, project)
	const writers = 24
	start := make(chan struct{})
	failures := make(chan string, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			cmd := exec.Command(installation.noiseTasks,
				"add", "--title", fmt.Sprintf("writer-%02d", i))
			cmd.Dir = project
			stdout, stderr, code := collectProcess(cmd)
			if code != 0 {
				failures <- fmt.Sprintf("writer %d: code=%d stdout=%q stderr=%q", i, code, stdout, stderr)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}

	list := exec.Command(installation.noiseTasks, "list", "--json")
	list.Dir = project
	stdout, stderr, code := collectProcess(list)
	if code != 0 {
		t.Fatalf("list after concurrent adds: code=%d stderr=%q", code, stderr)
	}
	var out struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tasks) != writers {
		t.Fatalf("got %d tasks after %d concurrent processes", len(out.Tasks), writers)
	}
}

func buildProbeInstallation(t *testing.T) builtInstallation {
	t.Helper()
	base := t.TempDir()
	stage := filepath.Join(base, "root.stage")
	root := filepath.Join(base, "root")
	binDir := filepath.Join(base, "bin")
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	helper := buildProbeHelper(t, base)
	if err := os.Link(helper, filepath.Join(stage, "noise-tasks")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(helper, filepath.Join(stage, "noise-alpha")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(helper, filepath.Join(stage, "noise-zed")); err != nil {
		t.Fatal(err)
	}
	installation := builtInstallation{
		root:  root,
		noise: filepath.Join(binDir, "noise"),
		shim:  filepath.Join(binDir, "noisetask"),
	}
	buildCommand(t, installation.noise, "./cmd/noise", root, "")
	buildCommand(t, installation.shim, "./cmd/noisetask", root, "")
	publishTestRoot(t, stage, root)
	return installation
}

func buildTaskInstallation(t *testing.T) builtInstallation {
	t.Helper()
	base := t.TempDir()
	stage := filepath.Join(base, "root.stage")
	root := filepath.Join(base, "root")
	binDir := filepath.Join(base, "bin")
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	installation := builtInstallation{
		root:       root,
		noise:      filepath.Join(binDir, "noise"),
		noiseTasks: filepath.Join(binDir, "noise-tasks"),
		shim:       filepath.Join(binDir, "noisetask"),
	}
	plugin := filepath.Join(stage, "noise-tasks")
	buildCommand(t, plugin, "./cmd/noise-tasks", root, "")
	if err := os.Link(plugin, installation.noiseTasks); err != nil {
		t.Fatal(err)
	}
	buildCommand(t, installation.noise, "./cmd/noise", root, "")
	buildCommand(t, installation.shim, "./cmd/noisetask", root, "")
	publishTestRoot(t, stage, root)
	return installation
}

func buildCommand(t *testing.T, output, packagePath, root, platform string) {
	t.Helper()
	ldflags := "-X " + modulePath + "/internal/buildinfo.Version=" + testVersion +
		" -X " + modulePath + "/internal/libexec.Root=" + root
	if platform != "" {
		ldflags += " -X " + modulePath + "/internal/libexec.Platform=" + platform
	}
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"),
		"build", "-ldflags", ldflags, "-o", output, packagePath)
	cmd.Dir = moduleRoot(t)
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", packagePath, err, out)
	}
}

func buildProbeHelper(t *testing.T, base string) string {
	t.Helper()
	source := filepath.Join(base, "probe.go")
	program := `package main
import (
 "encoding/json"
 "fmt"
 "io"
 "os"
 "strconv"
 "time"
)
func main() {
 if len(os.Args) < 2 { os.Exit(2) }
 switch os.Args[1] {
 case "dump":
  input, _ := io.ReadAll(os.Stdin)
  cwd, _ := os.Getwd()
  _ = json.NewEncoder(os.Stdout).Encode(struct {
   Args []string ` + "`json:\"args\"`" + `
   Env []string ` + "`json:\"env\"`" + `
   CWD string ` + "`json:\"cwd\"`" + `
   Stdin string ` + "`json:\"stdin\"`" + `
  }{os.Args, os.Environ(), cwd, string(input)})
  fmt.Fprintln(os.Stderr, "probe-stderr")
 case "exit":
  code, _ := strconv.Atoi(os.Args[2]); os.Exit(code)
 case "block":
  fmt.Printf("ready %d\n", os.Getpid())
  for { time.Sleep(time.Hour) }
 default: os.Exit(2)
 }
}
`
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(base, "probe")
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", output, source)
	cmd.Dir = base
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build probe: %v\n%s", err, out)
	}
	return output
}

func publishTestRoot(t *testing.T, stage, root string) {
	t.Helper()
	entries, err := os.ReadDir(stage)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		file, err := os.Open(filepath.Join(stage, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		_ = file.Close()
	}
	dir, err := os.Open(stage)
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		t.Fatal(err)
	}
	_ = dir.Close()
	if err := os.Rename(stage, root); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Sync(); err != nil {
		_ = parent.Close()
		t.Fatal(err)
	}
	_ = parent.Close()
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func collectProcess(cmd *exec.Cmd) (stdout, stderr string, code int) {
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if err == nil {
		return out.String(), errOut.String(), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out.String(), errOut.String(), exitErr.ExitCode()
	}
	return out.String(), errOut.String(), -1
}

func decodeProbe(t *testing.T, data string) probeOutput {
	t.Helper()
	var got probeOutput
	if err := json.Unmarshal([]byte(data), &got); err != nil {
		t.Fatalf("decode probe output: %v\n%s", err, data)
	}
	return got
}

func assertExit(t *testing.T, cmd *exec.Cmd, wantCode int, wantStderr string) {
	t.Helper()
	stdout, stderr, code := collectProcess(cmd)
	if code != wantCode || stdout != "" || stderr != wantStderr {
		t.Fatalf("process = code %d stdout %q stderr %q; want %d, empty, %q", code, stdout, stderr, wantCode, wantStderr)
	}
}

func assertExitContains(t *testing.T, cmd *exec.Cmd, wantCode int, text string) {
	t.Helper()
	stdout, stderr, code := collectProcess(cmd)
	if code != wantCode || stdout != "" || !strings.Contains(stderr, text) {
		t.Fatalf("process = code %d stdout %q stderr %q; want %d and %q", code, stdout, stderr, wantCode, text)
	}
}

func writeProbeScript(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeBacklog(t *testing.T, project string) string {
	t.Helper()
	dir := filepath.Join(project, ".tasks/tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "tasks.json")
	content := `{"main":{"tasks":[],"metadata":{"version":"1.0.0","lastModified":"2026-08-08T00:00:00Z","taskCount":0,"completedCount":0,"tags":["main"]}}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// initStoreProcess initializes the project task store through the built
// noise-tasks binary (the real-process counterpart of store.Create).
func initStoreProcess(t *testing.T, installation builtInstallation, project string) {
	t.Helper()
	cmd := exec.Command(installation.noiseTasks, "init")
	cmd.Dir = project
	stdout, stderr, code := collectProcess(cmd)
	if code != 0 || stderr != "" {
		t.Fatalf("init = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
