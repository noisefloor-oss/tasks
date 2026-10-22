package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/buildinfo"
	"github.com/noisefloor-oss/tasks/internal/libexec"
)

func TestHelpAndVersionDoNotConsultInstallation(t *testing.T) {
	originalRoot, originalVersion := libexec.Root, buildinfo.Version
	libexec.Root = filepath.Join(t.TempDir(), "missing")
	buildinfo.Version = "test-version"
	t.Cleanup(func() {
		libexec.Root = originalRoot
		buildinfo.Version = originalVersion
	})

	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"-h"}} {
		code, stdout, stderr := runHost(args...)
		if code != 0 || !strings.Contains(stdout, "usage: noise") || stderr != "" {
			t.Errorf("help %v = code %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
	for _, args := range [][]string{{"version"}, {"--version"}} {
		code, stdout, stderr := runHost(args...)
		if code != 0 || stdout != "noise test-version\n" || stderr != "" {
			t.Errorf("version %v = code %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
}

func TestUsageErrorsRejectNounsBeforeFilesystemAccess(t *testing.T) {
	original := libexec.Root
	libexec.Root = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { libexec.Root = original })

	for _, args := range [][]string{
		{"help", "extra"}, {"version", "extra"}, {"plugins", "extra"},
		{"Tasks"}, {"noise-tasks"}, {"../tasks"}, {"task--tools"},
	} {
		code, stdout, stderr := runHost(args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, shortUsage) {
			t.Errorf("args %v = code %d stdout %q stderr %q", args, code, stdout, stderr)
		}
		if strings.Contains(stderr, libexec.Root) {
			t.Errorf("args %v consulted configured root: %q", args, stderr)
		}
	}
}

func TestPluginsDeterministicAndSafe(t *testing.T) {
	root := t.TempDir()
	for _, noun := range []string{"zed", "alpha"} {
		if err := os.WriteFile(filepath.Join(root, "noise-"+noun), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "noise-noexec"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "noise-help"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	original := libexec.Root
	libexec.Root = root
	t.Cleanup(func() { libexec.Root = original })

	code, stdout, stderr := runHost("plugins")
	if code != 0 || stdout != "alpha\nzed\n" || stderr != "" {
		t.Fatalf("plugins = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestPluginsAllowsValidEmptyRoot(t *testing.T) {
	original := libexec.Root
	libexec.Root = t.TempDir()
	t.Cleanup(func() { libexec.Root = original })
	code, stdout, stderr := runHost("plugins")
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("empty plugins = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestUnsupportedPlatform(t *testing.T) {
	original := libexec.Platform
	libexec.Platform = "plan9"
	t.Cleanup(func() { libexec.Platform = original })
	for _, args := range [][]string{{"plugins"}, {"tasks"}} {
		code, stdout, stderr := runHost(args...)
		if code != 1 || stdout != "" || stderr != "noise: unsupported platform: plan9\n" {
			t.Errorf("args %v = code %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
}

func runHost(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}
