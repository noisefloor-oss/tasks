package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/libexec"
)

// §9.5 exact confined diagnostics through the CLI surface (unprivileged):
// the pinned confinement line when no broker resolves, and the refusal line
// when a broker answers no. The legacy `worktree create` and
// `claim --worktree` vehicles were removed by the §9.4 v5 cutover; the
// surviving provision and remove surfaces carry these contracts in
// worktree_provision_test.go and worktree_remove_test.go.

// confineCLIRegistration makes the registration area unwritable.
func confineCLIRegistration(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("confined registration requires an unprivileged uid")
	}
	reg := filepath.Join(dir, ".git", "worktrees")
	if err := os.MkdirAll(reg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(reg, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(reg, 0o755) })
}

// stageCLIBroker publishes a fake broker (or none) under a temp libexec root.
func stageCLIBroker(t *testing.T, script string) {
	t.Helper()
	root := t.TempDir()
	if script != "" {
		if err := os.WriteFile(filepath.Join(root, "noise-worktree-broker"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := libexec.Root
	libexec.Root = root
	t.Cleanup(func() { libexec.Root = old })
}
