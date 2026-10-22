//go:build unix

package store

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func lockTestStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tasks.sqlite")
	d, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// Same process: the exclusive lock refuses while a normal DB is open, and
// becomes available once Close releases the shared lock.
func TestExclusiveRefusesWhileNormalDBOpen(t *testing.T) {
	path := lockTestStore(t)
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockExclusive(filepath.Dir(path)); err == nil {
		t.Fatal("exclusive lock acquired while a normal DB is open")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	release, err := LockExclusive(filepath.Dir(path))
	if err != nil {
		t.Fatalf("exclusive lock after Close: %v", err)
	}
	release()
}

// Same process: normal opens refuse while the exclusive lock is held, and
// multiple shared holders coexist.
func TestNormalOpenRefusesWhileExclusiveHeld(t *testing.T) {
	path := lockTestStore(t)
	release, err := LockExclusive(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("normal open succeeded while exclusive lock held")
	}
	if _, err := Create(filepath.Join(filepath.Dir(path), "other.sqlite")); err == nil {
		t.Fatal("create succeeded while exclusive lock held")
	}
	release()
	a, err := Open(path)
	if err != nil {
		t.Fatalf("open after release: %v", err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatalf("second shared open: %v", err)
	}
	b.Close()
}

// TestLockChildProcess is the re-exec helper: when invoked by
// TestRealProcessExclusiveBlocksOpen it holds the exclusive lock, signals
// readiness on stdout, and releases on stdin EOF.
func TestLockChildProcess(t *testing.T) {
	if os.Getenv("NOISE_TASKS_LOCK_CHILD") != "hold-exclusive" {
		t.Skip("re-exec helper")
	}
	release, err := LockExclusive(os.Getenv("NOISE_TASKS_LOCK_STORE_DIR"))
	if err != nil {
		t.Fatalf("child exclusive lock: %v", err)
	}
	// Signal readiness and hold until the parent closes the pipe.
	os.Stdout.WriteString("ready\n")
	buf := make([]byte, 1)
	os.Stdin.Read(buf)
	release()
}

// Real process: a child process holding the exclusive lock blocks this
// process's normal open until the child exits (closing releases).
func TestRealProcessExclusiveBlocksOpen(t *testing.T) {
	path := lockTestStore(t)
	dir := filepath.Dir(path)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=TestLockChildProcess", "-test.v=false")
	cmd.Env = append(os.Environ(),
		"NOISE_TASKS_LOCK_CHILD=hold-exclusive",
		"NOISE_TASKS_LOCK_STORE_DIR="+dir,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	ready, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || ready != "ready\n" {
		t.Fatalf("child not ready: %q %v", ready, err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("normal open succeeded while another process held the exclusive lock")
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child exit: %v", err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open after child released: %v", err)
	}
	db.Close()
}
