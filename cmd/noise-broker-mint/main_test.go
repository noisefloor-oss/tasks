package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noisefloor-oss/tasks/internal/broker"
)

// TestMintLegacyCleanupNullBase exercises the mint gate end to end: the
// §9.5 legacy-cleanup null base (empty string on the wire) passes the
// ValidateBindings gate for prune-registration and mints a parseable
// capability, while the same null base under create is refused.
func TestMintLegacyCleanupNullBase(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "noise-broker-mint")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build mint: %v\n%s", err, out)
	}
	keyPath := filepath.Join(dir, "noise-worktree-broker.key")
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{0x5a}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	mintArgs := func(verb string) []string {
		return []string{
			"-verb", verb,
			"-family", "family-1",
			"-saga", "saga-7",
			"-registration-root", "/repo/.git/worktrees",
			"-owner-boundary", "/repo",
			"-target", "/repo/.worktrees/main-42",
			"-branch", "tasks/main/42",
			"-base-commit", "",
			"-key", keyPath,
		}
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, mintArgs(broker.VerbPruneRegistration)...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("null-base prune-registration mint refused: %v\n%s", err, stderr.String())
	}
	key, err := broker.LoadKey(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatal(err)
	}
	cap, err := broker.Parse(key, stdout.Bytes(), time.Now().Unix())
	if err != nil {
		t.Fatalf("minted capability does not parse: %v", err)
	}
	if cap.Verb != broker.VerbPruneRegistration || cap.BaseCommit != "" {
		t.Fatalf("minted capability = %+v", cap)
	}
	if err := cap.ValidateBindings(); err != nil {
		t.Fatalf("minted legacy-cleanup capability refused: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	cmd = exec.Command(bin, mintArgs(broker.VerbCreate)...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if err == nil || !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("null-base create mint = %v, want exit 2", err)
	}
	if !strings.Contains(stderr.String(), "base commit is invalid") {
		t.Fatalf("null-base create stderr = %q", stderr.String())
	}
}
