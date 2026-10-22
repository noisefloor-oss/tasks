//go:build unix

package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// readFd reads from an inherited descriptor without touching any path.
func readFd(fd int, buf []byte) (int, error) {
	for {
		n, err := unix.Read(fd, buf)
		if err == syscall.EINTR {
			continue
		}
		return n, err
	}
}

// closeFd closes one descriptor, ignoring the state it finds: the
// capability channel must never survive into a leaf exec.
func closeFd(fd int) { unix.Close(fd) }

func umask022() { syscall.Umask(0o022) }
