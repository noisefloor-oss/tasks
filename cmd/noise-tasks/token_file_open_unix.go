//go:build unix

package main

import (
	"os"
	"syscall"
)

// openTokenFile opens a fencing-token file without following a final
// symlink (O_NOFOLLOW) and without blocking on special files
// (O_NONBLOCK, so opening a named pipe with no writer succeeds
// non-blocking and the descriptor's regular-file check then rejects
// it), so the descriptor the caller stats and reads is the descriptor
// that was validated: the §4 guarantees cannot be bypassed by
// replacing the pathname.
func openTokenFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
