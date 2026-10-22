//go:build unix

package store

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// MaintenanceLockFile coordinates normal writers (shared) with offline
// restore and migration (exclusive) inside the §8.1 store directory.
const MaintenanceLockFile = "maintenance.lock"

// LockExclusive acquires the store directory's exclusive maintenance lock
// (SPEC §8.2/§8.5: offline restore and migration apply hold the exclusive
// lock; a cooperating process already holding it refuses the offline
// operation). Non-blocking: a contended lock is an error, never a wait. The
// returned release function drops the lock and closes the handle.
func LockExclusive(storeDir string) (release func(), err error) {
	path := filepath.Join(storeDir, MaintenanceLockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open maintenance lock %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("acquire exclusive maintenance lock: another noise-tasks process holds it; stop all writers and retry (%v)", err)
	}
	return func() {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

// LockShared acquires the store directory's shared maintenance lock held by
// every normal open: SQLite is only touched while this lock is held, and an
// exclusive holder (offline restore/migration) therefore never overlaps a
// normal open. Non-blocking: contention fails closed. The returned release
// function drops the lock and closes the handle.
func LockShared(storeDir string) (release func(), err error) {
	path := filepath.Join(storeDir, MaintenanceLockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open maintenance lock %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("acquire shared maintenance lock: offline maintenance is in progress; retry when it completes (%v)", err)
	}
	return func() {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}
