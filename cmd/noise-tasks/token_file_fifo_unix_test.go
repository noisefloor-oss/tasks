//go:build unix

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestTokenFileFIFORefused proves a named pipe with no writer is
// refused promptly: O_NONBLOCK lets the read-only open succeed and the
// descriptor's regular-file check reject it, rather than blocking.
func TestTokenFileFIFORefused(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "token file fifo")
	tokenFile := claimToFile(t, "tokenfile.fifo")
	_ = tokenFile

	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	code, stdout, stderr := runCmd(cmdRelease, "--json", "--owner", "file-probe",
		"--token-file", fifo, "1")
	if code != 2 || stdout != "" || stderr == "" {
		t.Fatalf("release --token-file fifo = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := os.Stat(fifo); err != nil {
		t.Fatalf("fifo disappeared: %v", err)
	}
}
