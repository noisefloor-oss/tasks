//go:build unix

package broker

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"

	"testing"
)

// newKeyFile writes a 32-byte key with the given mode into its own
// directory and returns the path.
func newKeyFile(t *testing.T, mode os.FileMode) string {
	t.Helper()
	raw := make([]byte, 32)
	rand.Read(raw)
	path := filepath.Join(t.TempDir(), "noise-worktree-broker.key")
	if err := os.WriteFile(path, raw, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadKeyFileRefusesNonRootOwner proves a key file not owned by root
// is refused. Where the test runs unprivileged its own files are already
// non-root-owned; where it runs as root it re-owns the file to nobody.
func TestLoadKeyFileRefusesNonRootOwner(t *testing.T) {
	path := newKeyFile(t, 0o400)
	if os.Geteuid() == 0 {
		if err := os.Chown(path, 65534, 65534); err != nil {
			t.Skipf("cannot re-own key fixture: %v", err)
		}
	}
	if _, err := LoadKeyFile(path); err == nil || !strings.Contains(err.Error(), "root-owned") {
		t.Fatalf("non-root-owned key accepted: %v", err)
	}
}

// TestLoadKeyFileAcceptsRootOwnedModes proves the deployed modes — 0400
// preferred, 0600 tolerated — load when the file really is root-owned,
// which only the privileged identity can arrange.
func TestLoadKeyFileAcceptsRootOwnedModes(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires euid 0 to own a key as root")
	}
	for _, mode := range []os.FileMode{0o400, 0o600} {
		path := newKeyFile(t, mode)
		if _, err := LoadKeyFile(path); err != nil {
			t.Fatalf("root-owned mode %o key refused: %v", mode, err)
		}
	}
}

// TestLoadKeyFileRefusesSymlink proves the open itself is O_NOFOLLOW: a
// symlink at the key path is refused even when its target is a perfectly
// valid key file.
func TestLoadKeyFileRefusesSymlink(t *testing.T) {
	real := newKeyFile(t, 0o400)
	dir := t.TempDir()
	link := filepath.Join(dir, "noise-worktree-broker.key")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlink unavailable")
	}
	if _, err := LoadKeyFile(link); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("symlinked key accepted: %v", err)
	}
}

// TestLoadKeyFileRefusesBroadPermissions proves exactly owner-only
// permissions load: any group or world bit refuses, as does a key the
// owner cannot read.
func TestLoadKeyFileRefusesBroadPermissions(t *testing.T) {
	for _, mode := range []os.FileMode{0o444, 0o440, 0o404, 0o640, 0o604} {
		path := newKeyFile(t, mode)
		if _, err := LoadKeyFile(path); err == nil || !strings.Contains(err.Error(), "too broad") {
			t.Fatalf("mode %o key accepted: %v", mode, err)
		}
	}
	// A key the owner cannot read is refused at open — still a refusal,
	// just detected one syscall earlier.
	if _, err := LoadKeyFile(newKeyFile(t, 0o000)); err == nil {
		t.Fatal("unreadable mode 000 key accepted")
	}
}

// TestLoadKeyFileRefusesNonRegular proves a directory at the key path —
// the other swap an attacker with write access to the directory might
// attempt — is refused before any read.
func TestLoadKeyFileRefusesNonRegular(t *testing.T) {
	path := t.TempDir()
	if _, err := LoadKeyFile(path); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory key accepted: %v", err)
	}
}

func TestLoadKeyFileRefusesMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.key")
	if _, err := LoadKeyFile(path); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing key accepted: %v", err)
	}
}

// TestLoadKeyFileRefusesWrongSize proves the read is complete and exactly
// bounded: 31 bytes refuse as a key-size error and 33 refuse as oversized.
// The size-specific reasons are only observable as root; unprivileged runs
// still prove the refusal.
func TestLoadKeyFileRefusesWrongSize(t *testing.T) {
	for _, size := range []int{31, 33, 1024} {
		raw := make([]byte, size)
		rand.Read(raw)
		path := filepath.Join(t.TempDir(), "noise-worktree-broker.key")
		if err := os.WriteFile(path, raw, 0o400); err != nil {
			t.Fatal(err)
		}
		_, err := LoadKeyFile(path)
		if err == nil {
			t.Fatalf("%d-byte key accepted", size)
		}
		if os.Geteuid() == 0 {
			want := "32 bytes"
			if size > 32 {
				want = "oversized"
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%d-byte key refusal: %v", size, err)
			}
		}
	}
}

// TestLoadKeyFileStableAcrossLoads proves repeated loads of the installed
// root-owned key yield identical material — the single-descriptor open,
// fstat, and bounded drain leave no partial-read window.
func TestLoadKeyFileStableAcrossLoads(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires euid 0 to own a key as root")
	}
	path := newKeyFile(t, 0o400)
	first, err := LoadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("key material is not stable across loads")
	}
}
