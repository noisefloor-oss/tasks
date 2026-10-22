//go:build linux || darwin

package preferences

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLoadRefusesFIFOWithoutBlocking(t *testing.T) {
	boundary := t.TempDir()
	if err := os.Mkdir(filepath.Join(boundary, stateDirectory), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(Path(boundary), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Load(boundary)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular") {
			t.Fatalf("FIFO error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Load blocked opening FIFO")
	}
}

func TestPublishForcesModeDespiteUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	boundary := t.TempDir()
	if err := Publish(boundary, validPreferences()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(Path(boundary))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %o, want 0644", info.Mode().Perm())
	}
}
