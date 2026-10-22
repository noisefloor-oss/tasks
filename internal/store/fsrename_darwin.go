//go:build darwin

package store

import (
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace uses Darwin's atomic RENAME_EXCL operation so backup
// publication has the same no-overwrite guarantee as Linux RENAME_NOREPLACE.
func renameNoReplace(oldPath, newPath string) error {
	err := unix.RenamexNp(oldPath, newPath, unix.RENAME_EXCL)
	if err != nil {
		return &os.LinkError{Op: "renamex_np", Old: oldPath, New: newPath, Err: err}
	}
	return nil
}
