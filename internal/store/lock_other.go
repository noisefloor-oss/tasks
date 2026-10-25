//go:build !unix

package store

import (
	"errors"
	"path/filepath"
)

// MaintenanceLockFile coordinates normal writers (shared) with offline
// restore and migration (exclusive) inside the §8.1 store directory.
const MaintenanceLockFile = "maintenance.lock"

// LockExclusive is unsupported off unix platforms; the offline operation
// fails closed rather than running without the §8.5 exclusive lock.
func LockExclusive(storeDir string) (func(), error) {
	return nil, errors.New("exclusive maintenance lock is not supported on this platform: " + filepath.Join(storeDir, MaintenanceLockFile))
}

// LockShared is a no-op off unix platforms: LockExclusive is unsupported
// there too, so no exclusive holder can exist to exclude.
func LockShared(storeDir string) (func(), error) {
	return func() {}, nil
}

func lockCreators(string) (func(), error) {
	return func() {}, nil
}
