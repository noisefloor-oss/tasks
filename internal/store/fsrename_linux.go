//go:build linux

package store

import (
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace atomically renames oldPath to newPath, failing with
// os.ErrExist if newPath already exists at the moment of the rename. This is
// the §8.5 no-overwrite publication primitive: there is no check-then-rename
// window in which an attacker-created artifact could be replaced.
func renameNoReplace(oldPath, newPath string) error {
	err := unix.Renameat2(unix.AT_FDCWD, oldPath, unix.AT_FDCWD, newPath, unix.RENAME_NOREPLACE)
	if err != nil {
		return &os.LinkError{Op: "renameat2", Old: oldPath, New: newPath, Err: err}
	}
	return nil
}
