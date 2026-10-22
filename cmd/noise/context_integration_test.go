//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRealProcessContextInvocationFormsAreReadOnly(t *testing.T) {
	installation := buildTaskInstallation(t)
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	initStoreProcess(t, installation, project)
	// A task title is private state: context must never echo store content.
	secret := "private-backlog-content"
	add := exec.Command(installation.noiseTasks, "add", "--title", secret)
	add.Dir = project
	if stdout, stderr, code := collectProcess(add); code != 0 || stderr != "" {
		t.Fatalf("seed add = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	storePath := filepath.Join(project, ".git", "noise-tasks", "tasks.sqlite")
	before, err := os.Stat(storePath)
	if err != nil {
		t.Fatal(err)
	}

	// A held directory flock is not the store maintenance lock: context
	// takes only the shared, non-blocking maintenance lock beside the
	// database and never blocks on either (the collector kills any
	// invocation that stalls).
	directory, err := os.Open(project)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := syscall.Flock(int(directory.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(directory.Fd()), syscall.LOCK_UN)

	direct := exec.Command(installation.noiseTasks, "context", "--json")
	direct.Dir = project
	direct.Env = contextTestEnvironment()
	directOut, directErr, directCode := collectContextProcess(t, direct)
	if directCode != 0 || directErr != "" {
		t.Fatalf("direct context = code %d stdout %q stderr %q", directCode, directOut, directErr)
	}

	umbrella := exec.Command(installation.noise, "tasks", "context", "--json")
	umbrella.Dir = project
	umbrella.Env = contextTestEnvironment()
	umbrellaOut, umbrellaErr, umbrellaCode := collectContextProcess(t, umbrella)
	if umbrellaCode != 0 || umbrellaErr != "" {
		t.Fatalf("umbrella context = code %d stdout %q stderr %q", umbrellaCode, umbrellaOut, umbrellaErr)
	}
	if directOut != umbrellaOut {
		t.Fatalf("invocation forms differ:\ndirect: %s\numbrella: %s", directOut, umbrellaOut)
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(directOut), &object); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"blocked", "blockedReason", "docsPath", "engineSchemaVersion", "initialized", "preferencesPath", "projectBoundary", "schemaVersion", "start", "storeOwnerBoundary", "storePath", "storeSchemaVersion", "writable"}
	gotKeys := make([]string, 0, len(object))
	for key := range object {
		gotKeys = append(gotKeys, key)
	}
	sort.Strings(gotKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("schema keys = %q, want %q", gotKeys, wantKeys)
	}
	for _, forbidden := range []string{secret, "rootToken", "generation", "inventory"} {
		if strings.Contains(directOut, forbidden) {
			t.Fatalf("context leaked %q: %s", forbidden, directOut)
		}
	}

	after, err := os.Stat(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() != after.Mode() || before.ModTime() != after.ModTime() || before.Size() != after.Size() {
		t.Fatal("context process mutated the task store")
	}
}

func collectContextProcess(t *testing.T, command *exec.Cmd) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		code := 0
		if err != nil {
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) {
				t.Fatal(err)
			}
			code = exitError.ExitCode()
		}
		return stdout.String(), stderr.String(), code
	case <-time.After(3 * time.Second):
		_ = command.Process.Kill()
		<-done
		t.Fatal("context blocked on the held project mutation lock")
		return "", "", -1
	}
}

func contextTestEnvironment() []string {
	environment := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "NOISE_PROJECT_DIR=") {
			environment = append(environment, entry)
		}
	}
	return append(environment, "NOISE_PROJECT_DIR=")
}
