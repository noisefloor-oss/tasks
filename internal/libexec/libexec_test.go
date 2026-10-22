package libexec

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestValidNoun(t *testing.T) {
	valid := []string{"tasks", "a", "a1", "task-tools", "1-2"}
	for _, noun := range valid {
		if !ValidNoun(noun) {
			t.Errorf("ValidNoun(%q) = false", noun)
		}
	}
	invalid := []string{
		"", "Tasks", "-tasks", "tasks-", "task--tools", "task_tools",
		"tasks/more", `tasks\\more`, ".", "..", "/tasks", "noise-tasks",
		"help", "version", "plugins",
	}
	for _, noun := range invalid {
		if ValidNoun(noun) {
			t.Errorf("ValidNoun(%q) = true", noun)
		}
	}
}

func TestResolveAndDiscover(t *testing.T) {
	base := t.TempDir()
	versioned := filepath.Join(base, "root-v1")
	if err := os.Mkdir(versioned, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(versioned, "noise-zed"))
	writeExecutable(t, filepath.Join(versioned, "noise-alpha"))
	writeExecutable(t, filepath.Join(versioned, "noise-help"))
	if err := os.WriteFile(filepath.Join(versioned, "noise-noexec"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(versioned, "noise-directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(base, "escape")
	writeExecutable(t, escape)
	if err := os.Symlink(escape, filepath.Join(versioned, "noise-escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(versioned, "noise-broken")); err != nil {
		t.Fatal(err)
	}
	rootLink := filepath.Join(base, "published")
	if err := os.Symlink(versioned, rootLink); err != nil {
		t.Fatal(err)
	}

	withRoot(t, rootLink)
	target, err := Resolve("alpha")
	if err != nil {
		t.Fatalf("Resolve(alpha): %v", err)
	}
	if target != filepath.Join(versioned, "noise-alpha") {
		t.Fatalf("target = %q", target)
	}
	if _, err := Resolve("missing"); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("missing error = %v", err)
	}
	for _, noun := range []string{"noexec", "directory", "escape", "broken"} {
		if _, err := Resolve(noun); !errors.Is(err, ErrUnsafe) {
			t.Errorf("Resolve(%s) error = %v, want ErrUnsafe", noun, err)
		}
	}
	nouns, err := Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if want := []string{"alpha", "zed"}; !reflect.DeepEqual(nouns, want) {
		t.Errorf("Discover = %v, want %v", nouns, want)
	}
}

func TestCanonicalRootInstallationFailures(t *testing.T) {
	original := Root
	t.Cleanup(func() { Root = original })
	for _, root := range []string{"relative", "/tmp/../tmp/noise-unclean"} {
		Root = root
		if _, err := CanonicalRoot(); !errors.Is(err, ErrInstallation) {
			t.Errorf("CanonicalRoot(%q) error = %v", root, err)
		}
	}
	Root = filepath.Join(t.TempDir(), "missing")
	if _, err := CanonicalRoot(); !errors.Is(err, ErrInstallation) {
		t.Errorf("missing root error = %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	Root = file
	if _, err := CanonicalRoot(); !errors.Is(err, ErrInstallation) {
		t.Errorf("file root error = %v", err)
	}
}

func TestProjectEnvironment(t *testing.T) {
	dir := t.TempDir()
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })

	got, err := ProjectEnvironment([]string{"A=one", "NOISE_PROJECT_DIR=", "B=two"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A=one", "B=two", "NOISE_PROJECT_DIR=" + dir}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("environment = %#v, want %#v", got, want)
	}

	got, err = ProjectEnvironment([]string{
		"NOISE_PROJECT_DIR=", "A=one", "NOISE_PROJECT_DIR=/chosen", "NOISE_PROJECT_DIR=/ignored",
	})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"A=one", "NOISE_PROJECT_DIR=/chosen"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deduplicated environment = %#v, want %#v", got, want)
	}
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func withRoot(t *testing.T, root string) {
	t.Helper()
	original := Root
	Root = root
	t.Cleanup(func() { Root = original })
}

func TestValidNounASCIIOnly(t *testing.T) {
	for _, noun := range []string{"tásks", "任务", strings.Repeat("a", 4) + "—x"} {
		if ValidNoun(noun) {
			t.Errorf("ValidNoun(%q) accepted non-ASCII input", noun)
		}
	}
}
